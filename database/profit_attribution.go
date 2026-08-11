package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	ProfitConsumerSourceAPIKey        = "api_key"
	ProfitConsumerSourceConfigKey     = "config_key"
	ProfitConsumerSourceSystem        = "system_internal"
	ProfitNonSettleableSystemInternal = "system_internal"
)

type ProfitConsumerAttribution struct {
	SourceType          string `json:"source_type"`
	SourceID            string `json:"source_id"`
	AssignmentVersionID int64  `json:"assignment_version_id"`
	GroupID             int64  `json:"group_id"`
	GroupName           string `json:"group_name"`
	AssignmentSource    string `json:"assignment_source"`
	NonSettleableReason string `json:"non_settleable_reason,omitempty"`
}

type ProfitOwnerAttribution struct {
	AccountID        int64  `json:"account_id"`
	GroupID          int64  `json:"group_id"`
	GroupName        string `json:"group_name"`
	AssignmentSource string `json:"assignment_source"`
}

type ProfitAPIKeyAssignment struct {
	APIKeyID            int64  `json:"api_key_id"`
	APIKeyName          string `json:"api_key_name"`
	AssignmentVersionID int64  `json:"assignment_version_id"`
	ConsumerGroupID     int64  `json:"consumer_group_id"`
	ConsumerGroupName   string `json:"consumer_group_name"`
	AssignmentSource    string `json:"assignment_source"`
	Pending             bool   `json:"pending"`
	SuggestedGroupID    int64  `json:"suggested_group_id,omitempty"`
	SuggestedGroupName  string `json:"suggested_group_name,omitempty"`
	UpdatedAt           string `json:"updated_at,omitempty"`
}

type ProfitAPIKeyAssignmentUpdate struct {
	ConsumerGroupID int64
	ApplyHistory    bool
	Actor           string
	Reason          string
	Source          string
}

type profitAttributionSnapshot struct {
	apiKeys  map[int64]ProfitConsumerAttribution
	accounts map[int64]ProfitOwnerAttribution
}

func emptyProfitAttributionSnapshot() *profitAttributionSnapshot {
	return &profitAttributionSnapshot{
		apiKeys:  make(map[int64]ProfitConsumerAttribution),
		accounts: make(map[int64]ProfitOwnerAttribution),
	}
}

func (db *DB) ProfitAPIKeyAttribution(apiKeyID int64) ProfitConsumerAttribution {
	if apiKeyID <= 0 {
		return ProfitConsumerAttribution{}
	}
	snapshot := db.profitAttribution.Load()
	if snapshot == nil {
		return ProfitConsumerAttribution{SourceType: ProfitConsumerSourceAPIKey, SourceID: strconv.FormatInt(apiKeyID, 10), AssignmentSource: "pending"}
	}
	if item, ok := snapshot.apiKeys[apiKeyID]; ok {
		return item
	}
	return ProfitConsumerAttribution{SourceType: ProfitConsumerSourceAPIKey, SourceID: strconv.FormatInt(apiKeyID, 10), AssignmentSource: "pending"}
}

func (db *DB) ProfitAccountAttribution(accountID int64) ProfitOwnerAttribution {
	if accountID <= 0 {
		return ProfitOwnerAttribution{}
	}
	snapshot := db.profitAttribution.Load()
	if snapshot == nil {
		return ProfitOwnerAttribution{AccountID: accountID, AssignmentSource: "pending"}
	}
	if item, ok := snapshot.accounts[accountID]; ok {
		return item
	}
	return ProfitOwnerAttribution{AccountID: accountID, AssignmentSource: "pending"}
}

func (db *DB) reloadProfitAttributionSnapshot(ctx context.Context) error {
	if db == nil || db.conn == nil {
		return nil
	}
	next := emptyProfitAttributionSnapshot()
	rows, err := db.conn.QueryContext(ctx, `SELECT c.api_key_id, v.id, v.consumer_group_id,
		COALESCE(v.consumer_group_name_snapshot, ''), COALESCE(NULLIF(v.assignment_source, ''), 'manual')
		FROM profit_api_key_current_assignments c
		JOIN profit_api_key_assignment_versions v ON v.id = c.assignment_version_id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var apiKeyID int64
		var item ProfitConsumerAttribution
		if err := rows.Scan(&apiKeyID, &item.AssignmentVersionID, &item.GroupID, &item.GroupName, &item.AssignmentSource); err != nil {
			rows.Close()
			return err
		}
		item.SourceType = ProfitConsumerSourceAPIKey
		item.SourceID = strconv.FormatInt(apiKeyID, 10)
		next.apiKeys[apiKeyID] = item
	}
	if err := rows.Close(); err != nil {
		return err
	}

	rows, err = db.conn.QueryContext(ctx, `SELECT pas.account_id, pas.settlement_group_id,
		COALESCE(NULLIF(g.name, ''), pas.settlement_group_name, ''), COALESCE(NULLIF(pas.assignment_source, ''), 'confirmed')
		FROM profit_account_settings pas LEFT JOIN account_groups g ON g.id = pas.settlement_group_id
		UNION ALL
		SELECT m.account_id, MIN(m.group_id), MAX(g.name), 'inherited'
		FROM account_group_members m JOIN account_groups g ON g.id = m.group_id
		LEFT JOIN profit_account_settings pas ON pas.account_id = m.account_id
		WHERE pas.account_id IS NULL GROUP BY m.account_id HAVING COUNT(*) = 1`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var item ProfitOwnerAttribution
		if err := rows.Scan(&item.AccountID, &item.GroupID, &item.GroupName, &item.AssignmentSource); err != nil {
			rows.Close()
			return err
		}
		next.accounts[item.AccountID] = item
	}
	if err := rows.Close(); err != nil {
		return err
	}
	db.profitAttribution.Store(next)
	return nil
}

func (db *DB) ListProfitAPIKeyAssignments(ctx context.Context) ([]ProfitAPIKeyAssignment, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT k.id, COALESCE(k.name, ''), COALESCE(v.id, 0),
		COALESCE(v.consumer_group_id, 0), COALESCE(NULLIF(g.name, ''), v.consumer_group_name_snapshot, ''),
		COALESCE(v.assignment_source, 'pending'), v.updated_at
		FROM api_keys k
		LEFT JOIN profit_api_key_current_assignments c ON c.api_key_id = k.id
		LEFT JOIN profit_api_key_assignment_versions v ON v.id = c.assignment_version_id
		LEFT JOIN account_groups g ON g.id = v.consumer_group_id
		ORDER BY LOWER(COALESCE(k.name, '')), k.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ProfitAPIKeyAssignment, 0)
	for rows.Next() {
		var item ProfitAPIKeyAssignment
		var updatedRaw interface{}
		if err := rows.Scan(&item.APIKeyID, &item.APIKeyName, &item.AssignmentVersionID,
			&item.ConsumerGroupID, &item.ConsumerGroupName, &item.AssignmentSource, &updatedRaw); err != nil {
			return nil, err
		}
		item.Pending = item.AssignmentVersionID <= 0 || item.ConsumerGroupID <= 0
		if updated, parseErr := parseDBTimeValue(updatedRaw); parseErr == nil {
			item.UpdatedAt = updated.Format(time.RFC3339)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	groups, err := db.ListAccountGroups(ctx)
	if err != nil {
		return nil, err
	}
	for i := range items {
		if !items[i].Pending {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(items[i].APIKeyName))
		for _, group := range groups {
			groupName := strings.ToLower(strings.TrimSpace(group.Name))
			if groupName != "" && strings.Contains(name, groupName) {
				items[i].SuggestedGroupID = group.ID
				items[i].SuggestedGroupName = group.Name
				break
			}
		}
	}
	return items, nil
}

func (db *DB) AssignProfitAPIKeyConsumerGroup(ctx context.Context, apiKeyID int64, update ProfitAPIKeyAssignmentUpdate) (ProfitAPIKeyAssignment, error) {
	if apiKeyID <= 0 || update.ConsumerGroupID <= 0 {
		return ProfitAPIKeyAssignment{}, fmt.Errorf("api key and consumer group are required")
	}
	assignmentSource := strings.TrimSpace(update.Source)
	if assignmentSource == "" {
		assignmentSource = "manual"
	}
	actor := strings.TrimSpace(update.Actor)
	reason := strings.TrimSpace(update.Reason)
	var versionID int64
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		var keyName, groupName string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(name, '') FROM api_keys WHERE id = $1`, apiKeyID).Scan(&keyName); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(name, '') FROM account_groups WHERE id = $1`, update.ConsumerGroupID).Scan(&groupName); err != nil {
			return err
		}
		if update.ApplyHistory {
			var settled int64
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM profit_settlement_items WHERE api_key_id = $1`, apiKeyID).Scan(&settled); err != nil {
				return err
			}
			if settled > 0 {
				return ErrProfitSettlementConflict
			}
		}
		if db.isSQLite() {
			res, err := tx.ExecContext(ctx, `INSERT INTO profit_api_key_assignment_versions
				(api_key_id, consumer_group_id, consumer_group_name_snapshot, assignment_source, actor, reason)
				VALUES ($1,$2,$3,$4,$5,$6)`, apiKeyID, update.ConsumerGroupID, groupName, assignmentSource, actor, reason)
			if err != nil {
				return err
			}
			versionID, err = res.LastInsertId()
			if err != nil {
				return err
			}
		} else {
			if err := tx.QueryRowContext(ctx, `INSERT INTO profit_api_key_assignment_versions
				(api_key_id, consumer_group_id, consumer_group_name_snapshot, assignment_source, actor, reason)
				VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`, apiKeyID, update.ConsumerGroupID, groupName,
				assignmentSource, actor, reason).Scan(&versionID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO profit_api_key_current_assignments (api_key_id, assignment_version_id, updated_at)
			VALUES ($1,$2,CURRENT_TIMESTAMP) ON CONFLICT(api_key_id) DO UPDATE SET
			assignment_version_id = excluded.assignment_version_id, updated_at = CURRENT_TIMESTAMP`, apiKeyID, versionID); err != nil {
			return err
		}
		if update.ApplyHistory {
			var highWater int64
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM usage_logs WHERE api_key_id = $1`, apiKeyID).Scan(&highWater); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM profit_api_key_assignment_overrides WHERE api_key_id = $1`, apiKeyID); err != nil {
				return err
			}
			if highWater > 0 {
				if _, err := tx.ExecContext(ctx, `INSERT INTO profit_api_key_assignment_overrides
					(api_key_id, from_log_id, to_log_id, assignment_version_id, actor, reason)
					VALUES ($1,0,$2,$3,$4,$5)`, apiKeyID, highWater+1, versionID, actor, reason); err != nil {
					return err
				}
			}
		}
		_ = keyName
		return nil
	})
	if err != nil {
		return ProfitAPIKeyAssignment{}, err
	}
	if err := db.reloadProfitAttributionSnapshot(ctx); err != nil {
		return ProfitAPIKeyAssignment{}, err
	}
	items, err := db.ListProfitAPIKeyAssignments(ctx)
	if err != nil {
		return ProfitAPIKeyAssignment{}, err
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].APIKeyID < items[j].APIKeyID })
	for _, item := range items {
		if item.APIKeyID == apiKeyID && item.AssignmentVersionID == versionID {
			return item, nil
		}
	}
	return ProfitAPIKeyAssignment{}, errors.New("profit api key assignment was committed but could not be reloaded")
}
