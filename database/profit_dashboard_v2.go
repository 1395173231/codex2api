package database

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (db *DB) getProfitDashboardV2(ctx context.Context, startDate, endDate string, costFXPPM int64) (ProfitDashboardResponse, error) {
	settings, err := db.GetProfitSettings(ctx)
	if err != nil {
		return ProfitDashboardResponse{}, err
	}
	if _, _, err := parseProfitDateRange(startDate, endDate); err != nil {
		return ProfitDashboardResponse{}, err
	}
	costFXPPM = normalizeProfitPPM(costFXPPM, settings.DefaultSettlementRatioPPM)
	result := ProfitDashboardResponse{
		StartDate: startDate, EndDate: endDate, Timezone: ProfitTimezone, SettlementRatioPPM: costFXPPM,
		Groups: []ProfitDashboardDimension{}, APIKeys: []ProfitDashboardDimension{},
		Accounts: []ProfitDashboardDimension{}, Models: []ProfitDashboardDimension{},
		SettlementMatrix: []ProfitSettlementMatrixCell{}, GroupSettlements: []ProfitGroupSettlementSummary{},
		AccountROI: []ProfitAccountROI{},
	}
	if result.Ledger, err = db.GetProfitLedgerStatus(ctx); err != nil {
		return result, err
	}
	rates, err := db.loadProfitRateResolver(ctx)
	if err != nil {
		return result, err
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT CAST(ledger_date AS TEXT), consumer_group_id,
		MAX(consumer_group_name_snapshot), settlement_group_id, MAX(settlement_group_name_snapshot),
		MAX(non_settleable_reason), SUM(official_cost_usd_micros), SUM(request_count), SUM(total_tokens),
		SUM(input_tokens), SUM(output_tokens), SUM(cached_tokens), SUM(reasoning_tokens)
		FROM profit_daily_ledger l WHERE ledger_date >= $1 AND ledger_date < $2
		AND NOT EXISTS (SELECT 1 FROM profit_ignored_accounts i WHERE i.account_id = l.account_id)
		GROUP BY ledger_date, consumer_group_id, settlement_group_id, non_settleable_reason
		ORDER BY ledger_date, consumer_group_id, settlement_group_id`, startDate, endDate)
	if err != nil {
		return result, err
	}
	type matrixKey struct{ consumer, owner int64 }
	matrix := make(map[matrixKey]*ProfitSettlementMatrixCell)
	groups := make(map[int64]*ProfitGroupSettlementSummary)
	for rows.Next() {
		var usage ProfitDirectionalUsage
		var nonSettleableReason string
		var inputTokens, outputTokens, cachedTokens, reasoningTokens int64
		if err := rows.Scan(&usage.LedgerDate, &usage.ConsumerGroupID, &usage.ConsumerGroupName,
			&usage.OwnerGroupID, &usage.OwnerGroupName, &nonSettleableReason, &usage.OfficialUSDMicros,
			&usage.RequestCount, &usage.TotalTokens, &inputTokens, &outputTokens, &cachedTokens, &reasoningTokens); err != nil {
			rows.Close()
			return result, err
		}
		usage.NonSettleable = strings.TrimSpace(nonSettleableReason) != ""
		result.Settlement.OfficialUSDMicros += usage.OfficialUSDMicros
		result.Overall.OfficialUSDMicros += usage.OfficialUSDMicros
		result.Overall.RequestCount += usage.RequestCount
		result.Overall.TotalTokens += usage.TotalTokens
		result.Overall.InputTokens += inputTokens
		result.Overall.OutputTokens += outputTokens
		result.Overall.CachedTokens += cachedTokens
		result.Overall.ReasoningTokens += reasoningTokens
		if usage.OwnerGroupID <= 0 {
			result.Settlement.PendingOwnerRequests += usage.RequestCount
		}
		if usage.NonSettleable {
			result.Settlement.NonSettleableUSDMicros += usage.OfficialUSDMicros
			continue
		}
		if usage.ConsumerGroupID <= 0 {
			result.Settlement.PendingConsumerRequests += usage.RequestCount
			continue
		}
		consumer := profitGroupSettlement(groups, usage.ConsumerGroupID, usage.ConsumerGroupName)
		owner := profitGroupSettlement(groups, usage.OwnerGroupID, usage.OwnerGroupName)
		if usage.ConsumerGroupID == usage.OwnerGroupID {
			result.Settlement.SelfUsageUSDMicros += usage.OfficialUSDMicros
			consumer.SelfUsageUSDMicros += usage.OfficialUSDMicros
			continue
		}
		if usage.OwnerGroupID <= 0 {
			continue
		}
		rate := rates.resolve(usage.LedgerDate, usage.ConsumerGroupID, usage.OwnerGroupID)
		payable := profitMulDiv(usage.OfficialUSDMicros, rate.RatePPM, ProfitScalePPM)
		result.Settlement.CrossGroupUSDMicros += usage.OfficialUSDMicros
		result.Settlement.PayableCNYMicros += payable
		result.Settlement.ReceivableCNYMicros += payable
		consumer.PayableCNYMicros += payable
		owner.ReceivableCNYMicros += payable
		key := matrixKey{usage.ConsumerGroupID, usage.OwnerGroupID}
		cell := matrix[key]
		if cell == nil {
			cell = &ProfitSettlementMatrixCell{ConsumerGroupID: usage.ConsumerGroupID,
				ConsumerGroupName: profitGroupDisplayName(usage.ConsumerGroupID, usage.ConsumerGroupName),
				OwnerGroupID:      usage.OwnerGroupID, OwnerGroupName: profitGroupDisplayName(usage.OwnerGroupID, usage.OwnerGroupName)}
			matrix[key] = cell
		}
		cell.OfficialUSDMicros += usage.OfficialUSDMicros
		cell.PayableCNYMicros += payable
		cell.RequestCount += usage.RequestCount
		cell.TotalTokens += usage.TotalTokens
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	for _, cell := range matrix {
		if cell.OfficialUSDMicros > 0 {
			cell.RatePPM = profitMulDiv(cell.PayableCNYMicros, ProfitScalePPM, cell.OfficialUSDMicros)
		}
		result.SettlementMatrix = append(result.SettlementMatrix, *cell)
	}
	sort.Slice(result.SettlementMatrix, func(i, j int) bool {
		if result.SettlementMatrix[i].ConsumerGroupID == result.SettlementMatrix[j].ConsumerGroupID {
			return result.SettlementMatrix[i].OwnerGroupID < result.SettlementMatrix[j].OwnerGroupID
		}
		return result.SettlementMatrix[i].ConsumerGroupID < result.SettlementMatrix[j].ConsumerGroupID
	})
	for _, group := range groups {
		group.NetCNYMicros = group.ReceivableCNYMicros - group.PayableCNYMicros
		result.GroupSettlements = append(result.GroupSettlements, *group)
	}
	sort.Slice(result.GroupSettlements, func(i, j int) bool { return result.GroupSettlements[i].GroupID < result.GroupSettlements[j].GroupID })
	result.Settlement.GlobalNetCNYMicros = result.Settlement.ReceivableCNYMicros - result.Settlement.PayableCNYMicros
	result.Overall.SettlementCNYMicros = result.Settlement.PayableCNYMicros
	result.Overall.RevenueCNYMicros = result.Settlement.ReceivableCNYMicros
	result.Overall.ProfitCNYMicros = result.Settlement.GlobalNetCNYMicros

	// Dimension details are intentionally loaded through the paged dimension endpoint.
	// Loading groups, API keys, accounts, and models together made wide date ranges do
	// four unnecessary aggregation scans before the dashboard could render.
	result.AccountROI, err = db.loadProfitAccountROI(ctx, startDate, endDate, costFXPPM)
	return result, err
}

func profitGroupSettlement(target map[int64]*ProfitGroupSettlementSummary, id int64, name string) *ProfitGroupSettlementSummary {
	item := target[id]
	if item == nil {
		item = &ProfitGroupSettlementSummary{GroupID: id, GroupName: profitGroupDisplayName(id, name)}
		target[id] = item
	}
	return item
}

func (db *DB) loadProfitDashboardDimension(ctx context.Context, startDate, endDate, dimension string, limit, offset int) ([]ProfitDashboardDimension, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	var idExpr, nameExpr, deletedExpr, pendingExpr, groupExpr, orderExpr string
	switch dimension {
	case "group":
		idExpr, nameExpr, deletedExpr, pendingExpr = "CAST(settlement_group_id AS TEXT)", "MAX(settlement_group_name_snapshot)", "0", "CASE WHEN settlement_group_id=0 THEN 1 ELSE 0 END"
		groupExpr, orderExpr = "settlement_group_id", "SUM(official_cost_usd_micros) DESC"
	case "api_key":
		idExpr, nameExpr, deletedExpr, pendingExpr = "CAST(api_key_id AS TEXT)", "MAX(COALESCE(NULLIF(api_key_name_snapshot,''),NULLIF(api_key_masked_snapshot,''),'未命名 Key'))", "0", "MAX(CASE WHEN consumer_group_id=0 AND non_settleable_reason='' THEN 1 ELSE 0 END)"
		groupExpr, orderExpr = "api_key_id", "SUM(official_cost_usd_micros) DESC"
	case "account":
		idExpr, nameExpr, deletedExpr, pendingExpr = "CAST(account_id AS TEXT)", "MAX(account_name_snapshot)", "MAX(CASE WHEN account_deleted THEN 1 ELSE 0 END)", "MAX(CASE WHEN settlement_group_id=0 THEN 1 ELSE 0 END)"
		groupExpr, orderExpr = "SUM(official_cost_usd_micros) DESC", "SUM(official_cost_usd_micros) DESC"
		groupExpr = "account_id"
	case "model":
		idExpr, nameExpr, deletedExpr, pendingExpr = "model", "model", "0", "0"
		groupExpr, orderExpr = "model", "SUM(official_cost_usd_micros) DESC"
	default:
		return nil, fmt.Errorf("unsupported profit dimension %q", dimension)
	}
	query := fmt.Sprintf(`SELECT %s, %s, %s, %s, SUM(request_count), SUM(input_tokens), SUM(output_tokens),
		SUM(cached_tokens), SUM(reasoning_tokens), SUM(total_tokens), SUM(official_cost_usd_micros)
		FROM profit_daily_ledger l WHERE ledger_date >= $1 AND ledger_date < $2
		AND NOT EXISTS (SELECT 1 FROM profit_ignored_accounts i WHERE i.account_id=l.account_id)
		GROUP BY %s ORDER BY %s LIMIT $3 OFFSET $4`, idExpr, nameExpr, deletedExpr, pendingExpr, groupExpr, orderExpr)
	rows, err := db.conn.QueryContext(ctx, query, startDate, endDate, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ProfitDashboardDimension, 0)
	for rows.Next() {
		var item ProfitDashboardDimension
		var deletedInt, pendingInt int
		if err := rows.Scan(&item.ID, &item.Name, &deletedInt, &pendingInt, &item.RequestCount,
			&item.InputTokens, &item.OutputTokens, &item.CachedTokens, &item.ReasoningTokens,
			&item.TotalTokens, &item.OfficialUSDMicros); err != nil {
			return nil, err
		}
		item.Deleted = deletedInt != 0
		item.Pending = pendingInt != 0
		items = append(items, item)
	}
	return items, rows.Err()
}

func (db *DB) GetProfitDashboardDimension(ctx context.Context, startDate, endDate, dimension string, page, pageSize int) ([]ProfitDashboardDimension, error) {
	if _, _, err := parseProfitDateRange(startDate, endDate); err != nil {
		return nil, err
	}
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 500 {
		pageSize = 100
	}
	return db.loadProfitDashboardDimension(ctx, startDate, endDate, dimension, pageSize, (page-1)*pageSize)
}

type profitROIUsageRow struct {
	AccountID       int64
	AccountName     string
	AccountDeleted  bool
	OwnerGroupID    int64
	OwnerGroupName  string
	Month           string
	UsageInManifest int64
}

type profitEconomicResolved struct {
	ID       int64
	Month    string
	Cost     int64
	Capacity int64
}

func (db *DB) loadProfitAccountROI(ctx context.Context, startDate, endDate string, costFXPPM int64) ([]ProfitAccountROI, error) {
	monthExpr := "TO_CHAR(ledger_date, 'YYYY-MM') || '-01'"
	if db.isSQLite() {
		monthExpr = "SUBSTR(CAST(ledger_date AS TEXT),1,7) || '-01'"
	}
	query := fmt.Sprintf(`SELECT account_id, MAX(account_name_snapshot), MAX(CASE WHEN account_deleted THEN 1 ELSE 0 END),
		settlement_group_id, MAX(settlement_group_name_snapshot), %s, SUM(official_cost_usd_micros)
		FROM profit_daily_ledger l WHERE ledger_date >= $1 AND ledger_date < $2
		AND NOT EXISTS (SELECT 1 FROM profit_ignored_accounts i WHERE i.account_id=l.account_id)
		GROUP BY account_id, settlement_group_id, %s ORDER BY account_id, %s`, monthExpr, monthExpr, monthExpr)
	rows, err := db.conn.QueryContext(ctx, query, startDate, endDate)
	if err != nil {
		return nil, err
	}
	usageRows := make([]profitROIUsageRow, 0)
	months := make(map[string]struct{})
	for rows.Next() {
		var item profitROIUsageRow
		var deletedInt int
		if err := rows.Scan(&item.AccountID, &item.AccountName, &deletedInt, &item.OwnerGroupID,
			&item.OwnerGroupName, &item.Month, &item.UsageInManifest); err != nil {
			rows.Close()
			return nil, err
		}
		item.AccountDeleted = deletedInt != 0
		usageRows = append(usageRows, item)
		months[item.Month] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(usageRows) == 0 {
		return []ProfitAccountROI{}, nil
	}
	minMonth, maxMonth := "9999-12-01", "0001-01-01"
	for month := range months {
		if month < minMonth {
			minMonth = month
		}
		if month > maxMonth {
			maxMonth = month
		}
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
	allocations, err := db.loadProfitActiveAllocations(ctx, minMonth, monthEnd)
	if err != nil {
		return nil, err
	}
	result := make([]ProfitAccountROI, 0, len(usageRows))
	for _, usage := range usageRows {
		economic := resolveProfitEconomicVersion(economics[usage.AccountID], usage.Month)
		allocation := CalculateProfitAccountCostAllocation(economic.Cost, economic.Capacity, usage.UsageInManifest,
			monthTotals[[2]string{strconv.FormatInt(usage.AccountID, 10), usage.Month}],
			allocations[[2]string{strconv.FormatInt(usage.AccountID, 10), usage.Month}], costFXPPM)
		result = append(result, ProfitAccountROI{AccountID: usage.AccountID, AccountName: usage.AccountName,
			AccountDeleted: usage.AccountDeleted, OwnerGroupID: usage.OwnerGroupID,
			OwnerGroupName: profitGroupDisplayName(usage.OwnerGroupID, usage.OwnerGroupName),
			EffectiveMonth: usage.Month, EconomicVersionID: economic.ID, ProfitAccountCostAllocation: allocation})
	}
	return result, nil
}

func parseProfitMonthEnd(month string) (string, error) {
	start, err := time.ParseInLocation("2006-01-02", month, profitLocation())
	if err != nil {
		return "", err
	}
	return start.AddDate(0, 1, 0).Format("2006-01-02"), nil
}

func (db *DB) loadProfitAccountMonthTotals(ctx context.Context, startMonth, endMonth string) (map[[2]string]int64, error) {
	monthExpr := "TO_CHAR(ledger_date, 'YYYY-MM') || '-01'"
	if db.isSQLite() {
		monthExpr = "SUBSTR(CAST(ledger_date AS TEXT),1,7) || '-01'"
	}
	query := fmt.Sprintf(`SELECT account_id, %s, SUM(official_cost_usd_micros) FROM profit_daily_ledger
		WHERE ledger_date >= $1 AND ledger_date < $2 GROUP BY account_id, %s`, monthExpr, monthExpr)
	rows, err := db.conn.QueryContext(ctx, query, startMonth, endMonth)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[[2]string]int64)
	for rows.Next() {
		var id int64
		var month string
		var total int64
		if err := rows.Scan(&id, &month, &total); err != nil {
			return nil, err
		}
		result[[2]string{strconv.FormatInt(id, 10), month}] = total
	}
	return result, rows.Err()
}

func (db *DB) loadProfitEconomicVersions(ctx context.Context, maxMonth string) (map[int64][]profitEconomicResolved, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT id, account_id, CAST(effective_month AS TEXT), monthly_fixed_cost_usd_micros,
		monthly_capacity_usd_micros FROM profit_account_economic_versions WHERE active=$1 AND effective_month <= $2
		ORDER BY account_id, effective_month DESC, revision_no DESC, id DESC`, true, maxMonth)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int64][]profitEconomicResolved)
	for rows.Next() {
		var accountID int64
		var item profitEconomicResolved
		if err := rows.Scan(&item.ID, &accountID, &item.Month, &item.Cost, &item.Capacity); err != nil {
			return nil, err
		}
		result[accountID] = append(result[accountID], item)
	}
	return result, rows.Err()
}

func resolveProfitEconomicVersion(versions []profitEconomicResolved, month string) profitEconomicResolved {
	for _, item := range versions {
		if item.Month <= month {
			return item
		}
	}
	return profitEconomicResolved{Month: month, Cost: DefaultProfitAccountCostMicros, Capacity: DefaultProfitAccountCapacityMicros}
}

func (db *DB) loadProfitActiveAllocations(ctx context.Context, startMonth, endMonth string) (map[[2]string]int64, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT account_id, CAST(effective_month AS TEXT), SUM(allocated_usd_micros)
		FROM profit_account_cost_allocations WHERE active=$1 AND effective_month >= $2 AND effective_month < $3
		GROUP BY account_id, effective_month`, true, startMonth, endMonth)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[[2]string]int64)
	for rows.Next() {
		var id int64
		var month string
		var total int64
		if err := rows.Scan(&id, &month, &total); err != nil {
			return nil, err
		}
		result[[2]string{strconv.FormatInt(id, 10), month}] = total
	}
	return result, rows.Err()
}
