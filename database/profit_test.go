package database

import (
	"context"
	"testing"
	"time"
)

func newProfitTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := New("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func insertProfitTestAccountAndGroup(t *testing.T, db *DB, deleted bool) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	groupResult, err := db.conn.ExecContext(ctx, `INSERT INTO account_groups (name, channel) VALUES ('结算一组', 'codex')`)
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	groupID, _ := groupResult.LastInsertId()
	status := "active"
	var deletedAt interface{}
	if deleted {
		status = "deleted"
		deletedAt = time.Now()
	}
	accountResult, err := db.conn.ExecContext(ctx, `INSERT INTO accounts
		(name, platform, type, credentials, status, deleted_at) VALUES ('测试账号', 'openai', 'oauth', '{}', $1, $2)`, status, deletedAt)
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	accountID, _ := accountResult.LastInsertId()
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO account_group_members (account_id, group_id) VALUES ($1,$2)`, accountID, groupID); err != nil {
		t.Fatalf("insert membership: %v", err)
	}
	return accountID, groupID
}

func profitTestDateRange() (string, string) {
	now := time.Now().In(profitLocation())
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, profitLocation())
	return start.Format("2006-01-02"), start.AddDate(0, 0, 1).Format("2006-01-02")
}

func TestProfitSnapshotDashboardAndSettlementRevision(t *testing.T) {
	db := newProfitTestDB(t)
	ctx := context.Background()
	accountID, groupID := insertProfitTestAccountAndGroup(t, db, false)
	if err := db.AssignProfitSettlementGroup(ctx, accountID, groupID); err != nil {
		t.Fatalf("assign settlement group: %v", err)
	}
	if _, err := db.UpdateProfitGroupMultiplier(ctx, groupID, 1_200_000); err != nil {
		t.Fatalf("update multiplier: %v", err)
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin snapshot tx: %v", err)
	}
	snapshots, err := db.populateProfitSettlementSnapshots(ctx, tx, []usageLogEntry{{AccountID: accountID}})
	_ = tx.Rollback()
	if err != nil {
		t.Fatalf("populate snapshot: %v", err)
	}
	if snapshots[0].SettlementGroupIDSnapshot != groupID || snapshots[0].SettlementAssignmentSource != "confirmed" {
		t.Fatalf("snapshot = (%d,%q), want (%d,confirmed)", snapshots[0].SettlementGroupIDSnapshot, snapshots[0].SettlementAssignmentSource, groupID)
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO usage_logs
		(account_id, channel, model, effective_model, status_code, input_tokens, output_tokens, total_tokens,
		 account_billed, settlement_group_id_snapshot, settlement_group_name_snapshot, settlement_assignment_source, created_at)
		VALUES ($1, 'codex', 'gpt-5.4', 'gpt-5.4', 200, 1000, 500, 1500, 3.0, $2, '结算一组', 'confirmed', CURRENT_TIMESTAMP)`, accountID, groupID); err != nil {
		t.Fatalf("insert assigned usage: %v", err)
	}
	refresh, err := db.RefreshProfitDailyLedger(ctx, 100)
	if err != nil {
		t.Fatalf("refresh ledger: %v", err)
	}
	if !refresh.CaughtUp || refresh.AggregatedLogs != 1 {
		t.Fatalf("unexpected refresh: %+v", refresh)
	}
	startDate, endDate := profitTestDateRange()
	dashboard, err := db.GetProfitDashboard(ctx, startDate, endDate, ProfitScalePPM)
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	if dashboard.Overall.OfficialUSDMicros <= 0 {
		t.Fatalf("official cost should be positive: %+v", dashboard.Overall)
	}
	wantRevenue := profitMulDiv(dashboard.Overall.OfficialUSDMicros, 1_200_000, ProfitScalePPM)
	if dashboard.Overall.RevenueCNYMicros != wantRevenue {
		t.Fatalf("revenue = %d, want %d", dashboard.Overall.RevenueCNYMicros, wantRevenue)
	}
	draft, err := db.CreateProfitSettlementDraft(ctx, startDate, endDate, ProfitScalePPM, "首版")
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	confirmed, err := db.ConfirmProfitSettlement(ctx, draft.Run.ID)
	if err != nil {
		t.Fatalf("confirm settlement: %v", err)
	}
	if confirmed.Run.Status != "confirmed" {
		t.Fatalf("status = %s", confirmed.Run.Status)
	}
	revision, err := db.CreateProfitSettlementRevision(ctx, confirmed.Run.ID, 2_000_000, "比例修订")
	if err != nil {
		t.Fatalf("create revision: %v", err)
	}
	if revision.Run.RevisionNo != 2 || revision.Run.LineageID != confirmed.Run.LineageID {
		t.Fatalf("unexpected revision: %+v", revision.Run)
	}
	if _, err := db.ConfirmProfitSettlement(ctx, revision.Run.ID); err != nil {
		t.Fatalf("confirm revision: %v", err)
	}
	oldRun, err := db.GetProfitSettlement(ctx, confirmed.Run.ID)
	if err != nil {
		t.Fatalf("get old settlement: %v", err)
	}
	if oldRun.Run.Status != "superseded" {
		t.Fatalf("old status = %s, want superseded", oldRun.Run.Status)
	}
}

func TestProfitDeletedAccountAutomaticallyInheritsOriginalGroup(t *testing.T) {
	db := newProfitTestDB(t)
	ctx := context.Background()
	accountID, groupID := insertProfitTestAccountAndGroup(t, db, true)
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO usage_logs
		(account_id, channel, model, effective_model, status_code, total_tokens, account_billed,
		 settlement_group_id_snapshot, settlement_group_name_snapshot, settlement_assignment_source, created_at)
		VALUES ($1, 'codex', 'gpt-5.4', 'gpt-5.4', 200, 1000, 2.5, 0, '', 'pending', CURRENT_TIMESTAMP)`, accountID); err != nil {
		t.Fatalf("insert pending usage: %v", err)
	}
	if _, err := db.RefreshProfitDailyLedger(ctx, 100); err != nil {
		t.Fatalf("refresh ledger: %v", err)
	}
	pending, err := db.ListProfitPendingAccounts(ctx)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("single original group should be inherited automatically: %+v", pending)
	}
	var ledgerGroupID int64
	var deleted bool
	var source string
	if err := db.conn.QueryRowContext(ctx, `SELECT settlement_group_id, account_deleted, assignment_source
		FROM profit_daily_ledger WHERE account_id = $1`, accountID).Scan(&ledgerGroupID, &deleted, &source); err != nil {
		t.Fatalf("read backfilled ledger: %v", err)
	}
	if ledgerGroupID != groupID || !deleted || source != "inherited" {
		t.Fatalf("inherited ledger = (%d,%v,%q)", ledgerGroupID, deleted, source)
	}
}

func TestProfitAccountWithMultipleOriginalGroupsRemainsPending(t *testing.T) {
	db := newProfitTestDB(t)
	ctx := context.Background()
	accountID, _ := insertProfitTestAccountAndGroup(t, db, false)
	secondGroup, err := db.conn.ExecContext(ctx, `INSERT INTO account_groups (name, channel) VALUES ('结算二组', 'codex')`)
	if err != nil {
		t.Fatalf("insert second group: %v", err)
	}
	secondGroupID, _ := secondGroup.LastInsertId()
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO account_group_members (account_id, group_id) VALUES ($1,$2)`, accountID, secondGroupID); err != nil {
		t.Fatalf("insert second membership: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO usage_logs
		(account_id, channel, model, effective_model, status_code, total_tokens, account_billed,
		 settlement_group_id_snapshot, settlement_group_name_snapshot, settlement_assignment_source, created_at)
		VALUES ($1, 'codex', 'gpt-5.4', 'gpt-5.4', 200, 1000, 2.5, 0, '', 'pending', CURRENT_TIMESTAMP)`, accountID); err != nil {
		t.Fatalf("insert pending usage: %v", err)
	}
	if _, err := db.RefreshProfitDailyLedger(ctx, 100); err != nil {
		t.Fatalf("refresh ledger: %v", err)
	}
	pending, err := db.ListProfitPendingAccounts(ctx)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 || len(pending[0].OperationalGroups) != 2 {
		t.Fatalf("multiple original groups must remain explicit: %+v", pending)
	}
}

func TestProfitDeletedAccountManualAssignmentRestoresTokenUsageGroup(t *testing.T) {
	db := newProfitTestDB(t)
	ctx := context.Background()
	accountID, groupID := insertProfitTestAccountAndGroup(t, db, true)
	if _, err := db.conn.ExecContext(ctx, `DELETE FROM account_group_members WHERE account_id = $1`, accountID); err != nil {
		t.Fatalf("remove retained membership: %v", err)
	}
	keyID, err := db.InsertAPIKey(ctx, "profit-history-key", "sk-profit-history-key-1234567890")
	if err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO usage_logs
		(api_key_id, account_id, channel, model, effective_model, status_code, total_tokens, account_billed,
		 settlement_group_id_snapshot, settlement_group_name_snapshot, settlement_assignment_source, created_at)
		VALUES ($1, $2, 'codex', 'gpt-5.4', 'gpt-5.4', 200, 321, 1.25, 0, '', 'pending', CURRENT_TIMESTAMP)`, keyID, accountID); err != nil {
		t.Fatalf("insert historical usage: %v", err)
	}
	if err := db.AssignProfitSettlementGroup(ctx, accountID, groupID); err != nil {
		t.Fatalf("assign settlement group: %v", err)
	}
	groupIDs, err := db.GetAccountGroupIDs(ctx, accountID)
	if err != nil {
		t.Fatalf("get restored groups: %v", err)
	}
	if len(groupIDs) != 1 || groupIDs[0] != groupID {
		t.Fatalf("restored groups = %v, want [%d]", groupIDs, groupID)
	}
	items, err := db.ListAPIKeyAccountStats(ctx, keyID, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("list api key account stats: %v", err)
	}
	if len(items) != 1 || len(items[0].Groups) != 1 || items[0].Groups[0].ID != groupID {
		t.Fatalf("token usage groups = %+v, want restored group %d", items, groupID)
	}
}

func TestProfitSQLiteMigrationAvoidsBlockingUsageLogSnapshotIndex(t *testing.T) {
	db := newProfitTestDB(t)
	var count int
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_usage_logs_profit_group_created'`).Scan(&count); err != nil {
		t.Fatalf("query profit usage index: %v", err)
	}
	if count != 0 {
		t.Fatalf("blocking profit usage index count = %d, want 0", count)
	}
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_profit_daily_ledger_upsert'`).Scan(&count); err != nil {
		t.Fatalf("query profit upsert index: %v", err)
	}
	if count != 1 {
		t.Fatalf("profit upsert index count = %d, want 1", count)
	}
}
