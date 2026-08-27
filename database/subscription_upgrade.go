package database

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrSubscriptionUpgradeIdempotencyConflict = errors.New("subscription upgrade idempotency key already exists")

// SubscriptionUpgradeOperation is an append-oriented payment journal. It must
// contain no OAuth credentials, cookies, payment method IDs, or card data.
type SubscriptionUpgradeOperation struct {
	ID                 string    `json:"operation_id"`
	AccountID          int64     `json:"account_id"`
	IdempotencyKeyHash string    `json:"-"`
	SourcePlan         string    `json:"source_plan"`
	TargetPlan         string    `json:"target_plan"`
	Currency           string    `json:"currency"`
	AmountDueMinor     int64     `json:"amount_due_minor"`
	Status             string    `json:"status"`
	SubmitHTTPStatus   int       `json:"submit_http_status,omitempty"`
	Detail             string    `json:"detail,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func (db *DB) ensureSubscriptionUpgradeSchema(ctx context.Context) error {
	ddl := `CREATE TABLE IF NOT EXISTS subscription_upgrade_operations (
		id TEXT PRIMARY KEY,
		account_id BIGINT NOT NULL,
		idempotency_key_hash TEXT NOT NULL,
		source_plan TEXT NOT NULL,
		target_plan TEXT NOT NULL,
		currency VARCHAR(16) NOT NULL,
		amount_due_minor BIGINT NOT NULL,
		status VARCHAR(64) NOT NULL,
		submit_http_status INT NOT NULL DEFAULT 0,
		detail TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(account_id, idempotency_key_hash)
	)`
	if db.isSQLite() {
		ddl = `CREATE TABLE IF NOT EXISTS subscription_upgrade_operations (
			id TEXT PRIMARY KEY,
			account_id INTEGER NOT NULL,
			idempotency_key_hash TEXT NOT NULL,
			source_plan TEXT NOT NULL,
			target_plan TEXT NOT NULL,
			currency TEXT NOT NULL,
			amount_due_minor INTEGER NOT NULL,
			status TEXT NOT NULL,
			submit_http_status INTEGER NOT NULL DEFAULT 0,
			detail TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(account_id, idempotency_key_hash)
		)`
	}
	if _, err := db.conn.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("create subscription upgrade operation table: %w", err)
	}
	return nil
}

func (db *DB) CreateSubscriptionUpgradeOperation(ctx context.Context, operation SubscriptionUpgradeOperation) error {
	query := `INSERT INTO subscription_upgrade_operations (
		id, account_id, idempotency_key_hash, source_plan, target_plan,
		currency, amount_due_minor, status, submit_http_status, detail
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
	ON CONFLICT (account_id, idempotency_key_hash) DO NOTHING`
	return db.withSQLiteWriteLock(ctx, func() error {
		result, err := db.conn.ExecContext(ctx, query,
			operation.ID, operation.AccountID, operation.IdempotencyKeyHash,
			operation.SourcePlan, operation.TargetPlan, operation.Currency,
			operation.AmountDueMinor, operation.Status, operation.SubmitHTTPStatus,
			operation.Detail,
		)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			return ErrSubscriptionUpgradeIdempotencyConflict
		}
		return nil
	})
}

func (db *DB) UpdateSubscriptionUpgradeOperation(ctx context.Context, id, status, detail string, submitHTTPStatus int) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		result, err := db.conn.ExecContext(ctx, `UPDATE subscription_upgrade_operations
			SET status=$2, detail=$3, submit_http_status=$4, updated_at=CURRENT_TIMESTAMP
			WHERE id=$1`, id, status, detail, submitHTTPStatus)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			return errors.New("subscription upgrade operation not found")
		}
		return nil
	})
}

func (db *DB) GetSubscriptionUpgradeOperation(ctx context.Context, id string) (*SubscriptionUpgradeOperation, error) {
	return db.querySubscriptionUpgradeOperation(ctx, `WHERE id=$1`, id)
}

func (db *DB) GetSubscriptionUpgradeOperationByIdempotencyHash(ctx context.Context, accountID int64, keyHash string) (*SubscriptionUpgradeOperation, error) {
	return db.querySubscriptionUpgradeOperation(ctx, `WHERE account_id=$1 AND idempotency_key_hash=$2`, accountID, keyHash)
}

func (db *DB) querySubscriptionUpgradeOperation(ctx context.Context, where string, args ...any) (*SubscriptionUpgradeOperation, error) {
	query := `SELECT id, account_id, idempotency_key_hash, source_plan, target_plan,
		currency, amount_due_minor, status, submit_http_status, detail, created_at, updated_at
		FROM subscription_upgrade_operations ` + where + ` LIMIT 1`
	var operation SubscriptionUpgradeOperation
	var createdRaw, updatedRaw any
	err := db.conn.QueryRowContext(ctx, query, args...).Scan(
		&operation.ID, &operation.AccountID, &operation.IdempotencyKeyHash,
		&operation.SourcePlan, &operation.TargetPlan, &operation.Currency,
		&operation.AmountDueMinor, &operation.Status, &operation.SubmitHTTPStatus,
		&operation.Detail, &createdRaw, &updatedRaw,
	)
	if err != nil {
		return nil, err
	}
	operation.CreatedAt, err = parseDBTimeValue(createdRaw)
	if err != nil {
		return nil, err
	}
	operation.UpdatedAt, err = parseDBTimeValue(updatedRaw)
	if err != nil {
		return nil, err
	}
	return &operation, nil
}
