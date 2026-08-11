package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
)

const profitSettlementWriteBatchSize = 100

type ProfitSettlementAccountROIItem struct {
	AccountID         int64  `json:"account_id"`
	AccountName       string `json:"account_name"`
	AccountDeleted    bool   `json:"account_deleted"`
	EffectiveMonth    string `json:"effective_month"`
	OwnerGroupID      int64  `json:"owner_group_id"`
	OwnerGroupName    string `json:"owner_group_name"`
	EconomicVersionID int64  `json:"economic_version_id"`
	CostFXPPM         int64  `json:"cost_fx_ppm"`
	Status            string `json:"status"`
	ProfitAccountCostAllocation
}

type profitSettlementManifest struct {
	Items             []ProfitSettlementItem
	AccountROI        []ProfitSettlementAccountROIItem
	OfficialUSD       int64
	PayableCNY        int64
	ReceivableCNY     int64
	FixedCostUSD      int64
	FixedCostCNY      int64
	SourceHighWaterID int64
	Hash              string
}

type profitAccountMonthKey struct {
	AccountID int64
	Month     string
}

type profitSettlementROISeed struct {
	AccountID      int64
	AccountName    string
	AccountDeleted bool
	OwnerGroupID   int64
	OwnerGroupName string
	Month          string
	Usage          int64
}

func (db *DB) buildProfitSettlementManifest(ctx context.Context, run ProfitSettlementRun) (profitSettlementManifest, error) {
	var manifest profitSettlementManifest
	start, end, err := parseProfitDateRange(run.StartDate, run.EndDate)
	if err != nil {
		return manifest, err
	}
	if err := db.validateProfitIngestionCompleteness(ctx, start, end); err != nil {
		return manifest, err
	}
	status, err := db.GetProfitLedgerStatus(ctx)
	if err != nil {
		return manifest, err
	}
	manifest.SourceHighWaterID = status.HighWaterID
	var unaggregated int64
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_logs
		WHERE id > $1 AND id <= $2 AND created_at >= $3 AND created_at < $4`, status.CheckpointID,
		status.HighWaterID, db.timeArg(start.UTC()), db.timeArg(end.UTC())).Scan(&unaggregated); err != nil {
		return manifest, err
	}
	if unaggregated > 0 {
		return manifest, ErrProfitLedgerBehind
	}
	var pendingOwner, pendingConsumer int64
	pendingQuery := `SELECT
		SUM(CASE WHEN l.settlement_group_id=0 THEN 1 ELSE 0 END),
		SUM(CASE WHEN l.consumer_group_id=0 AND COALESCE(l.non_settleable_reason,'')='' THEN 1 ELSE 0 END)
		FROM profit_daily_ledger l WHERE l.ledger_date >= $1 AND l.ledger_date < $2
		AND NOT EXISTS (SELECT 1 FROM profit_ignored_accounts i WHERE i.account_id=l.account_id)`
	pendingArgs := []interface{}{run.StartDate, run.EndDate}
	if run.RevisionNo > 1 {
		pendingQuery += ` AND EXISTS (SELECT 1 FROM profit_ledger_claims c WHERE c.ledger_row_id=l.id AND c.lineage_id=$3)`
		pendingArgs = append(pendingArgs, run.LineageID)
	} else {
		pendingQuery += ` AND COALESCE(l.claimed_lineage_id,'')=''`
	}
	var pendingOwnerNull, pendingConsumerNull sql.NullInt64
	if err := db.conn.QueryRowContext(ctx, pendingQuery, pendingArgs...).Scan(&pendingOwnerNull, &pendingConsumerNull); err != nil {
		return manifest, err
	}
	pendingOwner, pendingConsumer = pendingOwnerNull.Int64, pendingConsumerNull.Int64
	if pendingOwner > 0 || pendingConsumer > 0 {
		return manifest, ErrProfitPendingAssignment
	}
	rates, err := db.loadProfitRateResolver(ctx)
	if err != nil {
		return manifest, err
	}
	query := `SELECT l.id, l.ledger_version, CAST(l.ledger_date AS TEXT),
		l.consumer_group_id, l.consumer_group_name_snapshot, l.settlement_group_id,
		l.settlement_group_name_snapshot, l.api_key_id, l.api_key_name_snapshot, l.account_id,
		l.account_name_snapshot, l.account_deleted, l.model, l.channel, l.request_count, l.total_tokens,
		l.official_cost_usd_micros, l.source_first_log_id, l.source_last_log_id, l.source_hash,
		COALESCE(l.non_settleable_reason,'') FROM profit_daily_ledger l WHERE l.ledger_date >= $1 AND l.ledger_date < $2
		AND l.settlement_group_id > 0
		AND NOT EXISTS (SELECT 1 FROM profit_ignored_accounts i WHERE i.account_id=l.account_id)`
	args := []interface{}{run.StartDate, run.EndDate}
	if run.RevisionNo > 1 {
		query += ` AND EXISTS (SELECT 1 FROM profit_ledger_claims c WHERE c.ledger_row_id=l.id AND c.lineage_id=$3)`
		args = append(args, run.LineageID)
	} else {
		query += ` AND COALESCE(l.claimed_lineage_id,'')=''`
	}
	query += ` ORDER BY l.id`
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return manifest, err
	}
	for rows.Next() {
		var item ProfitSettlementItem
		if err := rows.Scan(&item.LedgerRowID, &item.LedgerVersion, &item.LedgerDate,
			&item.ConsumerGroupID, &item.ConsumerGroupName, &item.OwnerGroupID, &item.OwnerGroupName,
			&item.APIKeyID, &item.APIKeyName, &item.AccountID, &item.AccountName, &item.AccountDeleted,
			&item.Model, &item.Channel, &item.RequestCount, &item.TotalTokens, &item.OfficialUSDMicros,
			&item.SourceFirstLogID, &item.SourceLastLogID, &item.SourceHash, &item.NonSettleableReason); err != nil {
			rows.Close()
			return manifest, err
		}
		item.GroupID, item.GroupName = item.OwnerGroupID, item.OwnerGroupName
		item.SelfUsage = item.NonSettleableReason == "" && item.ConsumerGroupID == item.OwnerGroupID
		if item.NonSettleableReason == "" && !item.SelfUsage {
			rate := rates.resolve(item.LedgerDate, item.ConsumerGroupID, item.OwnerGroupID)
			item.RatePPM = rate.RatePPM
			item.PayableCNYMicros = profitMulDiv(item.OfficialUSDMicros, item.RatePPM, ProfitScalePPM)
			item.ReceivableCNYMicros = item.PayableCNYMicros
		}
		item.MultiplierPPM = item.RatePPM
		item.SettlementCNYMicros = item.PayableCNYMicros
		item.RevenueCNYMicros = item.ReceivableCNYMicros
		item.ProfitCNYMicros = item.ReceivableCNYMicros - item.PayableCNYMicros
		manifest.Items = append(manifest.Items, item)
		manifest.OfficialUSD += item.OfficialUSDMicros
		manifest.PayableCNY += item.PayableCNYMicros
		manifest.ReceivableCNY += item.ReceivableCNYMicros
	}
	if err := rows.Close(); err != nil {
		return manifest, err
	}
	if len(manifest.Items) == 0 {
		return manifest, ErrProfitSettlementEmpty
	}
	manifest.AccountROI, err = db.buildProfitSettlementROI(ctx, run, manifest.Items)
	if err != nil {
		return manifest, err
	}
	for _, item := range manifest.AccountROI {
		manifest.FixedCostUSD += item.AllocatedInRangeUSDMicros
		manifest.FixedCostCNY += item.AllocatedInRangeCNYMicros
	}
	hasher := sha256.New()
	for _, item := range manifest.Items {
		fmt.Fprintf(hasher, "%d|%d|%s|%d|%d|%d|%d|%d|%d|%s\n", item.LedgerRowID,
			item.LedgerVersion, item.SourceHash, item.ConsumerGroupID, item.OwnerGroupID, item.RatePPM,
			item.OfficialUSDMicros, item.PayableCNYMicros, item.ReceivableCNYMicros, item.NonSettleableReason)
	}
	for _, item := range manifest.AccountROI {
		fmt.Fprintf(hasher, "roi|%d|%s|%d|%d|%d|%d|%d|%d\n", item.AccountID, item.EffectiveMonth,
			item.OwnerGroupID, item.EconomicVersionID, item.MonthlyFixedCostUSDMicros,
			item.MonthlyCapacityUSDMicros, item.AllocatedBeforeUSDMicros, item.AllocatedInRangeUSDMicros)
	}
	manifest.Hash = hex.EncodeToString(hasher.Sum(nil))
	return manifest, nil
}

func (db *DB) buildProfitSettlementROI(ctx context.Context, run ProfitSettlementRun, items []ProfitSettlementItem) ([]ProfitSettlementAccountROIItem, error) {
	seeds := make(map[profitAccountMonthKey]*profitSettlementROISeed)
	minMonth, maxMonth := "9999-12-01", "0001-01-01"
	for _, item := range items {
		if item.AccountID <= 0 || len(item.LedgerDate) < 7 {
			continue
		}
		month := item.LedgerDate[:7] + "-01"
		key := profitAccountMonthKey{AccountID: item.AccountID, Month: month}
		seed := seeds[key]
		if seed == nil {
			seed = &profitSettlementROISeed{AccountID: item.AccountID, AccountName: item.AccountName,
				AccountDeleted: item.AccountDeleted, OwnerGroupID: item.OwnerGroupID,
				OwnerGroupName: item.OwnerGroupName, Month: month}
			seeds[key] = seed
		} else if seed.OwnerGroupID != item.OwnerGroupID {
			return nil, fmt.Errorf("account %d has multiple owner groups in month %s", item.AccountID, month)
		}
		seed.Usage += item.OfficialUSDMicros
		if month < minMonth {
			minMonth = month
		}
		if month > maxMonth {
			maxMonth = month
		}
	}
	if len(seeds) == 0 {
		return []ProfitSettlementAccountROIItem{}, nil
	}
	monthEnd, err := parseProfitMonthEnd(maxMonth)
	if err != nil {
		return nil, err
	}
	monthTotals, err := db.loadProfitAccountMonthTotals(ctx, minMonth, monthEnd)
	if err != nil {
		return nil, err
	}
	economics, err := db.loadProfitEconomicVersions(ctx, maxMonth)
	if err != nil {
		return nil, err
	}
	allocatedBefore, err := db.loadProfitAllocationsExcludingLineage(ctx, minMonth, monthEnd, run.LineageID)
	if err != nil {
		return nil, err
	}
	result := make([]ProfitSettlementAccountROIItem, 0, len(seeds))
	for key, seed := range seeds {
		economic := resolveProfitEconomicVersion(economics[key.AccountID], key.Month)
		mapKey := [2]string{strconv.FormatInt(key.AccountID, 10), key.Month}
		allocation := CalculateProfitAccountCostAllocation(economic.Cost, economic.Capacity, seed.Usage,
			monthTotals[mapKey], allocatedBefore[key], run.SettlementRatioPPM)
		result = append(result, ProfitSettlementAccountROIItem{AccountID: key.AccountID,
			AccountName: seed.AccountName, AccountDeleted: seed.AccountDeleted, EffectiveMonth: key.Month,
			OwnerGroupID: seed.OwnerGroupID, OwnerGroupName: seed.OwnerGroupName,
			EconomicVersionID: economic.ID, CostFXPPM: run.SettlementRatioPPM, Status: "pending",
			ProfitAccountCostAllocation: allocation})
	}
	sortProfitSettlementROI(result)
	return result, nil
}

func sortProfitSettlementROI(items []ProfitSettlementAccountROIItem) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0; j-- {
			left, right := items[j-1], items[j]
			if left.EffectiveMonth < right.EffectiveMonth || left.EffectiveMonth == right.EffectiveMonth && left.AccountID <= right.AccountID {
				break
			}
			items[j-1], items[j] = items[j], items[j-1]
		}
	}
}

func (db *DB) loadProfitAllocationsExcludingLineage(ctx context.Context, startMonth, endMonth, lineageID string) (map[profitAccountMonthKey]int64, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT a.account_id, CAST(a.effective_month AS TEXT), SUM(a.allocated_usd_micros)
		FROM profit_account_cost_allocations a JOIN profit_settlement_runs r ON r.id=a.run_id
		WHERE a.active=$1 AND a.effective_month >= $2 AND a.effective_month < $3 AND r.lineage_id <> $4
		GROUP BY a.account_id, a.effective_month`, true, startMonth, endMonth, lineageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[profitAccountMonthKey]int64)
	for rows.Next() {
		var key profitAccountMonthKey
		var total int64
		if err := rows.Scan(&key.AccountID, &key.Month, &total); err != nil {
			return nil, err
		}
		result[key] = total
	}
	return result, rows.Err()
}

func (db *DB) persistProfitSettlementManifest(ctx context.Context, run *ProfitSettlementRun, manifest profitSettlementManifest) error {
	if err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE profit_settlement_runs SET status='building', build_error='',
			settlement_ratio_ppm=$1, notes=$2 WHERE id=$3 AND status IN ('building','draft','build_failed')`,
			run.SettlementRatioPPM, run.Notes, run.ID)
		if err != nil {
			return err
		}
		if affected, _ := res.RowsAffected(); affected != 1 {
			return ErrProfitSettlementConflict
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM profit_settlement_items WHERE run_id=$1`, run.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM profit_settlement_account_roi_items WHERE run_id=$1`, run.ID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM profit_account_cost_allocations WHERE run_id=$1 AND active=$2`, run.ID, false)
		return err
	}); err != nil {
		return err
	}
	for start := 0; start < len(manifest.Items); start += profitSettlementWriteBatchSize {
		end := start + profitSettlementWriteBatchSize
		if end > len(manifest.Items) {
			end = len(manifest.Items)
		}
		batch := manifest.Items[start:end]
		if err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
			for _, item := range batch {
				if err := insertProfitSettlementItemTx(ctx, tx, run.ID, item); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return db.markProfitSettlementBuildFailed(ctx, run.ID, err)
		}
		runtime.Gosched()
	}
	for start := 0; start < len(manifest.AccountROI); start += profitSettlementWriteBatchSize {
		end := start + profitSettlementWriteBatchSize
		if end > len(manifest.AccountROI) {
			end = len(manifest.AccountROI)
		}
		batch := manifest.AccountROI[start:end]
		if err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
			for _, item := range batch {
				if err := insertProfitSettlementROITx(ctx, tx, run.ID, item); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return db.markProfitSettlementBuildFailed(ctx, run.ID, err)
		}
		runtime.Gosched()
	}
	run.OfficialUSDMicros = manifest.OfficialUSD
	run.PayableCNYMicros = manifest.PayableCNY
	run.ReceivableCNYMicros = manifest.ReceivableCNY
	run.SettlementCNYMicros = manifest.PayableCNY
	run.RevenueCNYMicros = manifest.ReceivableCNY
	run.ProfitCNYMicros = manifest.ReceivableCNY - manifest.PayableCNY
	run.FixedCostUSDMicros = manifest.FixedCostUSD
	run.FixedCostCNYMicros = manifest.FixedCostCNY
	run.SourceHighWaterID = manifest.SourceHighWaterID
	run.SourceManifestHash = manifest.Hash
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE profit_settlement_runs SET status='draft', build_error='',
			official_cost_usd_micros=$1, settlement_cost_cny_micros=$2, revenue_cny_micros=$3,
			profit_cny_micros=$4, payable_cny_micros=$5, receivable_cny_micros=$6,
			fixed_cost_allocated_usd_micros=$7, fixed_cost_allocated_cny_micros=$8,
			source_high_water_id=$9, source_manifest_hash=$10 WHERE id=$11 AND status='building'`,
			run.OfficialUSDMicros, run.SettlementCNYMicros, run.RevenueCNYMicros, run.ProfitCNYMicros,
			run.PayableCNYMicros, run.ReceivableCNYMicros, run.FixedCostUSDMicros, run.FixedCostCNYMicros,
			run.SourceHighWaterID, run.SourceManifestHash, run.ID)
		if err != nil {
			return err
		}
		if affected, _ := res.RowsAffected(); affected != 1 {
			return ErrProfitSettlementConflict
		}
		return nil
	})
}

func insertProfitSettlementItemTx(ctx context.Context, tx *sql.Tx, runID string, item ProfitSettlementItem) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO profit_settlement_items (
		run_id,ledger_row_id,ledger_version,ledger_date,group_id,group_name,
		consumer_group_id,consumer_group_name,owner_group_id,owner_group_name,
		api_key_id,api_key_name,account_id,account_name,account_deleted,model,channel,
		multiplier_ppm,rate_ppm,non_settleable_reason,self_usage,request_count,total_tokens,
		official_cost_usd_micros,settlement_cost_cny_micros,revenue_cny_micros,profit_cny_micros,
		payable_cny_micros,receivable_cny_micros,source_first_log_id,source_last_log_id,source_hash
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32)`,
		runID, item.LedgerRowID, item.LedgerVersion, item.LedgerDate, item.GroupID, item.GroupName,
		item.ConsumerGroupID, item.ConsumerGroupName, item.OwnerGroupID, item.OwnerGroupName,
		item.APIKeyID, item.APIKeyName, item.AccountID, item.AccountName, item.AccountDeleted,
		item.Model, item.Channel, item.MultiplierPPM, item.RatePPM, item.NonSettleableReason,
		item.SelfUsage, item.RequestCount, item.TotalTokens, item.OfficialUSDMicros,
		item.SettlementCNYMicros, item.RevenueCNYMicros, item.ProfitCNYMicros,
		item.PayableCNYMicros, item.ReceivableCNYMicros, item.SourceFirstLogID,
		item.SourceLastLogID, item.SourceHash)
	return err
}

func insertProfitSettlementROITx(ctx context.Context, tx *sql.Tx, runID string, item ProfitSettlementAccountROIItem) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO profit_settlement_account_roi_items (
		run_id,account_id,effective_month,owner_group_id,owner_group_name,economic_version_id,
		monthly_fixed_cost_usd_micros,monthly_capacity_usd_micros,usage_in_manifest_usd_micros,
		month_total_usage_usd_micros,allocated_before_usd_micros,allocated_in_run_usd_micros,
		remaining_fixed_cost_usd_micros,cost_fx_ppm,allocated_in_run_cny_micros,status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'pending')`,
		runID, item.AccountID, item.EffectiveMonth, item.OwnerGroupID, item.OwnerGroupName,
		item.EconomicVersionID, item.MonthlyFixedCostUSDMicros, item.MonthlyCapacityUSDMicros,
		item.UsageInManifestUSDMicros, item.MonthTotalUsageUSDMicros, item.AllocatedBeforeUSDMicros,
		item.AllocatedInRangeUSDMicros, item.RemainingFixedCostUSDMicros, item.CostFXPPM,
		item.AllocatedInRangeCNYMicros); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO profit_account_cost_allocations
		(run_id,account_id,effective_month,economic_version_id,allocated_usd_micros,active)
		VALUES ($1,$2,$3,$4,$5,$6)`, runID, item.AccountID, item.EffectiveMonth,
		item.EconomicVersionID, item.AllocatedInRangeUSDMicros, false)
	return err
}

func (db *DB) markProfitSettlementBuildFailed(ctx context.Context, runID string, buildErr error) error {
	message := strings.TrimSpace(buildErr.Error())
	if len(message) > 1000 {
		message = message[:1000]
	}
	_ = db.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE profit_settlement_runs SET status='build_failed', build_error=$1 WHERE id=$2`, message, runID)
		return err
	})
	return buildErr
}

func (db *DB) confirmProfitROIAllocationsTx(ctx context.Context, tx *sql.Tx, run ProfitSettlementRun) error {
	rows, err := tx.QueryContext(ctx, `SELECT account_id, CAST(effective_month AS TEXT), economic_version_id,
		allocated_before_usd_micros, allocated_in_run_usd_micros FROM profit_settlement_account_roi_items
		WHERE run_id=$1 ORDER BY account_id,effective_month`, run.ID)
	if err != nil {
		return err
	}
	type reservation struct {
		accountID, economicID, before, current int64
		month                                  string
	}
	items := make([]reservation, 0)
	for rows.Next() {
		var item reservation
		if err := rows.Scan(&item.accountID, &item.month, &item.economicID, &item.before, &item.current); err != nil {
			rows.Close()
			return err
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range items {
		var other sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT SUM(a.allocated_usd_micros)
			FROM profit_account_cost_allocations a JOIN profit_settlement_runs r ON r.id=a.run_id
			WHERE a.active=$1 AND a.account_id=$2 AND a.effective_month=$3 AND r.lineage_id<>$4`,
			true, item.accountID, item.month, run.LineageID).Scan(&other); err != nil {
			return err
		}
		if other.Int64 != item.before {
			return ErrProfitSettlementConflict
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE profit_account_cost_allocations SET active=$1 WHERE active=$2
		AND run_id IN (SELECT id FROM profit_settlement_runs WHERE lineage_id=$3)`, false, true, run.LineageID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE profit_account_cost_allocations SET active=$1 WHERE run_id=$2`, true, run.ID); err != nil {
		return err
	}
	nowExpr := "NOW()"
	if db.isSQLite() {
		nowExpr = "CURRENT_TIMESTAMP"
	}
	for _, item := range items {
		allocated := item.before + item.current
		_, err := tx.ExecContext(ctx, `INSERT INTO profit_account_month_cost_state
			(account_id,effective_month,economic_version_id,allocated_usd_micros,generation,updated_at)
			VALUES ($1,$2,$3,$4,1,`+nowExpr+`)
			ON CONFLICT (account_id,effective_month) DO UPDATE SET economic_version_id=excluded.economic_version_id,
			allocated_usd_micros=excluded.allocated_usd_micros,generation=profit_account_month_cost_state.generation+1,
			updated_at=`+nowExpr, item.accountID, item.month, item.economicID, allocated)
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE profit_settlement_account_roi_items SET status='confirmed' WHERE run_id=$1`, run.ID)
	return err
}

func (db *DB) loadProfitSettlementROI(ctx context.Context, runID string) ([]ProfitSettlementAccountROIItem, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT r.account_id, COALESCE(a.name,''),
		CASE WHEN COALESCE(a.status,'')='deleted' OR a.deleted_at IS NOT NULL THEN 1 ELSE 0 END,
		CAST(r.effective_month AS TEXT), r.owner_group_id, r.owner_group_name, r.economic_version_id,
		r.monthly_fixed_cost_usd_micros,r.monthly_capacity_usd_micros,r.usage_in_manifest_usd_micros,
		r.month_total_usage_usd_micros,r.allocated_before_usd_micros,r.allocated_in_run_usd_micros,
		r.remaining_fixed_cost_usd_micros,r.cost_fx_ppm,r.allocated_in_run_cny_micros,r.status
		FROM profit_settlement_account_roi_items r LEFT JOIN accounts a ON a.id=r.account_id
		WHERE r.run_id=$1 ORDER BY r.effective_month,r.account_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ProfitSettlementAccountROIItem, 0)
	for rows.Next() {
		var item ProfitSettlementAccountROIItem
		var deletedInt int
		if err := rows.Scan(&item.AccountID, &item.AccountName, &deletedInt, &item.EffectiveMonth,
			&item.OwnerGroupID, &item.OwnerGroupName, &item.EconomicVersionID,
			&item.MonthlyFixedCostUSDMicros, &item.MonthlyCapacityUSDMicros,
			&item.UsageInManifestUSDMicros, &item.MonthTotalUsageUSDMicros,
			&item.AllocatedBeforeUSDMicros, &item.AllocatedInRangeUSDMicros,
			&item.RemainingFixedCostUSDMicros, &item.CostFXPPM,
			&item.AllocatedInRangeCNYMicros, &item.Status); err != nil {
			return nil, err
		}
		item.AccountDeleted = deletedInt != 0
		item.AllocatedAfterUSDMicros = item.AllocatedBeforeUSDMicros + item.AllocatedInRangeUSDMicros
		item.UtilizationPPM = profitMulDiv(item.MonthTotalUsageUSDMicros, ProfitScalePPM, item.MonthlyCapacityUSDMicros)
		if item.MonthlyFixedCostUSDMicros > 0 {
			item.CostCoveragePPM = profitMulDiv(item.AllocatedAfterUSDMicros, ProfitScalePPM, item.MonthlyFixedCostUSDMicros)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func profitSettlementStatusCanBuild(status string) bool {
	return status == "draft" || status == "building" || status == "build_failed"
}

func normalizeProfitSettlementLegacyFields(run *ProfitSettlementRun) {
	if run.PayableCNYMicros == 0 && run.SettlementCNYMicros != 0 {
		run.PayableCNYMicros = run.SettlementCNYMicros
	}
	if run.ReceivableCNYMicros == 0 && run.RevenueCNYMicros != 0 {
		run.ReceivableCNYMicros = run.RevenueCNYMicros
	}
}

func isProfitSettlementConflictError(err error) bool {
	return errors.Is(err, ErrProfitSettlementConflict)
}
