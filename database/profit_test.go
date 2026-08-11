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
	if dashboard.Overall.RevenueCNYMicros != 0 || dashboard.Overall.SettlementCNYMicros != 0 {
		t.Fatalf("unbound system usage must not create bilateral settlement: %+v", dashboard.Overall)
	}
	if dashboard.Settlement.NonSettleableUSDMicros != dashboard.Overall.OfficialUSDMicros {
		t.Fatalf("non-settleable usage = %d, want %d", dashboard.Settlement.NonSettleableUSDMicros, dashboard.Overall.OfficialUSDMicros)
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

func TestProfitDirectionalSettlementAndAccountCostRecovery(t *testing.T) {
	db := newProfitTestDB(t)
	ctx := context.Background()
	consumerResult, err := db.conn.ExecContext(ctx, `INSERT INTO account_groups (name,channel) VALUES ('凡人','codex')`)
	if err != nil {
		t.Fatalf("insert consumer group: %v", err)
	}
	consumerID, _ := consumerResult.LastInsertId()
	ownerResult, err := db.conn.ExecContext(ctx, `INSERT INTO account_groups (name,channel) VALUES ('打铁','codex')`)
	if err != nil {
		t.Fatalf("insert owner group: %v", err)
	}
	ownerID, _ := ownerResult.LastInsertId()
	accountResult, err := db.conn.ExecContext(ctx, `INSERT INTO accounts
		(name,platform,type,credentials,status) VALUES ('Pro20x','openai','oauth','{}','active')`)
	if err != nil {
		t.Fatalf("insert owner account: %v", err)
	}
	accountID, _ := accountResult.LastInsertId()
	startDate, endDate := profitTestDateRange()
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO profit_daily_ledger
		(ledger_date,segment,api_key_id,api_key_name_snapshot,consumer_source_type,consumer_source_id,
		 consumer_assignment_version_id,consumer_group_id,consumer_group_name_snapshot,consumer_assignment_source,
		 account_id,account_name_snapshot,channel,model,settlement_group_id,settlement_group_name_snapshot,
		 assignment_source,request_count,total_tokens,official_cost_usd_micros,source_first_log_id,source_last_log_id,source_hash)
		VALUES ($1,0,101,'凡人-Key','api_key','101',1,$2,'凡人','manual',$3,'Pro20x','codex','gpt-5.4',$4,'打铁','confirmed',1,1000,$5,1,1,'cross')`,
		startDate, consumerID, accountID, ownerID, int64(4000)*ProfitScalePPM); err != nil {
		t.Fatalf("insert cross-group ledger: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO profit_daily_ledger
		(ledger_date,segment,api_key_id,api_key_name_snapshot,consumer_source_type,consumer_source_id,
		 consumer_assignment_version_id,consumer_group_id,consumer_group_name_snapshot,consumer_assignment_source,
		 account_id,account_name_snapshot,channel,model,settlement_group_id,settlement_group_name_snapshot,
		 assignment_source,request_count,total_tokens,official_cost_usd_micros,source_first_log_id,source_last_log_id,source_hash)
		VALUES ($1,1,102,'打铁-Key','api_key','102',2,$2,'打铁','manual',$3,'Pro20x','codex','gpt-5.4',$2,'打铁','confirmed',1,1000,$4,2,2,'self')`,
		startDate, ownerID, accountID, int64(1000)*ProfitScalePPM); err != nil {
		t.Fatalf("insert same-group ledger: %v", err)
	}
	draft, err := db.CreateProfitSettlementDraft(ctx, startDate, endDate, ProfitScalePPM, "方向结算")
	if err != nil {
		t.Fatalf("create directional draft: %v", err)
	}
	wantSettlement := int64(400) * ProfitScalePPM
	if draft.Run.PayableCNYMicros != wantSettlement || draft.Run.ReceivableCNYMicros != wantSettlement {
		t.Fatalf("directional settlement payable=%d receivable=%d, want %d", draft.Run.PayableCNYMicros,
			draft.Run.ReceivableCNYMicros, wantSettlement)
	}
	if len(draft.Items) != 2 || draft.Items[0].PayableCNYMicros+draft.Items[1].PayableCNYMicros != wantSettlement {
		t.Fatalf("unexpected settlement items: %+v", draft.Items)
	}
	if len(draft.AccountROI) != 1 {
		t.Fatalf("account ROI count=%d, want 1", len(draft.AccountROI))
	}
	roi := draft.AccountROI[0]
	if roi.MonthlyFixedCostUSDMicros != 200*ProfitScalePPM || roi.MonthlyCapacityUSDMicros != 10_000*ProfitScalePPM {
		t.Fatalf("unexpected default account economics: %+v", roi)
	}
	if roi.AllocatedInRangeUSDMicros != 100*ProfitScalePPM || roi.RemainingFixedCostUSDMicros != 100*ProfitScalePPM {
		t.Fatalf("fixed-cost recovery should allocate 100 USD from 5000/10000 usage: %+v", roi)
	}
	confirmed, err := db.ConfirmProfitSettlement(ctx, draft.Run.ID)
	if err != nil {
		t.Fatalf("confirm directional settlement: %v", err)
	}
	if confirmed.AccountROI[0].Status != "confirmed" {
		t.Fatalf("ROI status=%q, want confirmed", confirmed.AccountROI[0].Status)
	}
	var activeAllocation int64
	if err := db.conn.QueryRowContext(ctx, `SELECT SUM(allocated_usd_micros) FROM profit_account_cost_allocations
		WHERE account_id=$1 AND active=$2`, accountID, true).Scan(&activeAllocation); err != nil {
		t.Fatalf("read active account allocation: %v", err)
	}
	if activeAllocation != 100*ProfitScalePPM {
		t.Fatalf("active fixed-cost allocation=%d, want %d", activeAllocation, 100*ProfitScalePPM)
	}
}

func TestProfitSettlementRejectsIncompleteIngestionRange(t *testing.T) {
	db := newProfitTestDB(t)
	startDate, endDate := profitTestDateRange()
	if err := db.recordProfitIngestionEvent(context.Background(), "drop", UsageLogModeFull, 3, "test gap"); err != nil {
		t.Fatalf("record ingestion gap: %v", err)
	}
	if _, err := db.CreateProfitSettlementDraft(context.Background(), startDate, endDate, ProfitScalePPM, "不完整范围"); !errors.Is(err, ErrProfitIngestionIncomplete) {
		t.Fatalf("create draft error = %v, want ErrProfitIngestionIncomplete", err)
	}
	var failedRuns int64
	if err := db.conn.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM profit_settlement_runs
		WHERE status='build_failed' AND build_error<>''`).Scan(&failedRuns); err != nil {
		t.Fatalf("read failed settlement runs: %v", err)
	}
	if failedRuns != 1 {
		t.Fatalf("failed settlement runs=%d, want 1", failedRuns)
	}
}

func TestProfitLedgerBatchedCatchUpKeepsFiniteHighWater(t *testing.T) {
	db := newProfitTestDB(t)
	ctx := context.Background()
	accountID, _ := insertProfitTestAccountAndGroup(t, db, false)
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin usage seed: %v", err)
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO usage_logs
		(account_id,channel,model,effective_model,status_code,total_tokens,account_billed,created_at)
		VALUES ($1,'codex',$2,$2,200,1000,1,CURRENT_TIMESTAMP)`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("prepare usage seed: %v", err)
	}
	for i := 0; i < 250; i++ {
		if _, err := stmt.ExecContext(ctx, accountID, fmt.Sprintf("gpt-batch-%03d", i)); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			t.Fatalf("insert usage seed %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatalf("close usage seed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit usage seed: %v", err)
	}
	result, err := db.RefreshProfitDailyLedgerBatched(ctx, 250)
	if err != nil {
		t.Fatalf("batched catch-up: %v", err)
	}
	if result.ProcessedLogs != 250 || result.CheckpointID != 250 || result.HighWaterID != 250 || !result.CaughtUp {
		t.Fatalf("unexpected batched catch-up result: %+v", result)
	}
}

func TestProfitHistoryPreventsNormalAccountPurge(t *testing.T) {
	db := newProfitTestDB(t)
	ctx := context.Background()
	accountID, groupID := insertProfitTestAccountAndGroup(t, db, true)
	startDate, _ := profitTestDateRange()
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO profit_daily_ledger
		(ledger_date,segment,consumer_source_type,consumer_source_id,consumer_group_id,consumer_group_name_snapshot,
		 account_id,account_name_snapshot,channel,model,settlement_group_id,settlement_group_name_snapshot,
		 assignment_source,request_count,total_tokens,official_cost_usd_micros,source_first_log_id,source_last_log_id,source_hash)
		VALUES ($1,0,'system_internal','test',0,'',$2,'测试账号','codex','gpt-5.4',$3,'结算一组',
		'confirmed',1,1000,$4,1,1,'purge-guard')`, startDate, accountID, groupID, ProfitScalePPM); err != nil {
		t.Fatalf("insert protected ledger: %v", err)
	}
	if err := db.PurgeAccount(ctx, accountID); !errors.Is(err, ErrProfitAccountHasSettlement) {
		t.Fatalf("normal purge error = %v, want ErrProfitAccountHasSettlement", err)
	}
	count, err := db.PurgeDeletedAccounts(ctx)
	if err != nil {
		t.Fatalf("purge recycle bin: %v", err)
	}
	if count != 0 {
		t.Fatalf("purged protected account count=%d, want 0", count)
	}
	var remaining int64
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE id=$1`, accountID).Scan(&remaining); err != nil {
		t.Fatalf("read protected account: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("protected account remaining=%d, want 1", remaining)
	}
}

func TestProfitAPIKeyAssignmentPreservesRequestSnapshotsAndBackfillsPendingHistory(t *testing.T) {
	db := newProfitTestDB(t)
	ctx := context.Background()
	accountID, ownerGroupID := insertProfitTestAccountAndGroup(t, db, false)
	if err := db.AssignProfitSettlementGroup(ctx, accountID, ownerGroupID); err != nil {
		t.Fatalf("assign account owner: %v", err)
	}
	consumerAResult, err := db.conn.ExecContext(ctx, `INSERT INTO account_groups (name,channel) VALUES ('凡人','codex')`)
	if err != nil {
		t.Fatalf("insert consumer A: %v", err)
	}
	consumerA, _ := consumerAResult.LastInsertId()
	consumerBResult, err := db.conn.ExecContext(ctx, `INSERT INTO account_groups (name,channel) VALUES ('打铁','codex')`)
	if err != nil {
		t.Fatalf("insert consumer B: %v", err)
	}
	consumerB, _ := consumerBResult.LastInsertId()
	keyResult, err := db.conn.ExecContext(ctx, `INSERT INTO api_keys (name,key) VALUES ('共享-Key','sk-profit-test')`)
	if err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	apiKeyID, _ := keyResult.LastInsertId()
	assignmentA, err := db.AssignProfitAPIKeyConsumerGroup(ctx, apiKeyID, ProfitAPIKeyAssignmentUpdate{ConsumerGroupID: consumerA})
	if err != nil {
		t.Fatalf("assign consumer A: %v", err)
	}
	if snapshot := db.ProfitAPIKeyAttribution(apiKeyID); snapshot.GroupID != consumerA || snapshot.AssignmentVersionID != assignmentA.AssignmentVersionID {
		t.Fatalf("consumer A snapshot=%+v", snapshot)
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO usage_logs
		(account_id,api_key_id,api_key_name,channel,model,effective_model,status_code,total_tokens,account_billed,
		 consumer_source_type,consumer_source_id,consumer_assignment_version_id,consumer_group_id_snapshot,
		 consumer_group_name_snapshot,settlement_group_id_snapshot,
		 settlement_group_name_snapshot,settlement_assignment_source,created_at)
		VALUES ($1,$2,'共享-Key','codex','gpt-5.4','gpt-5.4',200,1000,1,
		'api_key',$3,$4,$5,'凡人',$6,'结算一组','confirmed',CURRENT_TIMESTAMP)`,
		accountID, apiKeyID, fmt.Sprint(apiKeyID), assignmentA.AssignmentVersionID, consumerA, ownerGroupID); err != nil {
		t.Fatalf("insert snapshotted usage: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO usage_logs
		(account_id,api_key_id,api_key_name,channel,model,effective_model,status_code,total_tokens,account_billed,
		 settlement_group_id_snapshot,settlement_group_name_snapshot,settlement_assignment_source,created_at)
		VALUES ($1,$2,'共享-Key','codex','gpt-5.4','gpt-5.4',200,1000,1,$3,'结算一组','confirmed',CURRENT_TIMESTAMP)`,
		accountID, apiKeyID, ownerGroupID); err != nil {
		t.Fatalf("insert pending historical usage: %v", err)
	}
	assignmentB, err := db.AssignProfitAPIKeyConsumerGroup(ctx, apiKeyID, ProfitAPIKeyAssignmentUpdate{
		ConsumerGroupID: consumerB,
		ApplyHistory:    true,
		Reason:          "confirm pending historical usage",
	})
	if err != nil {
		t.Fatalf("assign consumer B with history: %v", err)
	}
	if snapshot := db.ProfitAPIKeyAttribution(apiKeyID); snapshot.GroupID != consumerB || snapshot.AssignmentVersionID != assignmentB.AssignmentVersionID {
		t.Fatalf("consumer B snapshot=%+v", snapshot)
	}
	if _, err := db.RefreshProfitDailyLedger(ctx, 100); err != nil {
		t.Fatalf("refresh attributed ledger: %v", err)
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT consumer_group_id, SUM(request_count) FROM profit_daily_ledger
		WHERE api_key_id=$1 GROUP BY consumer_group_id ORDER BY consumer_group_id`, apiKeyID)
	if err != nil {
		t.Fatalf("query attributed ledger: %v", err)
	}
	defer rows.Close()
	counts := make(map[int64]int64)
	for rows.Next() {
		var groupID, count int64
		if err := rows.Scan(&groupID, &count); err != nil {
			t.Fatalf("scan attributed ledger: %v", err)
		}
		counts[groupID] = count
	}
	if counts[consumerA] != 1 || counts[consumerB] != 1 {
		t.Fatalf("attributed request counts=%v, want one immutable A snapshot and one B history override", counts)
	}
}

func TestProfitSettlementBuildYieldsSQLiteWriterBetweenBatches(t *testing.T) {
	db := newProfitTestDB(t)
	ctx := context.Background()
	accountID, groupID := insertProfitTestAccountAndGroup(t, db, false)
	startDate, endDate := profitTestDateRange()
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin ledger seed: %v", err)
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO profit_daily_ledger
		(ledger_date,segment,consumer_source_type,consumer_source_id,consumer_group_id,consumer_group_name_snapshot,
		 account_id,account_name_snapshot,channel,model,settlement_group_id,settlement_group_name_snapshot,
		 assignment_source,request_count,total_tokens,official_cost_usd_micros,source_first_log_id,source_last_log_id,source_hash)
		VALUES ($1,$2,'api_key',$3,$4,'结算一组',$5,'测试账号','codex','gpt-5.4',$4,'结算一组',
		'confirmed',1,1000,$6,$7,$7,$8)`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("prepare ledger seed: %v", err)
	}
	for i := 1; i <= 2500; i++ {
		if _, err := stmt.ExecContext(ctx, startDate, i, fmt.Sprint(i), groupID, accountID, ProfitScalePPM, i, fmt.Sprintf("batch-%d", i)); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			t.Fatalf("insert ledger seed %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatalf("close ledger seed statement: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit ledger seed: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, buildErr := db.CreateProfitSettlementDraft(ctx, startDate, endDate, ProfitScalePPM, "批量短事务")
		done <- buildErr
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var building int64
		if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM profit_settlement_runs WHERE status='building'`).Scan(&building); err != nil {
			t.Fatalf("read building settlement: %v", err)
		}
		if building > 0 {
			break
		}
		select {
		case buildErr := <-done:
			t.Fatalf("settlement finished before concurrency check: %v", buildErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("settlement did not enter building state")
		}
	}
	writerCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := db.conn.ExecContext(writerCtx, `INSERT INTO account_groups (name,channel) VALUES ('并发写入','codex')`); err != nil {
		t.Fatalf("concurrent SQLite writer could not progress: %v", err)
	}
	if buildErr := <-done; buildErr != nil {
		t.Fatalf("build settlement: %v", buildErr)
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
	// Simulate a legacy database where an old version physically removed the account
	// shell before profit history gained its current purge protection.
	if _, err := db.conn.ExecContext(ctx, `DELETE FROM account_group_members WHERE account_id=$1`, accountID); err != nil {
		t.Fatalf("delete legacy account memberships: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `DELETE FROM accounts WHERE id=$1`, accountID); err != nil {
		t.Fatalf("delete legacy account shell: %v", err)
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
