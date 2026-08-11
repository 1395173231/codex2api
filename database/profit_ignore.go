package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const profitAccountCleanupBatchSize = 500

var (
	ErrProfitAccountNotDeleted    = errors.New("profit account is not deleted")
	ErrProfitAccountHasSettlement = errors.New("profit account is referenced by a settlement")
)

type ProfitAccountCleanupResult struct {
	UsageLogs             int64 `json:"usage_logs"`
	ProfitLedgerRows      int64 `json:"profit_ledger_rows"`
	PromptPolicyIncidents int64 `json:"prompt_policy_incidents"`
	PromptRiskEvents      int64 `json:"prompt_risk_events"`
	AccountEvents         int64 `json:"account_events"`
	AccountScopeCounters  int64 `json:"account_scope_counters"`
}

func (db *DB) profitPendingDeletedAccount(ctx context.Context, accountID int64) (string, error) {
	if accountID <= 0 {
		return "", sql.ErrNoRows
	}
	var pendingCount int64
	var ledgerName string
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(account_name_snapshot), '')
		FROM profit_daily_ledger WHERE account_id = $1 AND settlement_group_id = 0
		AND COALESCE(claimed_lineage_id, '') = ''`, accountID).Scan(&pendingCount, &ledgerName); err != nil {
		return "", err
	}
	if pendingCount == 0 {
		return "", sql.ErrNoRows
	}

	var accountName, status, errorMessage string
	var deletedAt sql.NullTime
	err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(name, ''), COALESCE(status, ''),
		COALESCE(error_message, ''), deleted_at FROM accounts WHERE id = $1`, accountID).
		Scan(&accountName, &status, &errorMessage, &deletedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if err == nil && status != "deleted" && errorMessage != "deleted" && !deletedAt.Valid {
		return "", ErrProfitAccountNotDeleted
	}
	if accountName == "" {
		accountName = ledgerName
	}
	return accountName, nil
}

func (db *DB) IgnoreProfitPendingAccount(ctx context.Context, accountID int64) error {
	accountName, err := db.profitPendingDeletedAccount(ctx, accountID)
	if err != nil {
		return err
	}
	return db.withSQLiteWriteLock(ctx, func() error {
		_, err := db.conn.ExecContext(ctx, `INSERT INTO profit_ignored_accounts (account_id, account_name_snapshot, ignored_at)
			VALUES ($1,$2,`+db.nowExpr()+`) ON CONFLICT (account_id) DO UPDATE SET
			account_name_snapshot = excluded.account_name_snapshot, ignored_at = `+db.nowExpr(), accountID, accountName)
		return err
	})
}

func (db *DB) ensureProfitAccountCanBePurged(ctx context.Context, accountID int64) error {
	return ensureProfitAccountCanBePurgedQuery(ctx, db.conn, accountID)
}

type profitPurgeQueryer interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}

func ensureProfitAccountCanBePurgedQuery(ctx context.Context, queryer profitPurgeQueryer, accountID int64) error {
	var references int64
	if err := queryer.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM profit_daily_ledger WHERE account_id=$1)+
		(SELECT COUNT(*) FROM profit_settlement_items WHERE account_id=$1)+
		(SELECT COUNT(*) FROM profit_settlement_account_roi_items WHERE account_id=$1)+
		(SELECT COUNT(*) FROM profit_account_cost_allocations WHERE account_id=$1)+
		(SELECT COUNT(*) FROM profit_account_economic_versions WHERE account_id=$1)`, accountID).Scan(&references); err != nil {
		return err
	}
	if references > 0 {
		return ErrProfitAccountHasSettlement
	}
	return nil
}

func ensureProfitPendingAccountCanBeForcePurged(ctx context.Context, queryer profitPurgeQueryer, accountID int64) error {
	var references int64
	if err := queryer.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM profit_settlement_items WHERE account_id=$1)+
		(SELECT COUNT(*) FROM profit_daily_ledger WHERE account_id=$1 AND COALESCE(claimed_lineage_id,'')<>'')+
		(SELECT COUNT(*) FROM profit_settlement_account_roi_items WHERE account_id=$1)+
		(SELECT COUNT(*) FROM profit_account_cost_allocations WHERE account_id=$1)`, accountID).Scan(&references); err != nil {
		return err
	}
	if references > 0 {
		return ErrProfitAccountHasSettlement
	}
	return nil
}

func (db *DB) deleteProfitAccountRowsInBatches(ctx context.Context, table string, accountID int64, batchSize int) (int64, error) {
	if batchSize <= 0 {
		batchSize = profitAccountCleanupBatchSize
	}
	var total int64
	for {
		var affected int64
		err := db.withSQLiteWriteLock(ctx, func() error {
			tx, err := db.conn.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			query := fmt.Sprintf(`DELETE FROM %s WHERE id IN (
				SELECT id FROM %s WHERE account_id = $1 ORDER BY id LIMIT $2
			)`, table, table)
			result, err := tx.ExecContext(ctx, query, accountID, batchSize)
			if err != nil {
				return err
			}
			affected, err = result.RowsAffected()
			if err != nil {
				return err
			}
			return tx.Commit()
		})
		if err != nil {
			return total, err
		}
		total += affected
		if affected < int64(batchSize) {
			return total, nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return total, ctx.Err()
		case <-timer.C:
		}
	}
}

func (db *DB) deleteProfitPolicyIncidentsInBatches(ctx context.Context, accountID int64, batchSize int) (int64, error) {
	if batchSize <= 0 {
		batchSize = profitAccountCleanupBatchSize
	}
	var total int64
	for {
		var affected int64
		err := db.withSQLiteWriteLock(ctx, func() error {
			tx, err := db.conn.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(ctx, `DELETE FROM prompt_rule_candidate_evidence
				WHERE prompt_policy_incident_id IN (SELECT incident_id FROM prompt_policy_incidents
					WHERE account_id = $1 ORDER BY id LIMIT $2)`, accountID, batchSize); err != nil {
				return err
			}
			result, err := tx.ExecContext(ctx, `DELETE FROM prompt_policy_incidents WHERE id IN (
				SELECT id FROM prompt_policy_incidents WHERE account_id = $1 ORDER BY id LIMIT $2
			)`, accountID, batchSize)
			if err != nil {
				return err
			}
			affected, err = result.RowsAffected()
			if err != nil {
				return err
			}
			return tx.Commit()
		})
		if err != nil {
			return total, err
		}
		total += affected
		if affected < int64(batchSize) {
			return total, nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return total, ctx.Err()
		case <-timer.C:
		}
	}
}

func (db *DB) purgeProfitPendingAccountData(ctx context.Context, accountID int64, batchSize int) (ProfitAccountCleanupResult, error) {
	var cleanup ProfitAccountCleanupResult
	if _, err := db.profitPendingDeletedAccount(ctx, accountID); err != nil {
		return cleanup, err
	}
	if err := ensureProfitPendingAccountCanBeForcePurged(ctx, db.conn, accountID); err != nil {
		return cleanup, err
	}

	var err error
	if cleanup.UsageLogs, err = db.deleteProfitAccountRowsInBatches(ctx, "usage_logs", accountID, batchSize); err != nil {
		return cleanup, err
	}
	if cleanup.PromptPolicyIncidents, err = db.deleteProfitPolicyIncidentsInBatches(ctx, accountID, batchSize); err != nil {
		return cleanup, err
	}
	if cleanup.PromptRiskEvents, err = db.deleteProfitAccountRowsInBatches(ctx, "prompt_risk_events", accountID, batchSize); err != nil {
		return cleanup, err
	}
	if cleanup.AccountEvents, err = db.deleteProfitAccountRowsInBatches(ctx, "account_events", accountID, batchSize); err != nil {
		return cleanup, err
	}

	err = db.withProfitLedgerTx(ctx, func(tx *sql.Tx, _ int64) error {
		if err := ensureProfitPendingAccountCanBeForcePurged(ctx, tx, accountID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM api_key_scope_counters
			WHERE scope_type = 'account' AND scope_id = $1`, accountID)
		if err != nil {
			return err
		}
		cleanup.AccountScopeCounters, _ = result.RowsAffected()
		for _, query := range []string{
			`DELETE FROM account_model_cooldowns WHERE account_id = $1`,
			`DELETE FROM account_group_members WHERE account_id = $1`,
			`DELETE FROM profit_account_month_cost_state WHERE account_id = $1`,
			`DELETE FROM profit_account_economic_versions WHERE account_id = $1`,
			`DELETE FROM profit_account_settings WHERE account_id = $1`,
			`DELETE FROM profit_ignored_accounts WHERE account_id = $1`,
		} {
			_, err := tx.ExecContext(ctx, query, accountID)
			if err != nil {
				return err
			}
		}
		result, err = tx.ExecContext(ctx, `DELETE FROM profit_daily_ledger WHERE account_id = $1`, accountID)
		if err != nil {
			return err
		}
		cleanup.ProfitLedgerRows, _ = result.RowsAffected()
		result, err = tx.ExecContext(ctx, `DELETE FROM accounts WHERE id = $1
			AND (status = 'deleted' OR COALESCE(error_message, '') = 'deleted' OR deleted_at IS NOT NULL)`, accountID)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE id = $1`, accountID).Scan(&exists); err != nil {
				return err
			}
			if exists > 0 {
				return ErrProfitAccountNotDeleted
			}
		}
		return nil
	})
	return cleanup, err
}

func (db *DB) PurgeProfitPendingAccountData(ctx context.Context, accountID int64) (ProfitAccountCleanupResult, error) {
	return db.purgeProfitPendingAccountData(ctx, accountID, profitAccountCleanupBatchSize)
}
