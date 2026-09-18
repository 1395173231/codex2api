package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Tickets are separate from editable account credentials. Revision/lease fences
// prevent an old probe from overwriting a manual replacement or another worker.
type CodexTurnStateRecord struct {
	AccountID        int64     `json:"account_id"`
	Model            string    `json:"model"`
	Identity         string    `json:"identity"`
	Token            string    `json:"token"`
	IssuedAt         time.Time `json:"issued_at"`
	CapturedAt       time.Time `json:"captured_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	LastAttemptAt    time.Time `json:"last_attempt_at"`
	NextAttemptAt    time.Time `json:"next_attempt_at"`
	Attempts         int       `json:"attempts"`
	Failures         int       `json:"failures"`
	LastError        string    `json:"last_error"`
	RefreshRequested bool      `json:"refresh_requested"`
	Revision         int64     `json:"-"`
	Lease            string    `json:"-"`
	LeaseUntil       int64     `json:"-"`
}

func (db *DB) ensureCodexTurnStateSchema(ctx context.Context) error {
	for _, query := range []string{
		`CREATE TABLE IF NOT EXISTS codex_turn_state_settings (id INTEGER PRIMARY KEY, config TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS codex_turn_states (
			account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			model TEXT NOT NULL, payload TEXT NOT NULL DEFAULT '{}', revision BIGINT NOT NULL DEFAULT 0,
			token_hash TEXT NOT NULL DEFAULT '',
			lease_id TEXT NOT NULL DEFAULT '', lease_until BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY(account_id, model))`,
	} {
		if _, err := db.conn.ExecContext(ctx, query); err != nil {
			return err
		}
	}
	// Early development builds created this table before token_hash was added.
	// Add/backfill the column before creating the index so those databases can
	// start normally. PostgreSQL supports IF NOT EXISTS; SQLite uses PRAGMA.
	if db.isSQLite() {
		if err := db.ensureSQLiteColumn(ctx, "codex_turn_states", "token_hash", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	} else if _, err := db.conn.ExecContext(ctx, `ALTER TABLE codex_turn_states ADD COLUMN IF NOT EXISTS token_hash TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := db.backfillCodexTurnStateTokenHashes(ctx); err != nil {
		return err
	}
	_, err := db.conn.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_codex_turn_state_unique_ticket ON codex_turn_states(account_id,token_hash) WHERE token_hash <> ''`)
	return err
}

func (db *DB) backfillCodexTurnStateTokenHashes(ctx context.Context) error {
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT account_id,model,payload FROM codex_turn_states ORDER BY account_id,model`)
		if err != nil {
			return err
		}
		type row struct {
			accountID int64
			model     string
			payload   string
		}
		var records []row
		for rows.Next() {
			var item row
			if err := rows.Scan(&item.accountID, &item.model, &item.payload); err != nil {
				rows.Close()
				return err
			}
			records = append(records, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		seen := make(map[string]struct{}, len(records))
		for _, item := range records {
			var rec CodexTurnStateRecord
			if err := decodeTurnStateRecord(item.payload, &rec); err != nil {
				return fmt.Errorf("decode Codex turn-state account=%d model=%s: %w", item.accountID, item.model, err)
			}
			hash := turnStateTokenHash(rec.Token)
			key := fmt.Sprintf("%d\x00%s", item.accountID, hash)
			if hash != "" {
				if _, duplicate := seen[key]; duplicate {
					rec.Token = ""
					rec.IssuedAt = time.Time{}
					rec.CapturedAt = time.Time{}
					rec.ExpiresAt = time.Time{}
					rec.NextAttemptAt = time.Time{}
					rec.RefreshRequested = true
					rec.LastError = "duplicate legacy turn-state removed; refresh requested"
					hash = ""
				} else {
					seen[key] = struct{}{}
				}
			}
			payload, err := json.Marshal(rec)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE codex_turn_states SET payload=$3,token_hash=$4 WHERE account_id=$1 AND model=$2`,
				item.accountID, item.model, encryptCredentialValue("codex_turn_state_ticket", string(payload)), hash); err != nil {
				return err
			}
		}
		return nil
	})
}

func turnStateTokenHash(token string) string {
	if token == "" {
		return ""
	}
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}

func (db *DB) LoadCodexTurnStateConfig(ctx context.Context) (string, error) {
	var raw string
	err := db.conn.QueryRowContext(ctx, `SELECT config FROM codex_turn_state_settings WHERE id=1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "{}", nil
	}
	return decryptCredentialValue("codex_turn_state_config", raw), err
}

func (db *DB) SaveCodexTurnStateConfig(ctx context.Context, raw string) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		_, err := db.conn.ExecContext(ctx, `INSERT INTO codex_turn_state_settings(id,config) VALUES(1,$1)
			ON CONFLICT(id) DO UPDATE SET config=EXCLUDED.config`, encryptCredentialValue("codex_turn_state_config", raw))
		return err
	})
}

func decodeTurnStateRecord(raw string, record *CodexTurnStateRecord) error {
	return json.Unmarshal([]byte(decryptCredentialValue("codex_turn_state_ticket", raw)), record)
}

func (db *DB) ListCodexTurnStates(ctx context.Context) ([]CodexTurnStateRecord, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT t.account_id,t.model,t.payload,t.revision,t.lease_id,t.lease_until
		FROM codex_turn_states t JOIN accounts a ON a.id=t.account_id WHERE a.status <> 'deleted'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]CodexTurnStateRecord, 0)
	for rows.Next() {
		var rec CodexTurnStateRecord
		var raw string
		if err := rows.Scan(&rec.AccountID, &rec.Model, &raw, &rec.Revision, &rec.Lease, &rec.LeaseUntil); err != nil {
			return nil, err
		}
		if err := decodeTurnStateRecord(raw, &rec); err != nil {
			return nil, errors.New("cannot decode persisted turn-state ticket")
		}
		result = append(result, rec)
	}
	return result, rows.Err()
}

func (db *DB) ClaimCodexTurnState(ctx context.Context, id int64, model string, now time.Time, duration time.Duration) (CodexTurnStateRecord, bool, error) {
	rec := CodexTurnStateRecord{AccountID: id, Model: model}
	claimed := false
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO codex_turn_states(account_id,model)
			SELECT id,$2 FROM accounts WHERE id=$1 AND status <> 'deleted' AND enabled=TRUE
			ON CONFLICT(account_id,model) DO NOTHING`, id, model)
		if err != nil {
			return err
		}
		lease := uuid.NewString()
		res, err := tx.ExecContext(ctx, `UPDATE codex_turn_states SET lease_id=$3,lease_until=$4
			WHERE account_id=$1 AND model=$2 AND lease_until <= $5
			AND EXISTS(SELECT 1 FROM accounts WHERE id=$1 AND status <> 'deleted' AND enabled=TRUE)`, id, model, lease, now.Add(duration).Unix(), now.Unix())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT payload,revision,lease_id,lease_until FROM codex_turn_states
			WHERE account_id=$1 AND model=$2`, id, model).Scan(&raw, &rec.Revision, &rec.Lease, &rec.LeaseUntil); err != nil {
			return err
		}
		if err := decodeTurnStateRecord(raw, &rec); err != nil {
			return errors.New("cannot decode persisted turn-state ticket")
		}
		claimed = true
		return nil
	})
	return rec, claimed && err == nil, err
}

// CommitCodexTurnState releases the lease only if this worker still owns it.
func (db *DB) CommitCodexTurnState(ctx context.Context, rec CodexTurnStateRecord) (bool, error) {
	payload, err := json.Marshal(rec)
	if err != nil {
		return false, err
	}
	changed := false
	err = db.withSQLiteWriteLock(ctx, func() error {
		res, err := db.conn.ExecContext(ctx, `UPDATE codex_turn_states SET payload=$3,revision=revision+1,lease_id='',lease_until=0,token_hash=$6
			WHERE account_id=$1 AND model=$2 AND revision=$4 AND lease_id=$5`, rec.AccountID, rec.Model,
			encryptCredentialValue("codex_turn_state_ticket", string(payload)), rec.Revision, rec.Lease, turnStateTokenHash(rec.Token))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		changed = n == 1
		return err
	})
	return changed, err
}

// Replacing with an empty record leaves a tombstone that fences in-flight probes.
func (db *DB) ReplaceCodexTurnState(ctx context.Context, rec CodexTurnStateRecord) (CodexTurnStateRecord, error) {
	payload, err := json.Marshal(rec)
	if err != nil {
		return rec, err
	}
	err = db.withSQLiteWriteLock(ctx, func() error {
		return db.conn.QueryRowContext(ctx, `INSERT INTO codex_turn_states(account_id,model,payload,revision,token_hash) VALUES($1,$2,$3,1,$4)
			ON CONFLICT(account_id,model) DO UPDATE SET payload=EXCLUDED.payload,revision=codex_turn_states.revision+1,lease_id='',lease_until=0,token_hash=EXCLUDED.token_hash
			RETURNING revision`, rec.AccountID, rec.Model, encryptCredentialValue("codex_turn_state_ticket", string(payload)), turnStateTokenHash(rec.Token)).Scan(&rec.Revision)
	})
	rec.Lease = ""
	rec.LeaseUntil = 0
	return rec, err
}

func (db *DB) RequestCodexTurnStateRefresh(ctx context.Context, id int64, model string) (CodexTurnStateRecord, error) {
	rec, _, err := db.markCodexTurnStateRefresh(ctx, id, model, "")
	return rec, err
}

func (db *DB) InvalidateCodexTurnState(ctx context.Context, id int64, model, used string) (CodexTurnStateRecord, bool, error) {
	if used == "" {
		return CodexTurnStateRecord{}, false, nil
	}
	return db.markCodexTurnStateRefresh(ctx, id, model, used)
}

func (db *DB) markCodexTurnStateRefresh(ctx context.Context, id int64, model, used string) (CodexTurnStateRecord, bool, error) {
	rec := CodexTurnStateRecord{AccountID: id, Model: model}
	changed := false
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO codex_turn_states(account_id,model) VALUES($1,$2)
			ON CONFLICT(account_id,model) DO NOTHING`, id, model); err != nil {
			return err
		}
		query := `SELECT payload,revision FROM codex_turn_states WHERE account_id=$1 AND model=$2`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		var raw string
		if err := tx.QueryRowContext(ctx, query, id, model).Scan(&raw, &rec.Revision); err != nil {
			return err
		}
		if err := decodeTurnStateRecord(raw, &rec); err != nil {
			return errors.New("cannot decode persisted turn-state ticket")
		}
		if used != "" {
			if rec.Token != used {
				return nil
			}
			rec.Token = ""
			rec.ExpiresAt = time.Time{}
			rec.LastError = "observed a 312-character turn-state; refresh requested"
		}
		rec.RefreshRequested = true
		rec.NextAttemptAt = time.Time{}
		payload, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE codex_turn_states SET payload=$3,revision=revision+1,lease_id='',lease_until=0,token_hash=$4 WHERE account_id=$1 AND model=$2`,
			id, model, encryptCredentialValue("codex_turn_state_ticket", string(payload)), turnStateTokenHash(rec.Token))
		rec.Revision++
		changed = err == nil
		return err
	})
	return rec, changed, err
}

func (db *DB) ReleaseCodexTurnState(ctx context.Context, rec CodexTurnStateRecord) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		_, err := db.conn.ExecContext(ctx, `UPDATE codex_turn_states SET lease_id='',lease_until=0
			WHERE account_id=$1 AND model=$2 AND lease_id=$3`, rec.AccountID, rec.Model, rec.Lease)
		return err
	})
}
