package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type ProfitPairRateSetting struct {
	ID                int64  `json:"id"`
	ConsumerGroupID   int64  `json:"consumer_group_id"`
	ConsumerGroupName string `json:"consumer_group_name"`
	OwnerGroupID      int64  `json:"owner_group_id"`
	OwnerGroupName    string `json:"owner_group_name"`
	RatePPM           int64  `json:"rate_ppm"`
	EffectiveDate     string `json:"effective_date"`
	RevisionNo        int    `json:"revision_no"`
	Source            string `json:"source"`
}

type ProfitAccountEconomicSetting struct {
	ID                        int64  `json:"id"`
	AccountID                 int64  `json:"account_id"`
	AccountName               string `json:"account_name"`
	AccountDeleted            bool   `json:"account_deleted"`
	EffectiveMonth            string `json:"effective_month"`
	RevisionNo                int    `json:"revision_no"`
	MonthlyFixedCostUSDMicros int64  `json:"monthly_fixed_cost_usd_micros"`
	MonthlyCapacityUSDMicros  int64  `json:"monthly_capacity_usd_micros"`
	Source                    string `json:"source"`
	Frozen                    bool   `json:"frozen"`
}

type ProfitAccountROI struct {
	AccountID         int64  `json:"account_id"`
	AccountName       string `json:"account_name"`
	AccountDeleted    bool   `json:"account_deleted"`
	OwnerGroupID      int64  `json:"owner_group_id"`
	OwnerGroupName    string `json:"owner_group_name"`
	EffectiveMonth    string `json:"effective_month"`
	EconomicVersionID int64  `json:"economic_version_id"`
	ProfitAccountCostAllocation
}

type profitRateVersion struct {
	ID            int64
	ConsumerID    int64
	OwnerID       int64
	RatePPM       int64
	EffectiveDate string
	Source        string
}

type profitRateResolver struct {
	pairs   map[[2]int64][]profitRateVersion
	owners  map[int64][]profitRateVersion
	systems []profitRateVersion
}

func (r *profitRateResolver) resolve(date string, consumerID, ownerID int64) profitRateVersion {
	if consumerID == ownerID && consumerID > 0 {
		return profitRateVersion{ConsumerID: consumerID, OwnerID: ownerID, RatePPM: 0, EffectiveDate: date, Source: "same_group"}
	}
	if versions := r.pairs[[2]int64{consumerID, ownerID}]; len(versions) > 0 {
		if found, ok := latestProfitRate(versions, date); ok {
			return found
		}
	}
	if versions := r.owners[ownerID]; len(versions) > 0 {
		if found, ok := latestProfitRate(versions, date); ok {
			found.ConsumerID = consumerID
			return found
		}
	}
	if found, ok := latestProfitRate(r.systems, date); ok {
		found.ConsumerID = consumerID
		found.OwnerID = ownerID
		return found
	}
	return profitRateVersion{ConsumerID: consumerID, OwnerID: ownerID, RatePPM: DefaultProfitPairRatePPM, EffectiveDate: "1970-01-01", Source: "system_default"}
}

func latestProfitRate(versions []profitRateVersion, date string) (profitRateVersion, bool) {
	for _, version := range versions {
		if version.EffectiveDate <= date {
			return version, true
		}
	}
	return profitRateVersion{}, false
}

func (db *DB) loadProfitRateResolver(ctx context.Context) (*profitRateResolver, error) {
	resolver := &profitRateResolver{pairs: make(map[[2]int64][]profitRateVersion), owners: make(map[int64][]profitRateVersion)}
	rows, err := db.conn.QueryContext(ctx, `SELECT id, consumer_group_id, owner_group_id, rate_ppm, CAST(effective_date AS TEXT)
		FROM profit_pair_rate_versions WHERE active = $1 ORDER BY effective_date DESC, revision_no DESC, id DESC`, true)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var item profitRateVersion
		if err := rows.Scan(&item.ID, &item.ConsumerID, &item.OwnerID, &item.RatePPM, &item.EffectiveDate); err != nil {
			rows.Close()
			return nil, err
		}
		item.Source = "pair"
		key := [2]int64{item.ConsumerID, item.OwnerID}
		resolver.pairs[key] = append(resolver.pairs[key], item)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = db.conn.QueryContext(ctx, `SELECT id, owner_group_id, rate_ppm, CAST(effective_date AS TEXT)
		FROM profit_owner_default_rate_versions WHERE active = $1 ORDER BY effective_date DESC, revision_no DESC, id DESC`, true)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var item profitRateVersion
		if err := rows.Scan(&item.ID, &item.OwnerID, &item.RatePPM, &item.EffectiveDate); err != nil {
			rows.Close()
			return nil, err
		}
		item.Source = "owner_default"
		resolver.owners[item.OwnerID] = append(resolver.owners[item.OwnerID], item)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = db.conn.QueryContext(ctx, `SELECT id, rate_ppm, CAST(effective_date AS TEXT)
		FROM profit_system_default_rate_versions WHERE active = $1 ORDER BY effective_date DESC, revision_no DESC, id DESC`, true)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var item profitRateVersion
		if err := rows.Scan(&item.ID, &item.RatePPM, &item.EffectiveDate); err != nil {
			rows.Close()
			return nil, err
		}
		item.Source = "system_default"
		resolver.systems = append(resolver.systems, item)
	}
	return resolver, rows.Close()
}

func (db *DB) ListProfitPairRates(ctx context.Context) ([]ProfitPairRateSetting, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT v.id, v.consumer_group_id,
		COALESCE(NULLIF(c.name, ''), v.consumer_group_name_snapshot, ''), v.owner_group_id,
		COALESCE(NULLIF(o.name, ''), v.owner_group_name_snapshot, ''), v.rate_ppm,
		CAST(v.effective_date AS TEXT), v.revision_no
		FROM profit_pair_rate_versions v LEFT JOIN account_groups c ON c.id = v.consumer_group_id
		LEFT JOIN account_groups o ON o.id = v.owner_group_id WHERE v.active = $1
		ORDER BY v.consumer_group_id, v.owner_group_id, v.effective_date DESC`, true)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ProfitPairRateSetting, 0)
	for rows.Next() {
		var item ProfitPairRateSetting
		if err := rows.Scan(&item.ID, &item.ConsumerGroupID, &item.ConsumerGroupName, &item.OwnerGroupID,
			&item.OwnerGroupName, &item.RatePPM, &item.EffectiveDate, &item.RevisionNo); err != nil {
			return nil, err
		}
		item.Source = "pair"
		items = append(items, item)
	}
	return items, rows.Err()
}

func validateProfitEffectiveDate(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = time.Now().In(profitLocation()).Format("2006-01-02")
	}
	if _, err := time.ParseInLocation("2006-01-02", value, profitLocation()); err != nil {
		return "", fmt.Errorf("invalid effective date: %w", err)
	}
	return value, nil
}

func (db *DB) UpdateProfitPairRate(ctx context.Context, consumerID, ownerID, ratePPM int64, effectiveDate, actor, reason string) (ProfitPairRateSetting, error) {
	if consumerID <= 0 || ownerID <= 0 || consumerID == ownerID || ratePPM < 0 || ratePPM > 1_000_000_000 {
		return ProfitPairRateSetting{}, fmt.Errorf("invalid pair rate")
	}
	date, err := validateProfitEffectiveDate(effectiveDate)
	if err != nil {
		return ProfitPairRateSetting{}, err
	}
	var result ProfitPairRateSetting
	err = db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(name, '') FROM account_groups WHERE id=$1`, consumerID).Scan(&result.ConsumerGroupName); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(name, '') FROM account_groups WHERE id=$1`, ownerID).Scan(&result.OwnerGroupName); err != nil {
			return err
		}
		var revision int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision_no),0)+1 FROM profit_pair_rate_versions
			WHERE consumer_group_id=$1 AND owner_group_id=$2 AND effective_date=$3`, consumerID, ownerID, date).Scan(&revision); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE profit_pair_rate_versions SET active=$1
			WHERE consumer_group_id=$2 AND owner_group_id=$3 AND effective_date=$4 AND active=$5`, false, consumerID, ownerID, date, true); err != nil {
			return err
		}
		result = ProfitPairRateSetting{ConsumerGroupID: consumerID, OwnerGroupID: ownerID, RatePPM: ratePPM,
			EffectiveDate: date, RevisionNo: revision, Source: "pair", ConsumerGroupName: result.ConsumerGroupName,
			OwnerGroupName: result.OwnerGroupName}
		if db.isSQLite() {
			res, err := tx.ExecContext(ctx, `INSERT INTO profit_pair_rate_versions
				(consumer_group_id, owner_group_id, consumer_group_name_snapshot, owner_group_name_snapshot,
				rate_ppm, effective_date, revision_no, active, actor, reason)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, consumerID, ownerID, result.ConsumerGroupName,
				result.OwnerGroupName, ratePPM, date, revision, true, strings.TrimSpace(actor), strings.TrimSpace(reason))
			if err != nil {
				return err
			}
			result.ID, err = res.LastInsertId()
			return err
		}
		return tx.QueryRowContext(ctx, `INSERT INTO profit_pair_rate_versions
			(consumer_group_id, owner_group_id, consumer_group_name_snapshot, owner_group_name_snapshot,
			rate_ppm, effective_date, revision_no, active, actor, reason)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`, consumerID, ownerID,
			result.ConsumerGroupName, result.OwnerGroupName, ratePPM, date, revision, true,
			strings.TrimSpace(actor), strings.TrimSpace(reason)).Scan(&result.ID)
	})
	return result, err
}

func normalizeProfitMonth(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) == 7 {
		value += "-01"
	}
	if value == "" {
		now := time.Now().In(profitLocation())
		value = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, profitLocation()).Format("2006-01-02")
	}
	parsed, err := time.ParseInLocation("2006-01-02", value, profitLocation())
	if err != nil || parsed.Day() != 1 {
		return "", fmt.Errorf("effective_month must be the first day of a month")
	}
	return parsed.Format("2006-01-02"), nil
}

func (db *DB) resolveProfitAccountEconomic(ctx context.Context, accountID int64, month string) (ProfitAccountEconomicSetting, error) {
	result := ProfitAccountEconomicSetting{AccountID: accountID, EffectiveMonth: month,
		MonthlyFixedCostUSDMicros: DefaultProfitAccountCostMicros,
		MonthlyCapacityUSDMicros:  DefaultProfitAccountCapacityMicros, Source: "default"}
	var deletedInt int
	err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(name,''), CASE WHEN status='deleted' OR deleted_at IS NOT NULL THEN 1 ELSE 0 END
		FROM accounts WHERE id=$1`, accountID).Scan(&result.AccountName, &deletedInt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	result.AccountDeleted = deletedInt != 0
	var source string
	err = db.conn.QueryRowContext(ctx, `SELECT id, CAST(effective_month AS TEXT), revision_no,
		monthly_fixed_cost_usd_micros, monthly_capacity_usd_micros, source
		FROM profit_account_economic_versions WHERE account_id=$1 AND effective_month <= $2 AND active=$3
		ORDER BY effective_month DESC, revision_no DESC, id DESC LIMIT 1`, accountID, month, true).
		Scan(&result.ID, &result.EffectiveMonth, &result.RevisionNo, &result.MonthlyFixedCostUSDMicros,
			&result.MonthlyCapacityUSDMicros, &source)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.Source = source
	var confirmed int64
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM profit_account_cost_allocations
		WHERE account_id=$1 AND effective_month=$2 AND active=$3`, accountID, month, true).Scan(&confirmed); err != nil {
		return result, err
	}
	result.Frozen = confirmed > 0
	return result, nil
}

func (db *DB) ListProfitAccountEconomics(ctx context.Context, month string) ([]ProfitAccountEconomicSetting, error) {
	month, err := normalizeProfitMonth(month)
	if err != nil {
		return nil, err
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT id FROM accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	items := make([]ProfitAccountEconomicSetting, 0, len(ids))
	for _, id := range ids {
		item, err := db.resolveProfitAccountEconomic(ctx, id, month)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(items[i].AccountName) < strings.ToLower(items[j].AccountName)
	})
	return items, nil
}

func (db *DB) UpdateProfitAccountEconomic(ctx context.Context, accountID int64, effectiveMonth string, monthlyCost, monthlyCapacity int64, actor, reason string) (ProfitAccountEconomicSetting, error) {
	if accountID <= 0 || monthlyCost < 0 || monthlyCapacity <= 0 || monthlyCost > 1_000_000_000*ProfitScalePPM || monthlyCapacity > 1_000_000_000*ProfitScalePPM {
		return ProfitAccountEconomicSetting{}, fmt.Errorf("invalid account economics")
	}
	month, err := normalizeProfitMonth(effectiveMonth)
	if err != nil {
		return ProfitAccountEconomicSetting{}, err
	}
	var id int64
	err = db.withWriteTx(ctx, func(tx *sql.Tx) error {
		var exists int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE id=$1`, accountID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return sql.ErrNoRows
		}
		var frozen int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM profit_account_cost_allocations
			WHERE account_id=$1 AND effective_month=$2 AND active=$3`, accountID, month, true).Scan(&frozen); err != nil {
			return err
		}
		if frozen > 0 {
			return fmt.Errorf("account month is frozen; create a full-month revision")
		}
		var revision int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision_no),0)+1 FROM profit_account_economic_versions
			WHERE account_id=$1 AND effective_month=$2`, accountID, month).Scan(&revision); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE profit_account_economic_versions SET active=$1
			WHERE account_id=$2 AND effective_month=$3 AND active=$4`, false, accountID, month, true); err != nil {
			return err
		}
		if db.isSQLite() {
			res, err := tx.ExecContext(ctx, `INSERT INTO profit_account_economic_versions
				(account_id,effective_month,revision_no,active,monthly_fixed_cost_usd_micros,
				monthly_capacity_usd_micros,source,actor,reason) VALUES ($1,$2,$3,$4,$5,$6,'manual',$7,$8)`,
				accountID, month, revision, true, monthlyCost, monthlyCapacity, strings.TrimSpace(actor), strings.TrimSpace(reason))
			if err != nil {
				return err
			}
			id, err = res.LastInsertId()
			return err
		}
		return tx.QueryRowContext(ctx, `INSERT INTO profit_account_economic_versions
			(account_id,effective_month,revision_no,active,monthly_fixed_cost_usd_micros,
			monthly_capacity_usd_micros,source,actor,reason) VALUES ($1,$2,$3,$4,$5,$6,'manual',$7,$8) RETURNING id`,
			accountID, month, revision, true, monthlyCost, monthlyCapacity, strings.TrimSpace(actor), strings.TrimSpace(reason)).Scan(&id)
	})
	if err != nil {
		return ProfitAccountEconomicSetting{}, err
	}
	result, err := db.resolveProfitAccountEconomic(ctx, accountID, month)
	if err == nil && result.ID != id {
		return result, fmt.Errorf("economic version was committed but is not active")
	}
	return result, err
}
