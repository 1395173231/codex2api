package database

import (
	"context"
	"errors"
	"fmt"
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

func addSecondProfitTestGroup(t *testing.T, db *DB, accountID int64) int64 {
	t.Helper()
	result, err := db.conn.ExecContext(context.Background(), `INSERT INTO account_groups (name, channel)
		VALUES ($1, 'codex')`, fmt.Sprintf("结算附加组-%d", accountID))
	if err != nil {
		t.Fatalf("insert second group: %v", err)
	}
	groupID, _ := result.LastInsertId()
	if _, err := db.conn.ExecContext(context.Background(), `INSERT INTO account_group_members (account_id, group_id)
		VALUES ($1,$2)`, accountID, groupID); err != nil {
		t.Fatalf("insert second membership: %v", err)
	}
	return groupID
}

func insertPendingProfitTestUsage(t *testing.T, db *DB, accountID int64, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		if _, err := db.conn.ExecContext(context.Background(), `INSERT INTO usage_logs
			(account_id, channel, model, effective_model, status_code, total_tokens, account_billed,
			 settlement_group_id_snapshot, settlement_group_name_snapshot, settlement_assignment_source, created_at)
			VALUES ($1, 'codex', $2, $2, 200, 1000, 2.5, 0, '', 'pending', CURRENT_TIMESTAMP)`,
			accountID, fmt.Sprintf("gpt-pending-%d", i)); err != nil {
			t.Fatalf("insert pending usage %d: %v", i, err)
		}
	}
	if _, err := db.RefreshProfitDailyLedger(context.Background(), MaxProfitLedgerRefreshLimit); err != nil {
		t.Fatalf("refresh pending ledger: %v", err)
	}
}

func TestIgnoreProfitPendingAccountHidesPhysicallyMissingDeletedAccount(t *testing.T) {
	db := newProfitTestDB(t)
	ctx := context.Background()
	accountID, _ := insertProfitTestAccountAndGroup(t, db, true)
	addSecondProfitTestGroup(t, db, accountID)
	insertPendingProfitTestUsage(t, db, accountID, 1)
	if err := db.PurgeAccount(ctx, accountID); err != nil {
		t.Fatalf("purge account shell: %v", err)
	}
	pending, err := db.ListProfitPendingAccounts(ctx)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 || !pending[0].Deleted {
		t.Fatalf("physically missing account should be marked deleted: %+v", pending)
	}
	if err := db.IgnoreProfitPendingAccount(ctx, accountID); err != nil {
		t.Fatalf("ignore pending account: %v", err)
	}
	pending, err = db.ListProfitPendingAccounts(ctx)
	if err != nil {
		t.Fatalf("list pending after ignore: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("ignored account remains pending: %+v", pending)
	}
	var usageCount, ledgerCount, ignoredCount int
	_ = db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_logs WHERE account_id = $1`, accountID).Scan(&usageCount)
	_ = db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM profit_daily_ledger WHERE account_id = $1`, accountID).Scan(&ledgerCount)
	_ = db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM profit_ignored_accounts WHERE account_id = $1`, accountID).Scan(&ignoredCount)
	if usageCount != 1 || ledgerCount != 1 || ignoredCount != 1 {
		t.Fatalf("ignore must retain data: usage=%d ledger=%d marker=%d", usageCount, ledgerCount, ignoredCount)
	}
	startDate, endDate := profitTestDateRange()
	dashboard, err := db.GetProfitDashboard(ctx, startDate, endDate, ProfitScalePPM)
	if err != nil {
		t.Fatalf("dashboard after ignore: %v", err)
	}
	if dashboard.Overall.RequestCount != 0 || dashboard.Overall.OfficialUSDMicros != 0 {
		t.Fatalf("ignored account remains in dashboard: %+v", dashboard.Overall)
	}
	if _, err := db.CreateProfitSettlementDraft(ctx, startDate, endDate, ProfitScalePPM, "ignored-only"); !errors.Is(err, ErrProfitSettlementEmpty) {
		t.Fatalf("ignored pending row should not block settlement, got %v", err)
	}
}

func TestIgnoreProfitPendingAccountRejectsActiveAccount(t *testing.T) {
	db := newProfitTestDB(t)
	ctx := context.Background()
	accountID, _ := insertProfitTestAccountAndGroup(t, db, false)
	addSecondProfitTestGroup(t, db, accountID)
	insertPendingProfitTestUsage(t, db, accountID, 1)
	if err := db.IgnoreProfitPendingAccount(ctx, accountID); !errors.Is(err, ErrProfitAccountNotDeleted) {
		t.Fatalf("ignore active account error = %v, want ErrProfitAccountNotDeleted", err)
	}
}

func TestPurgeProfitPendingAccountDataCleansRelatedRowsInBatches(t *testing.T) {
	db := newProfitTestDB(t)
	ctx := context.Background()
	accountID, groupID := insertProfitTestAccountAndGroup(t, db, true)
	addSecondProfitTestGroup(t, db, accountID)
	insertPendingProfitTestUsage(t, db, accountID, 5)

	keyID, err := db.InsertAPIKey(ctx, "cleanup-key", "sk-profit-cleanup-1234567890")
	if err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO api_key_scope_counters
		(api_key_id, scope_type, scope_id, used_cost, used_tokens, used_requests)
		VALUES ($1, 'account', $2, 12.5, 5000, 5)`, keyID, accountID); err != nil {
		t.Fatalf("insert account counter: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO account_model_cooldowns
		(account_id, model, reset_at) VALUES ($1, 'gpt-5.4', CURRENT_TIMESTAMP)`, accountID); err != nil {
		t.Fatalf("insert cooldown: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO account_events (account_id, event_type, source)
			VALUES ($1, 'status', 'test')`, accountID); err != nil {
			t.Fatalf("insert account event: %v", err)
		}
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO prompt_policy_incidents (incident_id, account_id)
			VALUES ($1,$2)`, fmt.Sprintf("cleanup-incident-%d", i), accountID); err != nil {
			t.Fatalf("insert prompt incident: %v", err)
		}
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO prompt_rule_candidate_evidence
			(candidate_id, source_kind, source_ref_hash, prompt_policy_incident_id)
			VALUES ($1, 'test', $2, $3)`, i+1, fmt.Sprintf("cleanup-hash-%d", i), fmt.Sprintf("cleanup-incident-%d", i)); err != nil {
			t.Fatalf("insert prompt evidence: %v", err)
		}
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO prompt_risk_events
			(source_type, source_id, subject_type, subject_key, event_kind, account_id)
			VALUES ('test',$1,'account',$2,'risk',$3)`, fmt.Sprintf("source-%d", i), fmt.Sprintf("account-%d-%d", accountID, i), accountID); err != nil {
			t.Fatalf("insert prompt risk event: %v", err)
		}
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO profit_account_settings
		(account_id, settlement_group_id, settlement_group_name, assignment_source)
		VALUES ($1,$2,'结算一组','confirmed')`, accountID, groupID); err != nil {
		t.Fatalf("insert profit account settings: %v", err)
	}

	cleanup, err := db.purgeProfitPendingAccountData(ctx, accountID, 2)
	if err != nil {
		t.Fatalf("purge profit account data: %v", err)
	}
	if cleanup.UsageLogs != 5 || cleanup.AccountEvents != 5 || cleanup.PromptPolicyIncidents != 5 || cleanup.PromptRiskEvents != 5 {
		t.Fatalf("unexpected cleanup counts: %+v", cleanup)
	}
	for table, condition := range map[string]string{
		"accounts":                "id",
		"usage_logs":              "account_id",
		"profit_daily_ledger":     "account_id",
		"profit_account_settings": "account_id",
		"account_group_members":   "account_id",
		"account_model_cooldowns": "account_id",
		"account_events":          "account_id",
		"prompt_policy_incidents": "account_id",
		"prompt_risk_events":      "account_id",
	} {
		var count int
		if err := db.conn.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s = $1`, table, condition), accountID).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s rows remain: %d", table, count)
		}
	}
	var evidenceCount int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_rule_candidate_evidence
		WHERE prompt_policy_incident_id LIKE 'cleanup-incident-%'`).Scan(&evidenceCount); err != nil {
		t.Fatalf("count prompt evidence: %v", err)
	}
	if evidenceCount != 0 {
		t.Fatalf("prompt evidence rows remain: %d", evidenceCount)
	}
	var counterCount int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_key_scope_counters
		WHERE scope_type = 'account' AND scope_id = $1`, accountID).Scan(&counterCount); err != nil {
		t.Fatalf("count account counters: %v", err)
	}
	if counterCount != 0 || cleanup.AccountScopeCounters != 1 {
		t.Fatalf("account counters remain=%d cleanup=%+v", counterCount, cleanup)
	}
}

func TestPurgeProfitPendingAccountDataYieldsSQLiteLockToOAuthWrites(t *testing.T) {
	db := newProfitTestDB(t)
	ctx := context.Background()
	accountID, groupID := insertProfitTestAccountAndGroup(t, db, true)
	addSecondProfitTestGroup(t, db, accountID)
	insertPendingProfitTestUsage(t, db, accountID, 40)

	type purgeResult struct {
		cleanup ProfitAccountCleanupResult
		err     error
	}
	purgeDone := make(chan purgeResult, 1)
	go func() {
		cleanup, err := db.purgeProfitPendingAccountData(ctx, accountID, 1)
		purgeDone <- purgeResult{cleanup: cleanup, err: err}
	}()

	deadline := time.Now().Add(time.Second)
	for {
		select {
		case result := <-purgeDone:
			t.Fatalf("purge completed before lock-yield check: cleanup=%+v err=%v", result.cleanup, result.err)
		default:
		}
		var remaining int
		if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_logs WHERE account_id = $1`, accountID).Scan(&remaining); err != nil {
			t.Fatalf("count remaining usage logs: %v", err)
		}
		if remaining > 0 && remaining < 40 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("purge did not enter observable batched cleanup")
		}
		time.Sleep(2 * time.Millisecond)
	}

	oauthCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	writeStarted := time.Now()
	oauthAccountID, err := db.InsertAccount(oauthCtx, "oauth-during-profit-cleanup", "refresh-token", "")
	writeDuration := time.Since(writeStarted)
	if err != nil {
		t.Fatalf("OAuth-like account write was blocked by cleanup for %s: %v", writeDuration, err)
	}
	if oauthAccountID <= 0 || writeDuration >= 250*time.Millisecond {
		t.Fatalf("OAuth-like account write did not get a lock window: id=%d duration=%s", oauthAccountID, writeDuration)
	}
	groupCtx, groupCancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer groupCancel()
	groupStarted := time.Now()
	if err := db.SetAccountGroups(groupCtx, oauthAccountID, []int64{groupID}); err != nil {
		t.Fatalf("group assignment was blocked by cleanup for %s: %v", time.Since(groupStarted), err)
	}
	if groupDuration := time.Since(groupStarted); groupDuration >= 250*time.Millisecond {
		t.Fatalf("group assignment did not get a lock window: duration=%s", groupDuration)
	} else {
		t.Logf("concurrent SQLite writes: oauth=%s group=%s", writeDuration, groupDuration)
	}

	select {
	case result := <-purgeDone:
		if result.err != nil {
			t.Fatalf("purge after concurrent OAuth write: %v", result.err)
		}
		if result.cleanup.UsageLogs != 40 {
			t.Fatalf("cleanup usage logs = %d, want 40", result.cleanup.UsageLogs)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("purge did not finish after concurrent OAuth write")
	}
}

func TestPurgeProfitPendingAccountDataProductionBatchKeepsSQLiteWritable(t *testing.T) {
	db := newProfitTestDB(t)
	ctx := context.Background()
	accountID, groupID := insertProfitTestAccountAndGroup(t, db, true)
	addSecondProfitTestGroup(t, db, accountID)
	insertPendingProfitTestUsage(t, db, accountID, 2000)

	type purgeResult struct {
		cleanup ProfitAccountCleanupResult
		err     error
	}
	purgeDone := make(chan purgeResult, 1)
	go func() {
		cleanup, err := db.PurgeProfitPendingAccountData(ctx, accountID)
		purgeDone <- purgeResult{cleanup: cleanup, err: err}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		var remaining int
		if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_logs WHERE account_id = $1`, accountID).Scan(&remaining); err != nil {
			t.Fatalf("count production-batch usage logs: %v", err)
		}
		if remaining > 0 && remaining < 2000 {
			break
		}
		select {
		case result := <-purgeDone:
			t.Fatalf("production-batch purge completed before concurrency check: cleanup=%+v err=%v", result.cleanup, result.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("production-batch purge did not make observable progress")
		}
		time.Sleep(time.Millisecond)
	}

	writeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	writeStarted := time.Now()
	oauthAccountID, err := db.InsertAccount(writeCtx, "oauth-during-production-batch", "refresh-token", "")
	oauthDuration := time.Since(writeStarted)
	if err != nil {
		t.Fatalf("production-batch cleanup blocked OAuth write for %s: %v", oauthDuration, err)
	}
	groupStarted := time.Now()
	if err := db.SetAccountGroups(writeCtx, oauthAccountID, []int64{groupID}); err != nil {
		t.Fatalf("production-batch cleanup blocked group write for %s: %v", time.Since(groupStarted), err)
	}
	groupDuration := time.Since(groupStarted)
	if oauthDuration >= 500*time.Millisecond || groupDuration >= 500*time.Millisecond {
		t.Fatalf("production-batch writes exceeded limit: oauth=%s group=%s", oauthDuration, groupDuration)
	}
	t.Logf("production batch concurrent writes: oauth=%s group=%s", oauthDuration, groupDuration)

	select {
	case result := <-purgeDone:
		if result.err != nil {
			t.Fatalf("production-batch purge failed: %v", result.err)
		}
		if result.cleanup.UsageLogs != 2000 {
			t.Fatalf("production-batch cleanup usage logs = %d, want 2000", result.cleanup.UsageLogs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("production-batch purge did not finish")
	}
}

func TestPurgeProfitPendingAccountDataRejectsSettlementReferencesBeforeCleanup(t *testing.T) {
	db := newProfitTestDB(t)
	ctx := context.Background()
	accountID, _ := insertProfitTestAccountAndGroup(t, db, true)
	addSecondProfitTestGroup(t, db, accountID)
	insertPendingProfitTestUsage(t, db, accountID, 1)
	var ledgerID, ledgerVersion int64
	var ledgerDate string
	if err := db.conn.QueryRowContext(ctx, `SELECT id, ledger_version, CAST(ledger_date AS TEXT)
		FROM profit_daily_ledger WHERE account_id = $1`, accountID).Scan(&ledgerID, &ledgerVersion, &ledgerDate); err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO profit_settlement_items
		(run_id, ledger_row_id, ledger_version, ledger_date, group_id, account_id, multiplier_ppm)
		VALUES ('draft-cleanup-block',$1,$2,$3,1,$4,1000000)`, ledgerID, ledgerVersion, ledgerDate, accountID); err != nil {
		t.Fatalf("insert settlement reference: %v", err)
	}
	if _, err := db.purgeProfitPendingAccountData(ctx, accountID, 2); !errors.Is(err, ErrProfitAccountHasSettlement) {
		t.Fatalf("purge error = %v, want ErrProfitAccountHasSettlement", err)
	}
	var usageCount, accountCount int
	_ = db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_logs WHERE account_id = $1`, accountID).Scan(&usageCount)
	_ = db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE id = $1`, accountID).Scan(&accountCount)
	if usageCount != 1 || accountCount != 1 {
		t.Fatalf("blocked purge changed data: usage=%d account=%d", usageCount, accountCount)
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

func TestProfitAssignmentDefersHistoricalUsageRewriteUntilLedgerRefresh(t *testing.T) {
	db := newProfitTestDB(t)
	ctx := context.Background()
	accountID, groupID := insertProfitTestAccountAndGroup(t, db, false)
	result, err := db.conn.ExecContext(ctx, `INSERT INTO usage_logs
		(account_id, channel, model, effective_model, status_code, total_tokens, account_billed,
		 settlement_group_id_snapshot, settlement_group_name_snapshot, settlement_assignment_source, created_at)
		VALUES ($1, 'codex', 'gpt-5.4', 'gpt-5.4', 200, 321, 1.25, 0, '', 'pending', CURRENT_TIMESTAMP)`, accountID)
	if err != nil {
		t.Fatalf("insert historical usage: %v", err)
	}
	logID, _ := result.LastInsertId()

	if err := db.AssignProfitSettlementGroup(ctx, accountID, groupID); err != nil {
		t.Fatalf("assign settlement group: %v", err)
	}
	var snapshotGroupID int64
	if err := db.conn.QueryRowContext(ctx, `SELECT settlement_group_id_snapshot FROM usage_logs WHERE id=$1`, logID).Scan(&snapshotGroupID); err != nil {
		t.Fatalf("read usage snapshot: %v", err)
	}
	if snapshotGroupID != 0 {
		t.Fatalf("assignment synchronously rewrote historical usage snapshot to %d", snapshotGroupID)
	}

	refresh, err := db.RefreshProfitDailyLedger(ctx, 100)
	if err != nil {
		t.Fatalf("refresh ledger: %v", err)
	}
	if !refresh.CaughtUp {
		t.Fatalf("ledger did not catch up: %+v", refresh)
	}
	var ledgerGroupID int64
	if err := db.conn.QueryRowContext(ctx, `SELECT settlement_group_id FROM profit_daily_ledger WHERE account_id=$1`, accountID).Scan(&ledgerGroupID); err != nil {
		t.Fatalf("read ledger group: %v", err)
	}
	if ledgerGroupID != groupID {
		t.Fatalf("ledger group = %d, want %d", ledgerGroupID, groupID)
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

func TestProfitLedgerRefreshClampsBatchSizeForSQLiteWriteAvailability(t *testing.T) {
	db := newProfitTestDB(t)
	ctx := context.Background()
	accountID, _ := insertProfitTestAccountAndGroup(t, db, false)
	for i := 0; i < MaxProfitLedgerRefreshLimit+1; i++ {
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO usage_logs
			(account_id, channel, model, effective_model, status_code, total_tokens, account_billed, created_at)
			VALUES ($1, 'codex', $2, $2, 200, 1, 0.001, CURRENT_TIMESTAMP)`, accountID, fmt.Sprintf("batch-%d", i)); err != nil {
			t.Fatalf("insert usage log %d: %v", i, err)
		}
	}
	result, err := db.RefreshProfitDailyLedger(ctx, MaxProfitLedgerRefreshLimit*10)
	if err != nil {
		t.Fatalf("RefreshProfitDailyLedger: %v", err)
	}
	if result.ProcessedLogs != int64(MaxProfitLedgerRefreshLimit) || result.RemainingLogs != 1 || result.CaughtUp {
		t.Fatalf("clamped refresh = %+v", result)
	}
}
