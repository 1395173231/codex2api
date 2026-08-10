package database

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	ProfitScalePPM                  int64 = 1_000_000
	DefaultProfitSettlementRatioPPM int64 = ProfitScalePPM
	DefaultProfitGroupMultiplierPPM int64 = ProfitScalePPM
	DefaultProfitLedgerRefreshLimit       = 1_000
	MaxProfitLedgerRefreshLimit           = 1_000
	ProfitTimezone                        = "Asia/Shanghai"
)

var (
	ErrProfitPendingAssignment  = errors.New("profit settlement contains pending account assignments")
	ErrProfitLedgerBehind       = errors.New("profit daily ledger is not caught up")
	ErrProfitSettlementEmpty    = errors.New("profit settlement contains no eligible ledger rows")
	ErrProfitSettlementConflict = errors.New("profit settlement source rows changed or were already claimed")
)

// ProfitSettings 独立于 SystemSettings，避免继续扩大旧设置表的高风险位置参数链。
// 金额源账本始终为 USD 微单位；ratio/multiplier 都使用 ppm，避免 float 累计误差。
type ProfitSettings struct {
	Enabled                   bool   `json:"enabled"`
	DefaultSettlementRatioPPM int64  `json:"default_settlement_ratio_ppm"`
	DefaultGroupMultiplierPPM int64  `json:"default_group_multiplier_ppm"`
	Timezone                  string `json:"timezone"`
	UpdatedAt                 string `json:"updated_at,omitempty"`
}

type ProfitSettingsUpdate struct {
	Enabled                   *bool  `json:"enabled,omitempty"`
	DefaultSettlementRatioPPM *int64 `json:"default_settlement_ratio_ppm,omitempty"`
	DefaultGroupMultiplierPPM *int64 `json:"default_group_multiplier_ppm,omitempty"`
}

type ProfitGroupSetting struct {
	GroupID       int64  `json:"group_id"`
	GroupName     string `json:"group_name"`
	MultiplierPPM int64  `json:"multiplier_ppm"`
	AssignedCount int64  `json:"assigned_count"`
	Historical    bool   `json:"historical"`
}

type ProfitOperationalGroup struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type ProfitPendingAccount struct {
	AccountID         int64                    `json:"account_id"`
	AccountName       string                   `json:"account_name"`
	Deleted           bool                     `json:"deleted"`
	PendingRequests   int64                    `json:"pending_requests"`
	OfficialUSDMicros int64                    `json:"official_cost_usd_micros"`
	FirstDate         string                   `json:"first_date"`
	LastDate          string                   `json:"last_date"`
	OperationalGroups []ProfitOperationalGroup `json:"operational_groups"`
}

type ProfitLedgerRefreshResult struct {
	ProcessedLogs  int64  `json:"processed_logs"`
	AggregatedLogs int64  `json:"aggregated_logs"`
	CheckpointID   int64  `json:"checkpoint_id"`
	HighWaterID    int64  `json:"high_water_id"`
	RemainingLogs  int64  `json:"remaining_logs"`
	CaughtUp       bool   `json:"caught_up"`
	UpdatedAt      string `json:"updated_at"`
}

type ProfitMoneySummary struct {
	OfficialUSDMicros   int64    `json:"official_cost_usd_micros"`
	SettlementCNYMicros int64    `json:"settlement_cost_cny_micros"`
	RevenueCNYMicros    int64    `json:"revenue_cny_micros"`
	ProfitCNYMicros     int64    `json:"profit_cny_micros"`
	Margin              *float64 `json:"margin"`
	RequestCount        int64    `json:"request_count"`
	InputTokens         int64    `json:"input_tokens"`
	OutputTokens        int64    `json:"output_tokens"`
	CachedTokens        int64    `json:"cached_tokens"`
	ReasoningTokens     int64    `json:"reasoning_tokens"`
	TotalTokens         int64    `json:"total_tokens"`
}

type ProfitDashboardDimension struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Deleted       bool   `json:"deleted,omitempty"`
	Pending       bool   `json:"pending,omitempty"`
	MultiplierPPM int64  `json:"multiplier_ppm,omitempty"`
	ProfitMoneySummary
}

type ProfitDashboardResponse struct {
	StartDate          string                     `json:"start_date"`
	EndDate            string                     `json:"end_date"`
	Timezone           string                     `json:"timezone"`
	SettlementRatioPPM int64                      `json:"settlement_ratio_ppm"`
	Ledger             ProfitLedgerRefreshResult  `json:"ledger"`
	Overall            ProfitMoneySummary         `json:"overall"`
	Groups             []ProfitDashboardDimension `json:"groups"`
	APIKeys            []ProfitDashboardDimension `json:"api_keys"`
	Accounts           []ProfitDashboardDimension `json:"accounts"`
	Models             []ProfitDashboardDimension `json:"models"`
}

type ProfitSettlementRun struct {
	ID                  string `json:"id"`
	LineageID           string `json:"lineage_id"`
	RevisionNo          int    `json:"revision_no"`
	SupersedesID        string `json:"supersedes_id,omitempty"`
	Status              string `json:"status"`
	StartDate           string `json:"start_date"`
	EndDate             string `json:"end_date"`
	SettlementRatioPPM  int64  `json:"settlement_ratio_ppm"`
	Notes               string `json:"notes"`
	OfficialUSDMicros   int64  `json:"official_cost_usd_micros"`
	SettlementCNYMicros int64  `json:"settlement_cost_cny_micros"`
	RevenueCNYMicros    int64  `json:"revenue_cny_micros"`
	ProfitCNYMicros     int64  `json:"profit_cny_micros"`
	SourceManifestHash  string `json:"source_manifest_hash"`
	CreatedAt           string `json:"created_at"`
	ConfirmedAt         string `json:"confirmed_at,omitempty"`
}

type ProfitSettlementItem struct {
	LedgerRowID         int64  `json:"ledger_row_id"`
	LedgerVersion       int64  `json:"ledger_version"`
	LedgerDate          string `json:"ledger_date"`
	GroupID             int64  `json:"group_id"`
	GroupName           string `json:"group_name"`
	APIKeyID            int64  `json:"api_key_id"`
	APIKeyName          string `json:"api_key_name"`
	AccountID           int64  `json:"account_id"`
	AccountName         string `json:"account_name"`
	AccountDeleted      bool   `json:"account_deleted"`
	Model               string `json:"model"`
	Channel             string `json:"channel"`
	MultiplierPPM       int64  `json:"multiplier_ppm"`
	RequestCount        int64  `json:"request_count"`
	TotalTokens         int64  `json:"total_tokens"`
	OfficialUSDMicros   int64  `json:"official_cost_usd_micros"`
	SettlementCNYMicros int64  `json:"settlement_cost_cny_micros"`
	RevenueCNYMicros    int64  `json:"revenue_cny_micros"`
	ProfitCNYMicros     int64  `json:"profit_cny_micros"`
	SourceFirstLogID    int64  `json:"source_first_log_id"`
	SourceLastLogID     int64  `json:"source_last_log_id"`
	SourceHash          string `json:"source_hash"`
}

type ProfitSettlementDetail struct {
	Run   ProfitSettlementRun    `json:"run"`
	Items []ProfitSettlementItem `json:"items"`
}

func profitLocation() *time.Location {
	loc, err := time.LoadLocation(ProfitTimezone)
	if err == nil {
		return loc
	}
	return time.FixedZone(ProfitTimezone, 8*60*60)
}

func normalizeProfitPPM(value int64, fallback int64) int64 {
	if value <= 0 {
		return fallback
	}
	if value > 1_000_000_000 {
		return 1_000_000_000
	}
	return value
}

func profitMicros(value float64) int64 {
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	if value >= float64(math.MaxInt64)/float64(ProfitScalePPM) {
		return math.MaxInt64
	}
	return int64(math.Round(value * float64(ProfitScalePPM)))
}

func profitMulDiv(value int64, multiplier int64, divisor int64) int64 {
	if value == 0 || multiplier == 0 || divisor <= 0 {
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(value), big.NewInt(multiplier))
	half := big.NewInt(divisor / 2)
	if n.Sign() >= 0 {
		n.Add(n, half)
	} else {
		n.Sub(n, half)
	}
	n.Quo(n, big.NewInt(divisor))
	if !n.IsInt64() {
		if n.Sign() < 0 {
			return math.MinInt64
		}
		return math.MaxInt64
	}
	return n.Int64()
}

func applyProfitPricing(summary *ProfitMoneySummary, ratioPPM int64, multiplierPPM int64) {
	summary.SettlementCNYMicros = profitMulDiv(summary.OfficialUSDMicros, ratioPPM, ProfitScalePPM)
	summary.RevenueCNYMicros = profitMulDiv(summary.SettlementCNYMicros, multiplierPPM, ProfitScalePPM)
	summary.ProfitCNYMicros = summary.RevenueCNYMicros - summary.SettlementCNYMicros
	if summary.RevenueCNYMicros == 0 {
		summary.Margin = nil
		return
	}
	margin := float64(summary.ProfitCNYMicros) / float64(summary.RevenueCNYMicros)
	summary.Margin = &margin
}

func newProfitID(prefix string) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(buf), nil
}

func (db *DB) migrateProfitSettlement(ctx context.Context) error {
	if db.isSQLite() {
		if err := db.ensureSQLiteColumn(ctx, "usage_logs", "settlement_group_id_snapshot", "INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		if err := db.ensureSQLiteColumn(ctx, "usage_logs", "settlement_group_name_snapshot", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		if err := db.ensureSQLiteColumn(ctx, "usage_logs", "settlement_assignment_source", "TEXT NOT NULL DEFAULT 'pending'"); err != nil {
			return err
		}
		for _, stmt := range profitSQLiteSchemaStatements() {
			if _, err := db.conn.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("创建利润结算 SQLite 表失败: %w", err)
			}
		}
		return nil
	}
	if _, err := db.conn.ExecContext(ctx, profitPostgresSchema()); err != nil {
		return fmt.Errorf("创建利润结算 PostgreSQL 表失败: %w", err)
	}
	return nil
}

func profitPostgresSchema() string {
	return `
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS settlement_group_id_snapshot BIGINT NOT NULL DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS settlement_group_name_snapshot TEXT NOT NULL DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS settlement_assignment_source VARCHAR(32) NOT NULL DEFAULT 'pending';

	CREATE TABLE IF NOT EXISTS profit_settings (
		id INTEGER PRIMARY KEY CHECK (id = 1), enabled BOOLEAN NOT NULL DEFAULT FALSE,
		default_settlement_ratio_ppm BIGINT NOT NULL DEFAULT 1000000,
		default_group_multiplier_ppm BIGINT NOT NULL DEFAULT 1000000,
		timezone VARCHAR(64) NOT NULL DEFAULT 'Asia/Shanghai', updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	INSERT INTO profit_settings (id) VALUES (1) ON CONFLICT (id) DO NOTHING;

	CREATE TABLE IF NOT EXISTS profit_account_settings (
		account_id BIGINT PRIMARY KEY, settlement_group_id BIGINT NOT NULL,
		settlement_group_name TEXT NOT NULL DEFAULT '', assignment_source VARCHAR(32) NOT NULL DEFAULT 'confirmed',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_profit_account_settings_group ON profit_account_settings(settlement_group_id);

	CREATE TABLE IF NOT EXISTS profit_group_settings (
		group_id BIGINT PRIMARY KEY, group_name_snapshot TEXT NOT NULL DEFAULT '',
		multiplier_ppm BIGINT NOT NULL DEFAULT 1000000, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS profit_ledger_state (
		id INTEGER PRIMARY KEY CHECK (id = 1), last_usage_log_id BIGINT NOT NULL DEFAULT 0,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	INSERT INTO profit_ledger_state (id) VALUES (1) ON CONFLICT (id) DO NOTHING;

	CREATE TABLE IF NOT EXISTS profit_daily_ledger (
		id BIGSERIAL PRIMARY KEY, ledger_date DATE NOT NULL, segment INTEGER NOT NULL DEFAULT 0,
		api_key_id BIGINT NOT NULL DEFAULT 0, api_key_name_snapshot TEXT NOT NULL DEFAULT '', api_key_masked_snapshot TEXT NOT NULL DEFAULT '',
		account_id BIGINT NOT NULL DEFAULT 0, account_name_snapshot TEXT NOT NULL DEFAULT '', account_deleted BOOLEAN NOT NULL DEFAULT FALSE,
		channel VARCHAR(32) NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
		settlement_group_id BIGINT NOT NULL DEFAULT 0, settlement_group_name_snapshot TEXT NOT NULL DEFAULT '',
		assignment_source VARCHAR(32) NOT NULL DEFAULT 'pending', request_count BIGINT NOT NULL DEFAULT 0,
		input_tokens BIGINT NOT NULL DEFAULT 0, output_tokens BIGINT NOT NULL DEFAULT 0,
		cached_tokens BIGINT NOT NULL DEFAULT 0, reasoning_tokens BIGINT NOT NULL DEFAULT 0, total_tokens BIGINT NOT NULL DEFAULT 0,
		official_cost_usd_micros BIGINT NOT NULL DEFAULT 0,
		source_first_log_id BIGINT NOT NULL DEFAULT 0, source_last_log_id BIGINT NOT NULL DEFAULT 0,
		source_hash VARCHAR(64) NOT NULL DEFAULT '', ledger_version BIGINT NOT NULL DEFAULT 1,
		claimed_lineage_id VARCHAR(80) NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		UNIQUE (ledger_date, segment, api_key_id, account_id, model, channel, settlement_group_id)
	);
	CREATE INDEX IF NOT EXISTS idx_profit_daily_ledger_range ON profit_daily_ledger(ledger_date, settlement_group_id);
	CREATE INDEX IF NOT EXISTS idx_profit_daily_ledger_account ON profit_daily_ledger(account_id, ledger_date);
	CREATE INDEX IF NOT EXISTS idx_profit_daily_ledger_claim ON profit_daily_ledger(claimed_lineage_id);
	CREATE INDEX IF NOT EXISTS idx_profit_daily_ledger_upsert ON profit_daily_ledger(
		ledger_date, api_key_id, account_id, model, channel, settlement_group_id, segment DESC
	);

	CREATE TABLE IF NOT EXISTS profit_settlement_runs (
		id VARCHAR(80) PRIMARY KEY, lineage_id VARCHAR(80) NOT NULL, revision_no INTEGER NOT NULL DEFAULT 1,
		supersedes_id VARCHAR(80) NULL, status VARCHAR(24) NOT NULL DEFAULT 'draft', start_date DATE NOT NULL, end_date DATE NOT NULL,
		settlement_ratio_ppm BIGINT NOT NULL, notes TEXT NOT NULL DEFAULT '', official_cost_usd_micros BIGINT NOT NULL DEFAULT 0,
		settlement_cost_cny_micros BIGINT NOT NULL DEFAULT 0, revenue_cny_micros BIGINT NOT NULL DEFAULT 0,
		profit_cny_micros BIGINT NOT NULL DEFAULT 0, source_manifest_hash VARCHAR(64) NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), confirmed_at TIMESTAMPTZ NULL
	);
	CREATE INDEX IF NOT EXISTS idx_profit_settlement_runs_lineage ON profit_settlement_runs(lineage_id, revision_no DESC);
	CREATE INDEX IF NOT EXISTS idx_profit_settlement_runs_range ON profit_settlement_runs(start_date, end_date);

	CREATE TABLE IF NOT EXISTS profit_settlement_items (
		run_id VARCHAR(80) NOT NULL, ledger_row_id BIGINT NOT NULL, ledger_version BIGINT NOT NULL,
		ledger_date DATE NOT NULL, group_id BIGINT NOT NULL, group_name TEXT NOT NULL DEFAULT '',
		api_key_id BIGINT NOT NULL DEFAULT 0, api_key_name TEXT NOT NULL DEFAULT '', account_id BIGINT NOT NULL DEFAULT 0,
		account_name TEXT NOT NULL DEFAULT '', account_deleted BOOLEAN NOT NULL DEFAULT FALSE,
		model TEXT NOT NULL DEFAULT '', channel VARCHAR(32) NOT NULL DEFAULT '', multiplier_ppm BIGINT NOT NULL,
		request_count BIGINT NOT NULL DEFAULT 0, total_tokens BIGINT NOT NULL DEFAULT 0,
		official_cost_usd_micros BIGINT NOT NULL DEFAULT 0, settlement_cost_cny_micros BIGINT NOT NULL DEFAULT 0,
		revenue_cny_micros BIGINT NOT NULL DEFAULT 0, profit_cny_micros BIGINT NOT NULL DEFAULT 0,
		source_first_log_id BIGINT NOT NULL DEFAULT 0, source_last_log_id BIGINT NOT NULL DEFAULT 0, source_hash VARCHAR(64) NOT NULL DEFAULT '',
		PRIMARY KEY (run_id, ledger_row_id)
	);
	CREATE INDEX IF NOT EXISTS idx_profit_settlement_items_ledger ON profit_settlement_items(ledger_row_id);

	CREATE TABLE IF NOT EXISTS profit_ledger_claims (
		ledger_row_id BIGINT PRIMARY KEY, lineage_id VARCHAR(80) NOT NULL, run_id VARCHAR(80) NOT NULL,
		claimed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_profit_ledger_claims_lineage ON profit_ledger_claims(lineage_id);
	`
}

func profitSQLiteSchemaStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS profit_settings (
			id INTEGER PRIMARY KEY CHECK (id = 1), enabled INTEGER NOT NULL DEFAULT 0,
			default_settlement_ratio_ppm INTEGER NOT NULL DEFAULT 1000000,
			default_group_multiplier_ppm INTEGER NOT NULL DEFAULT 1000000,
			timezone TEXT NOT NULL DEFAULT 'Asia/Shanghai', updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT OR IGNORE INTO profit_settings (id) VALUES (1)`,
		`CREATE TABLE IF NOT EXISTS profit_account_settings (
			account_id INTEGER PRIMARY KEY, settlement_group_id INTEGER NOT NULL, settlement_group_name TEXT NOT NULL DEFAULT '',
			assignment_source TEXT NOT NULL DEFAULT 'confirmed', created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_profit_account_settings_group ON profit_account_settings(settlement_group_id)`,
		`CREATE TABLE IF NOT EXISTS profit_group_settings (
			group_id INTEGER PRIMARY KEY, group_name_snapshot TEXT NOT NULL DEFAULT '', multiplier_ppm INTEGER NOT NULL DEFAULT 1000000,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS profit_ledger_state (
			id INTEGER PRIMARY KEY CHECK (id = 1), last_usage_log_id INTEGER NOT NULL DEFAULT 0,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT OR IGNORE INTO profit_ledger_state (id) VALUES (1)`,
		`CREATE TABLE IF NOT EXISTS profit_daily_ledger (
			id INTEGER PRIMARY KEY AUTOINCREMENT, ledger_date TEXT NOT NULL, segment INTEGER NOT NULL DEFAULT 0,
			api_key_id INTEGER NOT NULL DEFAULT 0, api_key_name_snapshot TEXT NOT NULL DEFAULT '', api_key_masked_snapshot TEXT NOT NULL DEFAULT '',
			account_id INTEGER NOT NULL DEFAULT 0, account_name_snapshot TEXT NOT NULL DEFAULT '', account_deleted INTEGER NOT NULL DEFAULT 0,
			channel TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', settlement_group_id INTEGER NOT NULL DEFAULT 0,
			settlement_group_name_snapshot TEXT NOT NULL DEFAULT '', assignment_source TEXT NOT NULL DEFAULT 'pending', request_count INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0, cached_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0, official_cost_usd_micros INTEGER NOT NULL DEFAULT 0,
			source_first_log_id INTEGER NOT NULL DEFAULT 0, source_last_log_id INTEGER NOT NULL DEFAULT 0,
			source_hash TEXT NOT NULL DEFAULT '', ledger_version INTEGER NOT NULL DEFAULT 1, claimed_lineage_id TEXT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE (ledger_date, segment, api_key_id, account_id, model, channel, settlement_group_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_profit_daily_ledger_range ON profit_daily_ledger(ledger_date, settlement_group_id)`,
		`CREATE INDEX IF NOT EXISTS idx_profit_daily_ledger_account ON profit_daily_ledger(account_id, ledger_date)`,
		`CREATE INDEX IF NOT EXISTS idx_profit_daily_ledger_claim ON profit_daily_ledger(claimed_lineage_id)`,
		`CREATE INDEX IF NOT EXISTS idx_profit_daily_ledger_upsert ON profit_daily_ledger(
			ledger_date, api_key_id, account_id, model, channel, settlement_group_id, segment DESC
		)`,
		`CREATE TABLE IF NOT EXISTS profit_settlement_runs (
			id TEXT PRIMARY KEY, lineage_id TEXT NOT NULL, revision_no INTEGER NOT NULL DEFAULT 1, supersedes_id TEXT NULL,
			status TEXT NOT NULL DEFAULT 'draft', start_date TEXT NOT NULL, end_date TEXT NOT NULL, settlement_ratio_ppm INTEGER NOT NULL,
			notes TEXT NOT NULL DEFAULT '', official_cost_usd_micros INTEGER NOT NULL DEFAULT 0, settlement_cost_cny_micros INTEGER NOT NULL DEFAULT 0,
			revenue_cny_micros INTEGER NOT NULL DEFAULT 0, profit_cny_micros INTEGER NOT NULL DEFAULT 0,
			source_manifest_hash TEXT NOT NULL DEFAULT '', created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, confirmed_at TIMESTAMP NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_profit_settlement_runs_lineage ON profit_settlement_runs(lineage_id, revision_no DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_profit_settlement_runs_range ON profit_settlement_runs(start_date, end_date)`,
		`CREATE TABLE IF NOT EXISTS profit_settlement_items (
			run_id TEXT NOT NULL, ledger_row_id INTEGER NOT NULL, ledger_version INTEGER NOT NULL, ledger_date TEXT NOT NULL,
			group_id INTEGER NOT NULL, group_name TEXT NOT NULL DEFAULT '', api_key_id INTEGER NOT NULL DEFAULT 0, api_key_name TEXT NOT NULL DEFAULT '',
			account_id INTEGER NOT NULL DEFAULT 0, account_name TEXT NOT NULL DEFAULT '', account_deleted INTEGER NOT NULL DEFAULT 0,
			model TEXT NOT NULL DEFAULT '', channel TEXT NOT NULL DEFAULT '', multiplier_ppm INTEGER NOT NULL,
			request_count INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0, official_cost_usd_micros INTEGER NOT NULL DEFAULT 0,
			settlement_cost_cny_micros INTEGER NOT NULL DEFAULT 0, revenue_cny_micros INTEGER NOT NULL DEFAULT 0,
			profit_cny_micros INTEGER NOT NULL DEFAULT 0, source_first_log_id INTEGER NOT NULL DEFAULT 0,
			source_last_log_id INTEGER NOT NULL DEFAULT 0, source_hash TEXT NOT NULL DEFAULT '', PRIMARY KEY (run_id, ledger_row_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_profit_settlement_items_ledger ON profit_settlement_items(ledger_row_id)`,
		`CREATE TABLE IF NOT EXISTS profit_ledger_claims (
			ledger_row_id INTEGER PRIMARY KEY, lineage_id TEXT NOT NULL, run_id TEXT NOT NULL,
			claimed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_profit_ledger_claims_lineage ON profit_ledger_claims(lineage_id)`,
	}
}

func (db *DB) GetProfitSettings(ctx context.Context) (ProfitSettings, error) {
	var result ProfitSettings
	var updatedRaw interface{}
	err := db.conn.QueryRowContext(ctx, `
		SELECT enabled, default_settlement_ratio_ppm, default_group_multiplier_ppm, timezone, updated_at
		FROM profit_settings WHERE id = 1
	`).Scan(&result.Enabled, &result.DefaultSettlementRatioPPM, &result.DefaultGroupMultiplierPPM, &result.Timezone, &updatedRaw)
	if err != nil {
		return result, err
	}
	result.DefaultSettlementRatioPPM = normalizeProfitPPM(result.DefaultSettlementRatioPPM, DefaultProfitSettlementRatioPPM)
	result.DefaultGroupMultiplierPPM = normalizeProfitPPM(result.DefaultGroupMultiplierPPM, DefaultProfitGroupMultiplierPPM)
	if result.Timezone == "" {
		result.Timezone = ProfitTimezone
	}
	if updated, parseErr := parseDBTimeValue(updatedRaw); parseErr == nil {
		result.UpdatedAt = updated.Format(time.RFC3339)
	}
	return result, nil
}

func (db *DB) UpdateProfitSettings(ctx context.Context, update ProfitSettingsUpdate) (ProfitSettings, error) {
	current, err := db.GetProfitSettings(ctx)
	if err != nil {
		return current, err
	}
	if update.Enabled != nil {
		current.Enabled = *update.Enabled
	}
	if update.DefaultSettlementRatioPPM != nil {
		current.DefaultSettlementRatioPPM = normalizeProfitPPM(*update.DefaultSettlementRatioPPM, DefaultProfitSettlementRatioPPM)
	}
	if update.DefaultGroupMultiplierPPM != nil {
		current.DefaultGroupMultiplierPPM = normalizeProfitPPM(*update.DefaultGroupMultiplierPPM, DefaultProfitGroupMultiplierPPM)
	}
	nowExpr := "NOW()"
	if db.isSQLite() {
		nowExpr = "CURRENT_TIMESTAMP"
	}
	_, err = db.conn.ExecContext(ctx, `UPDATE profit_settings SET enabled = $1,
		default_settlement_ratio_ppm = $2, default_group_multiplier_ppm = $3, timezone = $4,
		updated_at = `+nowExpr+` WHERE id = 1`, current.Enabled, current.DefaultSettlementRatioPPM,
		current.DefaultGroupMultiplierPPM, ProfitTimezone)
	if err != nil {
		return current, err
	}
	return db.GetProfitSettings(ctx)
}

func (db *DB) populateProfitSettlementSnapshots(ctx context.Context, tx *sql.Tx, batch []usageLogEntry) ([]usageLogEntry, error) {
	if len(batch) == 0 {
		return batch, nil
	}
	ids := make([]int64, 0)
	seen := make(map[int64]struct{})
	for _, entry := range batch {
		if entry.AccountID <= 0 {
			continue
		}
		if _, ok := seen[entry.AccountID]; ok {
			continue
		}
		seen[entry.AccountID] = struct{}{}
		ids = append(ids, entry.AccountID)
	}
	if len(ids) == 0 {
		return batch, nil
	}
	args := make([]interface{}, 0, len(ids))
	placeholders := make([]string, 0, len(ids))
	for i, id := range ids {
		args = append(args, id)
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+1))
	}
	rows, err := tx.QueryContext(ctx, `SELECT pas.account_id, pas.settlement_group_id,
		COALESCE(NULLIF(g.name, ''), pas.settlement_group_name, ''), COALESCE(NULLIF(pas.assignment_source, ''), 'confirmed')
		FROM profit_account_settings pas LEFT JOIN account_groups g ON g.id = pas.settlement_group_id
		WHERE pas.account_id IN (`+strings.Join(placeholders, ",")+`)
		UNION ALL
		SELECT m.account_id, MIN(m.group_id), MAX(g.name), 'inherited'
		FROM account_group_members m JOIN account_groups g ON g.id = m.group_id
		LEFT JOIN profit_account_settings pas ON pas.account_id = m.account_id
		WHERE m.account_id IN (`+strings.Join(placeholders, ",")+`) AND pas.account_id IS NULL
		GROUP BY m.account_id HAVING COUNT(*) = 1`, args...)
	if err != nil {
		return nil, err
	}
	type assignment struct {
		groupID int64
		name    string
		source  string
	}
	assignments := make(map[int64]assignment)
	for rows.Next() {
		var accountID int64
		var item assignment
		if err := rows.Scan(&accountID, &item.groupID, &item.name, &item.source); err != nil {
			rows.Close()
			return nil, err
		}
		assignments[accountID] = item
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := make([]usageLogEntry, len(batch))
	copy(result, batch)
	for i := range result {
		item, ok := assignments[result[i].AccountID]
		if !ok {
			result[i].SettlementGroupIDSnapshot = 0
			result[i].SettlementGroupNameSnapshot = ""
			result[i].SettlementAssignmentSource = "pending"
			continue
		}
		result[i].SettlementGroupIDSnapshot = item.groupID
		result[i].SettlementGroupNameSnapshot = item.name
		result[i].SettlementAssignmentSource = item.source
	}
	return result, nil
}

type profitUsageSource struct {
	ID                  int64
	CreatedAt           time.Time
	AccountID           int64
	AccountName         string
	AccountDeleted      bool
	Channel             string
	Model               string
	APIKeyID            int64
	APIKeyName          string
	APIKeyMasked        string
	SettlementGroupID   int64
	SettlementGroupName string
	AssignmentSource    string
	StatusCode          int
	InputTokens         int64
	OutputTokens        int64
	CachedTokens        int64
	ReasoningTokens     int64
	TotalTokens         int64
	AccountBilled       float64
}

type profitLedgerAggregate struct {
	LedgerDate          string
	APIKeyID            int64
	APIKeyName          string
	APIKeyMasked        string
	AccountID           int64
	AccountName         string
	AccountDeleted      bool
	Channel             string
	Model               string
	SettlementGroupID   int64
	SettlementGroupName string
	AssignmentSource    string
	RequestCount        int64
	InputTokens         int64
	OutputTokens        int64
	CachedTokens        int64
	ReasoningTokens     int64
	TotalTokens         int64
	OfficialUSDMicros   int64
	SourceFirstLogID    int64
	SourceLastLogID     int64
	SourceHash          [32]byte
}

func (a *profitLedgerAggregate) add(row profitUsageSource) {
	a.RequestCount++
	a.InputTokens += row.InputTokens
	a.OutputTokens += row.OutputTokens
	a.CachedTokens += row.CachedTokens
	a.ReasoningTokens += row.ReasoningTokens
	a.TotalTokens += row.TotalTokens
	a.OfficialUSDMicros += profitMicros(row.AccountBilled)
	if a.SourceFirstLogID == 0 || row.ID < a.SourceFirstLogID {
		a.SourceFirstLogID = row.ID
	}
	if row.ID > a.SourceLastLogID {
		a.SourceLastLogID = row.ID
	}
	seed := fmt.Sprintf("%x|%d|%d|%d|%d|%d|%d|%d", a.SourceHash, row.ID,
		row.InputTokens, row.OutputTokens, row.CachedTokens, row.ReasoningTokens, row.TotalTokens, profitMicros(row.AccountBilled))
	a.SourceHash = sha256.Sum256([]byte(seed))
}

func (db *DB) withProfitLedgerTx(ctx context.Context, fn func(*sql.Tx, int64) error) error {
	run := func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		lockQuery := `SELECT last_usage_log_id FROM profit_ledger_state WHERE id = 1`
		if !db.isSQLite() {
			lockQuery += ` FOR UPDATE`
		}
		var checkpoint int64
		if err := tx.QueryRowContext(ctx, lockQuery).Scan(&checkpoint); err != nil {
			return err
		}
		if err := fn(tx, checkpoint); err != nil {
			return err
		}
		return tx.Commit()
	}
	if db.isSQLite() {
		return db.withSQLiteWriteLock(ctx, run)
	}
	return run()
}

func (db *DB) ensureProfitAccountAssignments(ctx context.Context) error {
	run := func() error {
		nowExpr := "NOW()"
		if db.isSQLite() {
			nowExpr = "CURRENT_TIMESTAMP"
		}
		_, err := db.conn.ExecContext(ctx, `INSERT INTO profit_account_settings
			(account_id, settlement_group_id, settlement_group_name, assignment_source, updated_at)
			SELECT m.account_id, MIN(m.group_id), MAX(g.name), 'inherited', `+nowExpr+`
			FROM account_group_members m JOIN account_groups g ON g.id = m.group_id
			LEFT JOIN profit_account_settings pas ON pas.account_id = m.account_id
			WHERE pas.account_id IS NULL
			GROUP BY m.account_id HAVING COUNT(*) = 1
			ON CONFLICT (account_id) DO NOTHING`)
		return err
	}
	if db.isSQLite() {
		return db.withSQLiteWriteLock(ctx, run)
	}
	return run()
}

func (db *DB) autoAssignProfitPendingAccounts(ctx context.Context) error {
	if err := db.ensureProfitAccountAssignments(ctx); err != nil {
		return err
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT DISTINCT l.account_id, pas.settlement_group_id
		FROM profit_daily_ledger l JOIN profit_account_settings pas ON pas.account_id = l.account_id
		WHERE l.settlement_group_id = 0 AND COALESCE(l.claimed_lineage_id, '') = ''
		ORDER BY l.account_id`)
	if err != nil {
		return err
	}
	type candidate struct{ accountID, groupID int64 }
	candidates := make([]candidate, 0)
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.accountID, &item.groupID); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range candidates {
		if err := db.assignProfitSettlementGroup(ctx, item.accountID, item.groupID, "inherited", false); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) RefreshProfitDailyLedger(ctx context.Context, limit int) (ProfitLedgerRefreshResult, error) {
	if limit <= 0 {
		limit = DefaultProfitLedgerRefreshLimit
	}
	if limit > MaxProfitLedgerRefreshLimit {
		limit = MaxProfitLedgerRefreshLimit
	}
	if err := db.autoAssignProfitPendingAccounts(ctx); err != nil {
		return ProfitLedgerRefreshResult{}, err
	}
	result := ProfitLedgerRefreshResult{}
	err := db.withProfitLedgerTx(ctx, func(tx *sql.Tx, checkpoint int64) error {
		result.CheckpointID = checkpoint
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM usage_logs`).Scan(&result.HighWaterID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT ul.id, ul.created_at, ul.account_id, COALESCE(a.name, ''),
				CASE WHEN COALESCE(a.status, '') = 'deleted' OR a.deleted_at IS NOT NULL THEN 1 ELSE 0 END,
				COALESCE(ul.channel, ''), COALESCE(NULLIF(ul.effective_model, ''), ul.model, ''),
				COALESCE(ul.api_key_id, 0), COALESCE(ul.api_key_name, ''), COALESCE(ul.api_key_masked, ''),
				CASE WHEN COALESCE(ul.settlement_group_id_snapshot, 0) > 0 THEN ul.settlement_group_id_snapshot ELSE COALESCE(pas.settlement_group_id, 0) END,
				CASE WHEN COALESCE(ul.settlement_group_id_snapshot, 0) > 0 THEN COALESCE(ul.settlement_group_name_snapshot, '') ELSE COALESCE(NULLIF(g.name, ''), pas.settlement_group_name, '') END,
				CASE WHEN COALESCE(ul.settlement_group_id_snapshot, 0) > 0 THEN COALESCE(ul.settlement_assignment_source, 'pending') ELSE COALESCE(NULLIF(pas.assignment_source, ''), 'pending') END,
				COALESCE(ul.status_code, 0),
				COALESCE(ul.input_tokens, 0), COALESCE(ul.output_tokens, 0), COALESCE(ul.cached_tokens, 0),
				COALESCE(ul.reasoning_tokens, 0), COALESCE(ul.total_tokens, 0), COALESCE(ul.account_billed, 0)
			FROM usage_logs ul LEFT JOIN accounts a ON a.id = ul.account_id
			LEFT JOIN profit_account_settings pas ON pas.account_id = ul.account_id
			LEFT JOIN account_groups g ON g.id = pas.settlement_group_id
			WHERE ul.id > $1 AND ul.id <= $2 ORDER BY ul.id LIMIT $3
		`, checkpoint, result.HighWaterID, limit)
		if err != nil {
			return err
		}
		aggregates := make(map[string]*profitLedgerAggregate)
		lastProcessed := checkpoint
		loc := profitLocation()
		for rows.Next() {
			var source profitUsageSource
			var createdRaw interface{}
			if err := rows.Scan(&source.ID, &createdRaw, &source.AccountID, &source.AccountName, &source.AccountDeleted,
				&source.Channel, &source.Model, &source.APIKeyID, &source.APIKeyName, &source.APIKeyMasked,
				&source.SettlementGroupID, &source.SettlementGroupName, &source.AssignmentSource, &source.StatusCode,
				&source.InputTokens, &source.OutputTokens, &source.CachedTokens, &source.ReasoningTokens,
				&source.TotalTokens, &source.AccountBilled); err != nil {
				rows.Close()
				return err
			}
			created, err := parseDBTimeValue(createdRaw)
			if err != nil {
				rows.Close()
				return err
			}
			source.CreatedAt = created
			lastProcessed = source.ID
			result.ProcessedLogs++
			if source.StatusCode == 499 {
				continue
			}
			ledgerDate := created.In(loc).Format("2006-01-02")
			key := strings.Join([]string{ledgerDate, strconv.FormatInt(source.APIKeyID, 10),
				strconv.FormatInt(source.AccountID, 10), source.Model, source.Channel,
				strconv.FormatInt(source.SettlementGroupID, 10)}, "\x1f")
			aggregate := aggregates[key]
			if aggregate == nil {
				aggregate = &profitLedgerAggregate{
					LedgerDate: ledgerDate, APIKeyID: source.APIKeyID, APIKeyName: source.APIKeyName,
					APIKeyMasked: source.APIKeyMasked, AccountID: source.AccountID, AccountName: source.AccountName,
					AccountDeleted: source.AccountDeleted, Channel: source.Channel, Model: source.Model,
					SettlementGroupID: source.SettlementGroupID, SettlementGroupName: source.SettlementGroupName,
					AssignmentSource: source.AssignmentSource,
				}
				aggregates[key] = aggregate
			}
			aggregate.add(source)
			result.AggregatedLogs++
		}
		if err := rows.Close(); err != nil {
			return err
		}
		keys := make([]string, 0, len(aggregates))
		for key := range aggregates {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if err := db.upsertProfitLedgerAggregate(ctx, tx, aggregates[key]); err != nil {
				return err
			}
		}
		if lastProcessed > checkpoint {
			nowExpr := "NOW()"
			if db.isSQLite() {
				nowExpr = "CURRENT_TIMESTAMP"
			}
			if _, err := tx.ExecContext(ctx, `UPDATE profit_ledger_state SET last_usage_log_id = $1, updated_at = `+nowExpr+` WHERE id = 1`, lastProcessed); err != nil {
				return err
			}
			result.CheckpointID = lastProcessed
		}
		result.RemainingLogs = result.HighWaterID - result.CheckpointID
		if result.RemainingLogs < 0 {
			result.RemainingLogs = 0
		}
		result.CaughtUp = result.CheckpointID >= result.HighWaterID
		result.UpdatedAt = time.Now().Format(time.RFC3339)
		return nil
	})
	return result, err
}

func (db *DB) upsertProfitLedgerAggregate(ctx context.Context, tx *sql.Tx, item *profitLedgerAggregate) error {
	var existingID, segment, version int64
	var claimed sql.NullString
	var oldHash string
	err := tx.QueryRowContext(ctx, `SELECT id, segment, ledger_version, claimed_lineage_id, source_hash
		FROM profit_daily_ledger WHERE ledger_date = $1 AND api_key_id = $2 AND account_id = $3
		AND model = $4 AND channel = $5 AND settlement_group_id = $6 ORDER BY segment DESC LIMIT 1`,
		item.LedgerDate, item.APIKeyID, item.AccountID, item.Model, item.Channel, item.SettlementGroupID).
		Scan(&existingID, &segment, &version, &claimed, &oldHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	chunkHash := hex.EncodeToString(item.SourceHash[:])
	if errors.Is(err, sql.ErrNoRows) || claimed.Valid && claimed.String != "" {
		if claimed.Valid && claimed.String != "" {
			segment++
		} else {
			segment = 0
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO profit_daily_ledger (
			ledger_date, segment, api_key_id, api_key_name_snapshot, api_key_masked_snapshot,
			account_id, account_name_snapshot, account_deleted, channel, model,
			settlement_group_id, settlement_group_name_snapshot, assignment_source,
			request_count, input_tokens, output_tokens, cached_tokens, reasoning_tokens, total_tokens,
			official_cost_usd_micros, source_first_log_id, source_last_log_id, source_hash, ledger_version
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,1)`,
			item.LedgerDate, segment, item.APIKeyID, item.APIKeyName, item.APIKeyMasked,
			item.AccountID, item.AccountName, item.AccountDeleted, item.Channel, item.Model,
			item.SettlementGroupID, item.SettlementGroupName, item.AssignmentSource,
			item.RequestCount, item.InputTokens, item.OutputTokens, item.CachedTokens, item.ReasoningTokens,
			item.TotalTokens, item.OfficialUSDMicros, item.SourceFirstLogID, item.SourceLastLogID, chunkHash)
		return err
	}
	combined := sha256.Sum256([]byte(oldHash + "|" + chunkHash))
	nowExpr := "NOW()"
	if db.isSQLite() {
		nowExpr = "CURRENT_TIMESTAMP"
	}
	_, err = tx.ExecContext(ctx, `UPDATE profit_daily_ledger SET
		api_key_name_snapshot = CASE WHEN api_key_name_snapshot = '' THEN $1 ELSE api_key_name_snapshot END,
		api_key_masked_snapshot = CASE WHEN api_key_masked_snapshot = '' THEN $2 ELSE api_key_masked_snapshot END,
		account_name_snapshot = CASE WHEN account_name_snapshot = '' THEN $3 ELSE account_name_snapshot END,
		account_deleted = CASE WHEN $4 THEN $4 ELSE account_deleted END,
		settlement_group_name_snapshot = CASE WHEN settlement_group_name_snapshot = '' THEN $5 ELSE settlement_group_name_snapshot END,
		assignment_source = CASE WHEN assignment_source = 'pending' AND $6 <> 'pending' THEN $6 ELSE assignment_source END,
		request_count = request_count + $7, input_tokens = input_tokens + $8, output_tokens = output_tokens + $9,
		cached_tokens = cached_tokens + $10, reasoning_tokens = reasoning_tokens + $11, total_tokens = total_tokens + $12,
		official_cost_usd_micros = official_cost_usd_micros + $13,
		source_first_log_id = CASE WHEN source_first_log_id = 0 OR $14 < source_first_log_id THEN $14 ELSE source_first_log_id END,
		source_last_log_id = CASE WHEN $15 > source_last_log_id THEN $15 ELSE source_last_log_id END,
		source_hash = $16, ledger_version = ledger_version + 1, updated_at = `+nowExpr+` WHERE id = $17`,
		item.APIKeyName, item.APIKeyMasked, item.AccountName, item.AccountDeleted, item.SettlementGroupName,
		item.AssignmentSource, item.RequestCount, item.InputTokens, item.OutputTokens, item.CachedTokens,
		item.ReasoningTokens, item.TotalTokens, item.OfficialUSDMicros, item.SourceFirstLogID,
		item.SourceLastLogID, hex.EncodeToString(combined[:]), existingID)
	return err
}

func (db *DB) GetProfitLedgerStatus(ctx context.Context) (ProfitLedgerRefreshResult, error) {
	var result ProfitLedgerRefreshResult
	var updatedRaw interface{}
	err := db.conn.QueryRowContext(ctx, `SELECT s.last_usage_log_id,
		(SELECT COALESCE(MAX(u.id), 0) FROM usage_logs u), s.updated_at
		FROM profit_ledger_state s WHERE s.id = 1`).
		Scan(&result.CheckpointID, &result.HighWaterID, &updatedRaw)
	if err != nil {
		return result, err
	}
	result.RemainingLogs = result.HighWaterID - result.CheckpointID
	if result.RemainingLogs < 0 {
		result.RemainingLogs = 0
	}
	result.CaughtUp = result.RemainingLogs == 0
	if updated, parseErr := parseDBTimeValue(updatedRaw); parseErr == nil {
		result.UpdatedAt = updated.Format(time.RFC3339)
	}
	return result, nil
}

func (db *DB) ListProfitGroupSettings(ctx context.Context) ([]ProfitGroupSetting, error) {
	settings, err := db.GetProfitSettings(ctx)
	if err != nil {
		return nil, err
	}
	groups := make(map[int64]*ProfitGroupSetting)
	rows, err := db.conn.QueryContext(ctx, `SELECT g.id, g.name, COALESCE(p.multiplier_ppm, $1),
		(SELECT COUNT(*) FROM profit_account_settings a WHERE a.settlement_group_id = g.id)
		FROM account_groups g LEFT JOIN profit_group_settings p ON p.group_id = g.id
		ORDER BY g.sort_order, g.id`, settings.DefaultGroupMultiplierPPM)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		item := &ProfitGroupSetting{}
		if err := rows.Scan(&item.GroupID, &item.GroupName, &item.MultiplierPPM, &item.AssignedCount); err != nil {
			rows.Close()
			return nil, err
		}
		item.MultiplierPPM = normalizeProfitPPM(item.MultiplierPPM, settings.DefaultGroupMultiplierPPM)
		groups[item.GroupID] = item
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	// 已删除的业务分组仍可能存在于历史账本中；保留其快照和倍率供审计。
	historyRows, err := db.conn.QueryContext(ctx, `SELECT l.settlement_group_id,
		MAX(l.settlement_group_name_snapshot), COALESCE(p.multiplier_ppm, $1)
		FROM profit_daily_ledger l LEFT JOIN profit_group_settings p ON p.group_id = l.settlement_group_id
		WHERE l.settlement_group_id > 0 GROUP BY l.settlement_group_id, p.multiplier_ppm`, settings.DefaultGroupMultiplierPPM)
	if err != nil {
		return nil, err
	}
	for historyRows.Next() {
		var id, multiplier int64
		var name string
		if err := historyRows.Scan(&id, &name, &multiplier); err != nil {
			historyRows.Close()
			return nil, err
		}
		if _, ok := groups[id]; ok {
			continue
		}
		groups[id] = &ProfitGroupSetting{GroupID: id, GroupName: name, MultiplierPPM: normalizeProfitPPM(multiplier, settings.DefaultGroupMultiplierPPM), Historical: true}
	}
	if err := historyRows.Close(); err != nil {
		return nil, err
	}
	result := make([]ProfitGroupSetting, 0, len(groups))
	for _, item := range groups {
		result = append(result, *item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Historical != result[j].Historical {
			return !result[i].Historical
		}
		return result[i].GroupID < result[j].GroupID
	})
	return result, nil
}

func (db *DB) UpdateProfitGroupMultiplier(ctx context.Context, groupID int64, multiplierPPM int64) (ProfitGroupSetting, error) {
	multiplierPPM = normalizeProfitPPM(multiplierPPM, DefaultProfitGroupMultiplierPPM)
	var groupName string
	err := db.conn.QueryRowContext(ctx, `SELECT name FROM account_groups WHERE id = $1`, groupID).Scan(&groupName)
	if errors.Is(err, sql.ErrNoRows) {
		err = db.conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(settlement_group_name_snapshot), '')
			FROM profit_daily_ledger WHERE settlement_group_id = $1`, groupID).Scan(&groupName)
	}
	if err != nil {
		return ProfitGroupSetting{}, err
	}
	nowExpr := "NOW()"
	if db.isSQLite() {
		nowExpr = "CURRENT_TIMESTAMP"
	}
	_, err = db.conn.ExecContext(ctx, `INSERT INTO profit_group_settings (group_id, group_name_snapshot, multiplier_ppm)
		VALUES ($1,$2,$3) ON CONFLICT (group_id) DO UPDATE SET group_name_snapshot = excluded.group_name_snapshot,
		multiplier_ppm = excluded.multiplier_ppm, updated_at = `+nowExpr, groupID, groupName, multiplierPPM)
	if err != nil {
		return ProfitGroupSetting{}, err
	}
	var assigned int64
	_ = db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM profit_account_settings WHERE settlement_group_id = $1`, groupID).Scan(&assigned)
	return ProfitGroupSetting{GroupID: groupID, GroupName: groupName, MultiplierPPM: multiplierPPM, AssignedCount: assigned}, nil
}

func buildProfitInClause(ids []int64, start int) (string, []interface{}) {
	placeholders := make([]string, 0, len(ids))
	args := make([]interface{}, 0, len(ids))
	for i, id := range ids {
		placeholders = append(placeholders, fmt.Sprintf("$%d", start+i))
		args = append(args, id)
	}
	return strings.Join(placeholders, ","), args
}

func (db *DB) ListProfitPendingAccounts(ctx context.Context) ([]ProfitPendingAccount, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT l.account_id, COALESCE(MAX(a.name), MAX(l.account_name_snapshot), ''),
		MAX(CASE WHEN COALESCE(a.status, '') = 'deleted' OR a.deleted_at IS NOT NULL OR l.account_deleted THEN 1 ELSE 0 END),
		SUM(l.request_count), SUM(l.official_cost_usd_micros), CAST(MIN(l.ledger_date) AS TEXT), CAST(MAX(l.ledger_date) AS TEXT)
		FROM profit_daily_ledger l LEFT JOIN accounts a ON a.id = l.account_id
		WHERE l.settlement_group_id = 0 AND COALESCE(l.claimed_lineage_id, '') = ''
		GROUP BY l.account_id ORDER BY SUM(l.official_cost_usd_micros) DESC, l.account_id`)
	if err != nil {
		return nil, err
	}
	result := make([]ProfitPendingAccount, 0)
	ids := make([]int64, 0)
	for rows.Next() {
		var item ProfitPendingAccount
		var deletedInt int
		if err := rows.Scan(&item.AccountID, &item.AccountName, &deletedInt, &item.PendingRequests,
			&item.OfficialUSDMicros, &item.FirstDate, &item.LastDate); err != nil {
			rows.Close()
			return nil, err
		}
		item.Deleted = deletedInt != 0
		result = append(result, item)
		ids = append(ids, item.AccountID)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return result, nil
	}
	inClause, args := buildProfitInClause(ids, 1)
	groupRows, err := db.conn.QueryContext(ctx, `SELECT m.account_id, g.id, g.name
		FROM account_group_members m JOIN account_groups g ON g.id = m.group_id
		WHERE m.account_id IN (`+inClause+`) ORDER BY g.sort_order, g.id`, args...)
	if err != nil {
		return nil, err
	}
	groupsByAccount := make(map[int64][]ProfitOperationalGroup)
	for groupRows.Next() {
		var accountID int64
		var group ProfitOperationalGroup
		if err := groupRows.Scan(&accountID, &group.ID, &group.Name); err != nil {
			groupRows.Close()
			return nil, err
		}
		groupsByAccount[accountID] = append(groupsByAccount[accountID], group)
	}
	if err := groupRows.Close(); err != nil {
		return nil, err
	}
	for i := range result {
		result[i].OperationalGroups = groupsByAccount[result[i].AccountID]
		if result[i].OperationalGroups == nil {
			result[i].OperationalGroups = []ProfitOperationalGroup{}
		}
	}
	return result, nil
}

type profitLedgerMergeRow struct {
	ID                int64
	LedgerDate        string
	Segment           int64
	APIKeyID          int64
	APIKeyName        string
	APIKeyMasked      string
	AccountID         int64
	AccountName       string
	AccountDeleted    bool
	Channel           string
	Model             string
	RequestCount      int64
	InputTokens       int64
	OutputTokens      int64
	CachedTokens      int64
	ReasoningTokens   int64
	TotalTokens       int64
	OfficialUSDMicros int64
	SourceFirstLogID  int64
	SourceLastLogID   int64
	SourceHash        string
	LedgerVersion     int64
}

func (db *DB) AssignProfitSettlementGroup(ctx context.Context, accountID int64, groupID int64) error {
	return db.assignProfitSettlementGroup(ctx, accountID, groupID, "confirmed", true)
}

func (db *DB) assignProfitSettlementGroup(ctx context.Context, accountID int64, groupID int64, assignmentSource string, backfillUsageLogs bool) error {
	if accountID <= 0 || groupID <= 0 {
		return fmt.Errorf("account_id and group_id must be positive")
	}
	if strings.TrimSpace(assignmentSource) == "" {
		assignmentSource = "confirmed"
	}
	return db.withProfitLedgerTx(ctx, func(tx *sql.Tx, _ int64) error {
		var groupName string
		if err := tx.QueryRowContext(ctx, `SELECT name FROM account_groups WHERE id = $1`, groupID).Scan(&groupName); err != nil {
			return err
		}
		var deletedInt int
		accountErr := tx.QueryRowContext(ctx, `SELECT CASE WHEN status = 'deleted' OR deleted_at IS NOT NULL THEN 1 ELSE 0 END
			FROM accounts WHERE id = $1`, accountID).Scan(&deletedInt)
		if accountErr != nil && !errors.Is(accountErr, sql.ErrNoRows) {
			return accountErr
		}
		if deletedInt != 0 {
			var membershipCount int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_group_members WHERE account_id = $1`, accountID).Scan(&membershipCount); err != nil {
				return err
			}
			if membershipCount == 0 {
				if _, err := tx.ExecContext(ctx, `INSERT INTO account_group_members (account_id, group_id)
					VALUES ($1,$2) ON CONFLICT (account_id, group_id) DO NOTHING`, accountID, groupID); err != nil {
					return err
				}
			}
		}
		nowExpr := "NOW()"
		if db.isSQLite() {
			nowExpr = "CURRENT_TIMESTAMP"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO profit_account_settings
			(account_id, settlement_group_id, settlement_group_name, assignment_source)
			VALUES ($1,$2,$3,$4) ON CONFLICT (account_id) DO UPDATE SET
			settlement_group_id = excluded.settlement_group_id,
			settlement_group_name = excluded.settlement_group_name,
			assignment_source = excluded.assignment_source, updated_at = `+nowExpr, accountID, groupID, groupName, assignmentSource); err != nil {
			return err
		}
		// 尚未进入日账本的历史日志与未来日志都使用已确认分组，避免再次出现待确认。
		if backfillUsageLogs {
			if _, err := tx.ExecContext(ctx, `UPDATE usage_logs SET settlement_group_id_snapshot = $1,
				settlement_group_name_snapshot = $2, settlement_assignment_source = 'confirmed_backfill'
				WHERE account_id = $3 AND COALESCE(settlement_group_id_snapshot, 0) = 0`, groupID, groupName, accountID); err != nil {
				return err
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT id, CAST(ledger_date AS TEXT), segment, api_key_id, api_key_name_snapshot,
			api_key_masked_snapshot, account_id, account_name_snapshot, account_deleted, channel, model,
			request_count, input_tokens, output_tokens, cached_tokens, reasoning_tokens, total_tokens,
			official_cost_usd_micros, source_first_log_id, source_last_log_id, source_hash, ledger_version
			FROM profit_daily_ledger WHERE account_id = $1 AND settlement_group_id = 0
			AND COALESCE(claimed_lineage_id, '') = '' ORDER BY id`, accountID)
		if err != nil {
			return err
		}
		pending := make([]profitLedgerMergeRow, 0)
		for rows.Next() {
			var row profitLedgerMergeRow
			if err := rows.Scan(&row.ID, &row.LedgerDate, &row.Segment, &row.APIKeyID, &row.APIKeyName,
				&row.APIKeyMasked, &row.AccountID, &row.AccountName, &row.AccountDeleted, &row.Channel,
				&row.Model, &row.RequestCount, &row.InputTokens, &row.OutputTokens, &row.CachedTokens,
				&row.ReasoningTokens, &row.TotalTokens, &row.OfficialUSDMicros, &row.SourceFirstLogID,
				&row.SourceLastLogID, &row.SourceHash, &row.LedgerVersion); err != nil {
				rows.Close()
				return err
			}
			pending = append(pending, row)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, row := range pending {
			if err := db.mergePendingProfitLedgerRow(ctx, tx, row, groupID, groupName); err != nil {
				return err
			}
		}
		return nil
	})
}

func (db *DB) mergePendingProfitLedgerRow(ctx context.Context, tx *sql.Tx, row profitLedgerMergeRow, groupID int64, groupName string) error {
	var targetID, targetSegment int64
	var claimed sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id, segment, claimed_lineage_id FROM profit_daily_ledger
		WHERE ledger_date = $1 AND api_key_id = $2 AND account_id = $3 AND model = $4 AND channel = $5
		AND settlement_group_id = $6 ORDER BY segment DESC LIMIT 1`, row.LedgerDate, row.APIKeyID,
		row.AccountID, row.Model, row.Channel, groupID).Scan(&targetID, &targetSegment, &claimed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	nowExpr := "NOW()"
	if db.isSQLite() {
		nowExpr = "CURRENT_TIMESTAMP"
	}
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `UPDATE profit_daily_ledger SET settlement_group_id = $1,
			settlement_group_name_snapshot = $2, assignment_source = 'confirmed_backfill',
			ledger_version = ledger_version + 1, updated_at = `+nowExpr+` WHERE id = $3`, groupID, groupName, row.ID)
		return err
	}
	if claimed.Valid && claimed.String != "" {
		targetSegment++
		_, err = tx.ExecContext(ctx, `INSERT INTO profit_daily_ledger (
			ledger_date, segment, api_key_id, api_key_name_snapshot, api_key_masked_snapshot,
			account_id, account_name_snapshot, account_deleted, channel, model,
			settlement_group_id, settlement_group_name_snapshot, assignment_source,
			request_count, input_tokens, output_tokens, cached_tokens, reasoning_tokens, total_tokens,
			official_cost_usd_micros, source_first_log_id, source_last_log_id, source_hash, ledger_version
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'confirmed_backfill',$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`,
			row.LedgerDate, targetSegment, row.APIKeyID, row.APIKeyName, row.APIKeyMasked,
			row.AccountID, row.AccountName, row.AccountDeleted, row.Channel, row.Model,
			groupID, groupName, row.RequestCount, row.InputTokens, row.OutputTokens, row.CachedTokens,
			row.ReasoningTokens, row.TotalTokens, row.OfficialUSDMicros, row.SourceFirstLogID,
			row.SourceLastLogID, row.SourceHash, row.LedgerVersion+1)
		if err != nil {
			return err
		}
	} else {
		var oldHash string
		if err := tx.QueryRowContext(ctx, `SELECT source_hash FROM profit_daily_ledger WHERE id = $1`, targetID).Scan(&oldHash); err != nil {
			return err
		}
		combined := sha256.Sum256([]byte(oldHash + "|" + row.SourceHash))
		_, err = tx.ExecContext(ctx, `UPDATE profit_daily_ledger SET
			request_count = request_count + $1, input_tokens = input_tokens + $2, output_tokens = output_tokens + $3,
			cached_tokens = cached_tokens + $4, reasoning_tokens = reasoning_tokens + $5, total_tokens = total_tokens + $6,
			official_cost_usd_micros = official_cost_usd_micros + $7,
			source_first_log_id = CASE WHEN source_first_log_id = 0 OR $8 < source_first_log_id THEN $8 ELSE source_first_log_id END,
			source_last_log_id = CASE WHEN $9 > source_last_log_id THEN $9 ELSE source_last_log_id END,
			source_hash = $10, ledger_version = ledger_version + 1, assignment_source = 'confirmed_backfill',
			updated_at = `+nowExpr+` WHERE id = $11`, row.RequestCount, row.InputTokens, row.OutputTokens,
			row.CachedTokens, row.ReasoningTokens, row.TotalTokens, row.OfficialUSDMicros,
			row.SourceFirstLogID, row.SourceLastLogID, hex.EncodeToString(combined[:]), targetID)
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM profit_daily_ledger WHERE id = $1`, row.ID)
	return err
}

type profitDashboardSourceRow struct {
	GroupID           int64
	GroupName         string
	APIKeyID          int64
	APIKeyName        string
	AccountID         int64
	AccountName       string
	AccountDeleted    bool
	Model             string
	RequestCount      int64
	InputTokens       int64
	OutputTokens      int64
	CachedTokens      int64
	ReasoningTokens   int64
	TotalTokens       int64
	OfficialUSDMicros int64
}

func (db *DB) GetProfitDashboard(ctx context.Context, startDate string, endDate string, ratioPPM int64) (ProfitDashboardResponse, error) {
	settings, err := db.GetProfitSettings(ctx)
	if err != nil {
		return ProfitDashboardResponse{}, err
	}
	if _, _, err := parseProfitDateRange(startDate, endDate); err != nil {
		return ProfitDashboardResponse{}, err
	}
	ratioPPM = normalizeProfitPPM(ratioPPM, settings.DefaultSettlementRatioPPM)
	result := ProfitDashboardResponse{StartDate: startDate, EndDate: endDate, Timezone: ProfitTimezone, SettlementRatioPPM: ratioPPM}
	if result.Ledger, err = db.GetProfitLedgerStatus(ctx); err != nil {
		return result, err
	}
	multipliers, err := db.profitMultiplierMap(ctx, settings.DefaultGroupMultiplierPPM)
	if err != nil {
		return result, err
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT settlement_group_id, MAX(settlement_group_name_snapshot),
		api_key_id, MAX(COALESCE(NULLIF(api_key_name_snapshot, ''), NULLIF(api_key_masked_snapshot, ''), '未命名 Key')),
		account_id, MAX(account_name_snapshot), MAX(CASE WHEN account_deleted THEN 1 ELSE 0 END), model,
		SUM(request_count), SUM(input_tokens), SUM(output_tokens), SUM(cached_tokens), SUM(reasoning_tokens),
		SUM(total_tokens), SUM(official_cost_usd_micros)
		FROM profit_daily_ledger WHERE ledger_date >= $1 AND ledger_date < $2
		GROUP BY settlement_group_id, api_key_id, account_id, model ORDER BY settlement_group_id, account_id`, startDate, endDate)
	if err != nil {
		return result, err
	}
	groupMap := make(map[string]*ProfitDashboardDimension)
	keyMap := make(map[string]*ProfitDashboardDimension)
	accountMap := make(map[string]*ProfitDashboardDimension)
	modelMap := make(map[string]*ProfitDashboardDimension)
	for rows.Next() {
		var source profitDashboardSourceRow
		var deletedInt int
		if err := rows.Scan(&source.GroupID, &source.GroupName, &source.APIKeyID, &source.APIKeyName,
			&source.AccountID, &source.AccountName, &deletedInt, &source.Model, &source.RequestCount,
			&source.InputTokens, &source.OutputTokens, &source.CachedTokens, &source.ReasoningTokens,
			&source.TotalTokens, &source.OfficialUSDMicros); err != nil {
			rows.Close()
			return result, err
		}
		source.AccountDeleted = deletedInt != 0
		multiplier := multipliers[source.GroupID]
		if multiplier <= 0 {
			multiplier = settings.DefaultGroupMultiplierPPM
		}
		addProfitDimension(groupMap, strconv.FormatInt(source.GroupID, 10), profitGroupDisplayName(source.GroupID, source.GroupName), source, ratioPPM, multiplier, source.GroupID == 0, false, true)
		addProfitDimension(keyMap, strconv.FormatInt(source.APIKeyID, 10), source.APIKeyName, source, ratioPPM, multiplier, false, false, false)
		addProfitDimension(accountMap, strconv.FormatInt(source.AccountID, 10), source.AccountName, source, ratioPPM, multiplier, false, source.AccountDeleted, false)
		addProfitDimension(modelMap, source.Model, source.Model, source, ratioPPM, multiplier, false, false, false)
		result.Overall.OfficialUSDMicros += source.OfficialUSDMicros
		result.Overall.RequestCount += source.RequestCount
		result.Overall.InputTokens += source.InputTokens
		result.Overall.OutputTokens += source.OutputTokens
		result.Overall.CachedTokens += source.CachedTokens
		result.Overall.ReasoningTokens += source.ReasoningTokens
		result.Overall.TotalTokens += source.TotalTokens
		settlement := profitMulDiv(source.OfficialUSDMicros, ratioPPM, ProfitScalePPM)
		revenue := profitMulDiv(settlement, multiplier, ProfitScalePPM)
		result.Overall.SettlementCNYMicros += settlement
		result.Overall.RevenueCNYMicros += revenue
		result.Overall.ProfitCNYMicros += revenue - settlement
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	if result.Overall.RevenueCNYMicros != 0 {
		margin := float64(result.Overall.ProfitCNYMicros) / float64(result.Overall.RevenueCNYMicros)
		result.Overall.Margin = &margin
	}
	result.Groups = sortedProfitDimensions(groupMap)
	result.APIKeys = sortedProfitDimensions(keyMap)
	result.Accounts = sortedProfitDimensions(accountMap)
	result.Models = sortedProfitDimensions(modelMap)
	return result, nil
}

func parseProfitDateRange(startDate string, endDate string) (time.Time, time.Time, error) {
	loc := profitLocation()
	start, err := time.ParseInLocation("2006-01-02", startDate, loc)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid start_date: %w", err)
	}
	end, err := time.ParseInLocation("2006-01-02", endDate, loc)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid end_date: %w", err)
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("end_date must be after start_date")
	}
	return start, end, nil
}

func (db *DB) profitMultiplierMap(ctx context.Context, fallback int64) (map[int64]int64, error) {
	result := map[int64]int64{0: fallback}
	rows, err := db.conn.QueryContext(ctx, `SELECT group_id, multiplier_ppm FROM profit_group_settings`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var groupID, multiplier int64
		if err := rows.Scan(&groupID, &multiplier); err != nil {
			rows.Close()
			return nil, err
		}
		result[groupID] = normalizeProfitPPM(multiplier, fallback)
	}
	return result, rows.Close()
}

func profitGroupDisplayName(groupID int64, name string) string {
	if groupID == 0 {
		return "待确认分组"
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Sprintf("历史分组 #%d", groupID)
	}
	return name
}

func addProfitDimension(target map[string]*ProfitDashboardDimension, id string, name string, source profitDashboardSourceRow, ratioPPM int64, multiplierPPM int64, pending bool, deleted bool, showMultiplier bool) {
	item := target[id]
	if item == nil {
		item = &ProfitDashboardDimension{ID: id, Name: name, Pending: pending, Deleted: deleted}
		if showMultiplier {
			item.MultiplierPPM = multiplierPPM
		}
		target[id] = item
	}
	item.Deleted = item.Deleted || deleted
	item.ProfitMoneySummary.OfficialUSDMicros += source.OfficialUSDMicros
	item.ProfitMoneySummary.RequestCount += source.RequestCount
	item.ProfitMoneySummary.InputTokens += source.InputTokens
	item.ProfitMoneySummary.OutputTokens += source.OutputTokens
	item.ProfitMoneySummary.CachedTokens += source.CachedTokens
	item.ProfitMoneySummary.ReasoningTokens += source.ReasoningTokens
	item.ProfitMoneySummary.TotalTokens += source.TotalTokens
	settlement := profitMulDiv(source.OfficialUSDMicros, ratioPPM, ProfitScalePPM)
	revenue := profitMulDiv(settlement, multiplierPPM, ProfitScalePPM)
	item.ProfitMoneySummary.SettlementCNYMicros += settlement
	item.ProfitMoneySummary.RevenueCNYMicros += revenue
	item.ProfitMoneySummary.ProfitCNYMicros += revenue - settlement
	if item.ProfitMoneySummary.RevenueCNYMicros != 0 {
		margin := float64(item.ProfitMoneySummary.ProfitCNYMicros) / float64(item.ProfitMoneySummary.RevenueCNYMicros)
		item.ProfitMoneySummary.Margin = &margin
	} else {
		item.ProfitMoneySummary.Margin = nil
	}
}

func sortedProfitDimensions(source map[string]*ProfitDashboardDimension) []ProfitDashboardDimension {
	result := make([]ProfitDashboardDimension, 0, len(source))
	for _, item := range source {
		result = append(result, *item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].RevenueCNYMicros != result[j].RevenueCNYMicros {
			return result[i].RevenueCNYMicros > result[j].RevenueCNYMicros
		}
		return result[i].Name < result[j].Name
	})
	return result
}

func (db *DB) CreateProfitSettlementDraft(ctx context.Context, startDate string, endDate string, ratioPPM int64, notes string) (ProfitSettlementDetail, error) {
	start, end, err := parseProfitDateRange(startDate, endDate)
	if err != nil {
		return ProfitSettlementDetail{}, err
	}
	settings, err := db.GetProfitSettings(ctx)
	if err != nil {
		return ProfitSettlementDetail{}, err
	}
	ratioPPM = normalizeProfitPPM(ratioPPM, settings.DefaultSettlementRatioPPM)
	runID, err := newProfitID("settlement")
	if err != nil {
		return ProfitSettlementDetail{}, err
	}
	lineageID, err := newProfitID("lineage")
	if err != nil {
		return ProfitSettlementDetail{}, err
	}
	run := ProfitSettlementRun{ID: runID, LineageID: lineageID, RevisionNo: 1, Status: "draft",
		StartDate: startDate, EndDate: endDate, SettlementRatioPPM: ratioPPM, Notes: strings.TrimSpace(notes)}
	err = db.withProfitLedgerTx(ctx, func(tx *sql.Tx, checkpoint int64) error {
		if err := db.ensureProfitLedgerRangeReady(ctx, tx, checkpoint, start, end); err != nil {
			return err
		}
		var pendingCount int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM profit_daily_ledger
			WHERE ledger_date >= $1 AND ledger_date < $2 AND settlement_group_id = 0
			AND COALESCE(claimed_lineage_id, '') = ''`, startDate, endDate).Scan(&pendingCount); err != nil {
			return err
		}
		if pendingCount > 0 {
			return ErrProfitPendingAssignment
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO profit_settlement_runs
			(id, lineage_id, revision_no, status, start_date, end_date, settlement_ratio_ppm, notes)
			VALUES ($1,$2,1,'draft',$3,$4,$5,$6)`, run.ID, run.LineageID, run.StartDate, run.EndDate,
			run.SettlementRatioPPM, run.Notes); err != nil {
			return err
		}
		return db.rebuildProfitSettlementDraft(ctx, tx, &run)
	})
	if err != nil {
		return ProfitSettlementDetail{}, err
	}
	return db.GetProfitSettlement(ctx, run.ID)
}

func (db *DB) ensureProfitLedgerRangeReady(ctx context.Context, tx *sql.Tx, checkpoint int64, start time.Time, end time.Time) error {
	var unaggregated int64
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_logs WHERE id > $1 AND created_at >= $2 AND created_at < $3`,
		checkpoint, db.timeArg(start.UTC()), db.timeArg(end.UTC())).Scan(&unaggregated)
	if err != nil {
		return err
	}
	if unaggregated > 0 {
		return ErrProfitLedgerBehind
	}
	return nil
}

func (db *DB) profitMultiplierMapTx(ctx context.Context, tx *sql.Tx, fallback int64) (map[int64]int64, error) {
	result := map[int64]int64{0: fallback}
	rows, err := tx.QueryContext(ctx, `SELECT group_id, multiplier_ppm FROM profit_group_settings`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var groupID, multiplier int64
		if err := rows.Scan(&groupID, &multiplier); err != nil {
			rows.Close()
			return nil, err
		}
		result[groupID] = normalizeProfitPPM(multiplier, fallback)
	}
	return result, rows.Close()
}

func (db *DB) rebuildProfitSettlementDraft(ctx context.Context, tx *sql.Tx, run *ProfitSettlementRun) error {
	settings, err := db.getProfitSettingsTx(ctx, tx)
	if err != nil {
		return err
	}
	multipliers, err := db.profitMultiplierMapTx(ctx, tx, settings.DefaultGroupMultiplierPPM)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM profit_settlement_items WHERE run_id = $1`, run.ID); err != nil {
		return err
	}
	query := `SELECT l.id, l.ledger_version, CAST(l.ledger_date AS TEXT), l.settlement_group_id,
		l.settlement_group_name_snapshot, l.api_key_id, l.api_key_name_snapshot, l.account_id,
		l.account_name_snapshot, l.account_deleted, l.model, l.channel, l.request_count, l.total_tokens,
		l.official_cost_usd_micros, l.source_first_log_id, l.source_last_log_id, l.source_hash
		FROM profit_daily_ledger l `
	args := []interface{}{run.StartDate, run.EndDate}
	if run.RevisionNo > 1 {
		query += `JOIN profit_ledger_claims c ON c.ledger_row_id = l.id AND c.lineage_id = $3
			WHERE l.ledger_date >= $1 AND l.ledger_date < $2 ORDER BY l.id`
		args = append(args, run.LineageID)
	} else {
		query += `WHERE l.ledger_date >= $1 AND l.ledger_date < $2
			AND l.settlement_group_id > 0 AND COALESCE(l.claimed_lineage_id, '') = '' ORDER BY l.id`
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	items := make([]ProfitSettlementItem, 0)
	for rows.Next() {
		var item ProfitSettlementItem
		if err := rows.Scan(&item.LedgerRowID, &item.LedgerVersion, &item.LedgerDate, &item.GroupID,
			&item.GroupName, &item.APIKeyID, &item.APIKeyName, &item.AccountID, &item.AccountName,
			&item.AccountDeleted, &item.Model, &item.Channel, &item.RequestCount, &item.TotalTokens,
			&item.OfficialUSDMicros, &item.SourceFirstLogID, &item.SourceLastLogID, &item.SourceHash); err != nil {
			rows.Close()
			return err
		}
		item.MultiplierPPM = multipliers[item.GroupID]
		if item.MultiplierPPM <= 0 {
			item.MultiplierPPM = settings.DefaultGroupMultiplierPPM
		}
		item.SettlementCNYMicros = profitMulDiv(item.OfficialUSDMicros, run.SettlementRatioPPM, ProfitScalePPM)
		item.RevenueCNYMicros = profitMulDiv(item.SettlementCNYMicros, item.MultiplierPPM, ProfitScalePPM)
		item.ProfitCNYMicros = item.RevenueCNYMicros - item.SettlementCNYMicros
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(items) == 0 {
		return ErrProfitSettlementEmpty
	}
	hasher := sha256.New()
	run.OfficialUSDMicros = 0
	run.SettlementCNYMicros = 0
	run.RevenueCNYMicros = 0
	run.ProfitCNYMicros = 0
	for _, item := range items {
		if _, err := tx.ExecContext(ctx, `INSERT INTO profit_settlement_items (
			run_id, ledger_row_id, ledger_version, ledger_date, group_id, group_name,
			api_key_id, api_key_name, account_id, account_name, account_deleted, model, channel,
			multiplier_ppm, request_count, total_tokens, official_cost_usd_micros,
			settlement_cost_cny_micros, revenue_cny_micros, profit_cny_micros,
			source_first_log_id, source_last_log_id, source_hash
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`,
			run.ID, item.LedgerRowID, item.LedgerVersion, item.LedgerDate, item.GroupID, item.GroupName,
			item.APIKeyID, item.APIKeyName, item.AccountID, item.AccountName, item.AccountDeleted,
			item.Model, item.Channel, item.MultiplierPPM, item.RequestCount, item.TotalTokens,
			item.OfficialUSDMicros, item.SettlementCNYMicros, item.RevenueCNYMicros, item.ProfitCNYMicros,
			item.SourceFirstLogID, item.SourceLastLogID, item.SourceHash); err != nil {
			return err
		}
		fmt.Fprintf(hasher, "%d|%d|%s|%d|%d|%d|%d|%s\n", item.LedgerRowID, item.LedgerVersion,
			item.SourceHash, item.MultiplierPPM, item.OfficialUSDMicros, item.SettlementCNYMicros,
			item.RevenueCNYMicros, item.LedgerDate)
		run.OfficialUSDMicros += item.OfficialUSDMicros
		run.SettlementCNYMicros += item.SettlementCNYMicros
		run.RevenueCNYMicros += item.RevenueCNYMicros
		run.ProfitCNYMicros += item.ProfitCNYMicros
	}
	run.SourceManifestHash = hex.EncodeToString(hasher.Sum(nil))
	_, err = tx.ExecContext(ctx, `UPDATE profit_settlement_runs SET settlement_ratio_ppm = $1, notes = $2,
		official_cost_usd_micros = $3, settlement_cost_cny_micros = $4, revenue_cny_micros = $5,
		profit_cny_micros = $6, source_manifest_hash = $7, created_at = created_at
		WHERE id = $8 AND status = 'draft'`, run.SettlementRatioPPM, run.Notes, run.OfficialUSDMicros,
		run.SettlementCNYMicros, run.RevenueCNYMicros, run.ProfitCNYMicros, run.SourceManifestHash, run.ID)
	return err
}

func (db *DB) getProfitSettingsTx(ctx context.Context, tx *sql.Tx) (ProfitSettings, error) {
	var result ProfitSettings
	err := tx.QueryRowContext(ctx, `SELECT enabled, default_settlement_ratio_ppm,
		default_group_multiplier_ppm, timezone FROM profit_settings WHERE id = 1`).
		Scan(&result.Enabled, &result.DefaultSettlementRatioPPM, &result.DefaultGroupMultiplierPPM, &result.Timezone)
	result.DefaultSettlementRatioPPM = normalizeProfitPPM(result.DefaultSettlementRatioPPM, DefaultProfitSettlementRatioPPM)
	result.DefaultGroupMultiplierPPM = normalizeProfitPPM(result.DefaultGroupMultiplierPPM, DefaultProfitGroupMultiplierPPM)
	return result, err
}

func (db *DB) UpdateProfitSettlementDraft(ctx context.Context, runID string, ratioPPM int64, notes string) (ProfitSettlementDetail, error) {
	err := db.withProfitLedgerTx(ctx, func(tx *sql.Tx, checkpoint int64) error {
		run, err := scanProfitSettlementRun(tx.QueryRowContext(ctx, `SELECT id, lineage_id, revision_no,
			COALESCE(supersedes_id, ''), status, CAST(start_date AS TEXT), CAST(end_date AS TEXT), settlement_ratio_ppm, notes,
			official_cost_usd_micros, settlement_cost_cny_micros, revenue_cny_micros, profit_cny_micros,
			source_manifest_hash, created_at, confirmed_at FROM profit_settlement_runs WHERE id = $1`, runID))
		if err != nil {
			return err
		}
		if run.Status != "draft" {
			return fmt.Errorf("only draft settlements can be updated")
		}
		start, end, err := parseProfitDateRange(run.StartDate, run.EndDate)
		if err != nil {
			return err
		}
		if err := db.ensureProfitLedgerRangeReady(ctx, tx, checkpoint, start, end); err != nil {
			return err
		}
		run.SettlementRatioPPM = normalizeProfitPPM(ratioPPM, run.SettlementRatioPPM)
		run.Notes = strings.TrimSpace(notes)
		return db.rebuildProfitSettlementDraft(ctx, tx, &run)
	})
	if err != nil {
		return ProfitSettlementDetail{}, err
	}
	return db.GetProfitSettlement(ctx, runID)
}

func (db *DB) ConfirmProfitSettlement(ctx context.Context, runID string) (ProfitSettlementDetail, error) {
	err := db.withProfitLedgerTx(ctx, func(tx *sql.Tx, checkpoint int64) error {
		run, err := scanProfitSettlementRun(tx.QueryRowContext(ctx, `SELECT id, lineage_id, revision_no,
			COALESCE(supersedes_id, ''), status, CAST(start_date AS TEXT), CAST(end_date AS TEXT), settlement_ratio_ppm, notes,
			official_cost_usd_micros, settlement_cost_cny_micros, revenue_cny_micros, profit_cny_micros,
			source_manifest_hash, created_at, confirmed_at FROM profit_settlement_runs WHERE id = $1`, runID))
		if err != nil {
			return err
		}
		if run.Status != "draft" {
			return fmt.Errorf("settlement is not a draft")
		}
		start, end, err := parseProfitDateRange(run.StartDate, run.EndDate)
		if err != nil {
			return err
		}
		if err := db.ensureProfitLedgerRangeReady(ctx, tx, checkpoint, start, end); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT i.ledger_row_id, i.ledger_version, l.ledger_version,
			COALESCE(l.claimed_lineage_id, ''), COALESCE(c.lineage_id, '')
			FROM profit_settlement_items i JOIN profit_daily_ledger l ON l.id = i.ledger_row_id
			LEFT JOIN profit_ledger_claims c ON c.ledger_row_id = i.ledger_row_id WHERE i.run_id = $1 ORDER BY i.ledger_row_id`, runID)
		if err != nil {
			return err
		}
		ledgerIDs := make([]int64, 0)
		for rows.Next() {
			var ledgerID, itemVersion, ledgerVersion int64
			var ledgerLineage, claimLineage string
			if err := rows.Scan(&ledgerID, &itemVersion, &ledgerVersion, &ledgerLineage, &claimLineage); err != nil {
				rows.Close()
				return err
			}
			if itemVersion != ledgerVersion || ledgerLineage != "" && ledgerLineage != run.LineageID || claimLineage != "" && claimLineage != run.LineageID {
				rows.Close()
				return ErrProfitSettlementConflict
			}
			ledgerIDs = append(ledgerIDs, ledgerID)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(ledgerIDs) == 0 {
			return ErrProfitSettlementEmpty
		}
		if run.RevisionNo == 1 {
			var missingRows, pendingRows int64
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM profit_daily_ledger l
				LEFT JOIN profit_settlement_items i ON i.run_id = $1 AND i.ledger_row_id = l.id
				WHERE l.ledger_date >= $2 AND l.ledger_date < $3 AND l.settlement_group_id > 0
				AND COALESCE(l.claimed_lineage_id, '') = '' AND i.ledger_row_id IS NULL`, run.ID, run.StartDate, run.EndDate).Scan(&missingRows); err != nil {
				return err
			}
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM profit_daily_ledger
				WHERE ledger_date >= $1 AND ledger_date < $2 AND settlement_group_id = 0
				AND COALESCE(claimed_lineage_id, '') = ''`, run.StartDate, run.EndDate).Scan(&pendingRows); err != nil {
				return err
			}
			if missingRows > 0 || pendingRows > 0 {
				return ErrProfitSettlementConflict
			}
		}
		for _, ledgerID := range ledgerIDs {
			if run.RevisionNo == 1 {
				if _, err := tx.ExecContext(ctx, `INSERT INTO profit_ledger_claims (ledger_row_id, lineage_id, run_id)
					VALUES ($1,$2,$3)`, ledgerID, run.LineageID, run.ID); err != nil {
					return ErrProfitSettlementConflict
				}
			} else {
				res, err := tx.ExecContext(ctx, `UPDATE profit_ledger_claims SET run_id = $1
					WHERE ledger_row_id = $2 AND lineage_id = $3`, run.ID, ledgerID, run.LineageID)
				if err != nil {
					return err
				}
				if affected, _ := res.RowsAffected(); affected != 1 {
					return ErrProfitSettlementConflict
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE profit_daily_ledger SET claimed_lineage_id = $1 WHERE id = $2`, run.LineageID, ledgerID); err != nil {
				return err
			}
		}
		nowExpr := "NOW()"
		if db.isSQLite() {
			nowExpr = "CURRENT_TIMESTAMP"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE profit_settlement_runs SET status = 'superseded'
			WHERE lineage_id = $1 AND status = 'confirmed' AND id <> $2`, run.LineageID, run.ID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE profit_settlement_runs SET status = 'confirmed', confirmed_at = `+nowExpr+`
			WHERE id = $1 AND status = 'draft'`, run.ID)
		if err != nil {
			return err
		}
		if affected, _ := res.RowsAffected(); affected != 1 {
			return ErrProfitSettlementConflict
		}
		return nil
	})
	if err != nil {
		return ProfitSettlementDetail{}, err
	}
	return db.GetProfitSettlement(ctx, runID)
}

func (db *DB) CreateProfitSettlementRevision(ctx context.Context, confirmedRunID string, ratioPPM int64, notes string) (ProfitSettlementDetail, error) {
	newRunID, err := newProfitID("settlement")
	if err != nil {
		return ProfitSettlementDetail{}, err
	}
	var newRun ProfitSettlementRun
	err = db.withProfitLedgerTx(ctx, func(tx *sql.Tx, _ int64) error {
		base, err := scanProfitSettlementRun(tx.QueryRowContext(ctx, `SELECT id, lineage_id, revision_no,
			COALESCE(supersedes_id, ''), status, CAST(start_date AS TEXT), CAST(end_date AS TEXT), settlement_ratio_ppm, notes,
			official_cost_usd_micros, settlement_cost_cny_micros, revenue_cny_micros, profit_cny_micros,
			source_manifest_hash, created_at, confirmed_at FROM profit_settlement_runs WHERE id = $1`, confirmedRunID))
		if err != nil {
			return err
		}
		if base.Status != "confirmed" {
			return fmt.Errorf("only the current confirmed settlement can be revised")
		}
		var existingDraft int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM profit_settlement_runs WHERE lineage_id = $1 AND status = 'draft'`, base.LineageID).Scan(&existingDraft); err != nil {
			return err
		}
		if existingDraft > 0 {
			return fmt.Errorf("a draft revision already exists")
		}
		newRun = ProfitSettlementRun{ID: newRunID, LineageID: base.LineageID, RevisionNo: base.RevisionNo + 1,
			SupersedesID: base.ID, Status: "draft", StartDate: base.StartDate, EndDate: base.EndDate,
			SettlementRatioPPM: normalizeProfitPPM(ratioPPM, base.SettlementRatioPPM), Notes: strings.TrimSpace(notes)}
		if _, err := tx.ExecContext(ctx, `INSERT INTO profit_settlement_runs
			(id, lineage_id, revision_no, supersedes_id, status, start_date, end_date, settlement_ratio_ppm, notes)
			VALUES ($1,$2,$3,$4,'draft',$5,$6,$7,$8)`, newRun.ID, newRun.LineageID, newRun.RevisionNo,
			newRun.SupersedesID, newRun.StartDate, newRun.EndDate, newRun.SettlementRatioPPM, newRun.Notes); err != nil {
			return err
		}
		return db.rebuildProfitSettlementDraft(ctx, tx, &newRun)
	})
	if err != nil {
		return ProfitSettlementDetail{}, err
	}
	return db.GetProfitSettlement(ctx, newRun.ID)
}

type profitRowScanner interface {
	Scan(dest ...interface{}) error
}

func scanProfitSettlementRun(scanner profitRowScanner) (ProfitSettlementRun, error) {
	var run ProfitSettlementRun
	var createdRaw interface{}
	var confirmedRaw interface{}
	err := scanner.Scan(&run.ID, &run.LineageID, &run.RevisionNo, &run.SupersedesID, &run.Status,
		&run.StartDate, &run.EndDate, &run.SettlementRatioPPM, &run.Notes, &run.OfficialUSDMicros,
		&run.SettlementCNYMicros, &run.RevenueCNYMicros, &run.ProfitCNYMicros, &run.SourceManifestHash,
		&createdRaw, &confirmedRaw)
	if err != nil {
		return run, err
	}
	if created, parseErr := parseDBTimeValue(createdRaw); parseErr == nil {
		run.CreatedAt = created.Format(time.RFC3339)
	}
	if confirmedRaw != nil {
		if confirmed, parseErr := parseDBTimeValue(confirmedRaw); parseErr == nil {
			run.ConfirmedAt = confirmed.Format(time.RFC3339)
		}
	}
	return run, nil
}

func (db *DB) ListProfitSettlements(ctx context.Context, limit int) ([]ProfitSettlementRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT id, lineage_id, revision_no, COALESCE(supersedes_id, ''),
		status, CAST(start_date AS TEXT), CAST(end_date AS TEXT), settlement_ratio_ppm, notes, official_cost_usd_micros,
		settlement_cost_cny_micros, revenue_cny_micros, profit_cny_micros, source_manifest_hash,
		created_at, confirmed_at FROM profit_settlement_runs ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	result := make([]ProfitSettlementRun, 0)
	for rows.Next() {
		run, err := scanProfitSettlementRun(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		result = append(result, run)
	}
	return result, rows.Close()
}

func (db *DB) GetProfitSettlement(ctx context.Context, runID string) (ProfitSettlementDetail, error) {
	run, err := scanProfitSettlementRun(db.conn.QueryRowContext(ctx, `SELECT id, lineage_id, revision_no,
		COALESCE(supersedes_id, ''), status, CAST(start_date AS TEXT), CAST(end_date AS TEXT), settlement_ratio_ppm, notes,
		official_cost_usd_micros, settlement_cost_cny_micros, revenue_cny_micros, profit_cny_micros,
		source_manifest_hash, created_at, confirmed_at FROM profit_settlement_runs WHERE id = $1`, runID))
	if err != nil {
		return ProfitSettlementDetail{}, err
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT ledger_row_id, ledger_version, CAST(ledger_date AS TEXT), group_id,
		group_name, api_key_id, api_key_name, account_id, account_name, account_deleted, model, channel,
		multiplier_ppm, request_count, total_tokens, official_cost_usd_micros, settlement_cost_cny_micros,
		revenue_cny_micros, profit_cny_micros, source_first_log_id, source_last_log_id, source_hash
		FROM profit_settlement_items WHERE run_id = $1 ORDER BY ledger_date, group_id, account_id, ledger_row_id`, runID)
	if err != nil {
		return ProfitSettlementDetail{}, err
	}
	items := make([]ProfitSettlementItem, 0)
	for rows.Next() {
		var item ProfitSettlementItem
		if err := rows.Scan(&item.LedgerRowID, &item.LedgerVersion, &item.LedgerDate, &item.GroupID,
			&item.GroupName, &item.APIKeyID, &item.APIKeyName, &item.AccountID, &item.AccountName,
			&item.AccountDeleted, &item.Model, &item.Channel, &item.MultiplierPPM, &item.RequestCount,
			&item.TotalTokens, &item.OfficialUSDMicros, &item.SettlementCNYMicros, &item.RevenueCNYMicros,
			&item.ProfitCNYMicros, &item.SourceFirstLogID, &item.SourceLastLogID, &item.SourceHash); err != nil {
			rows.Close()
			return ProfitSettlementDetail{}, err
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return ProfitSettlementDetail{}, err
	}
	return ProfitSettlementDetail{Run: run, Items: items}, nil
}
