package database

import "math"

type ProfitDirectionalUsage struct {
	LedgerDate        string
	ConsumerGroupID   int64
	ConsumerGroupName string
	OwnerGroupID      int64
	OwnerGroupName    string
	OfficialUSDMicros int64
	RequestCount      int64
	TotalTokens       int64
	NonSettleable     bool
}

type ProfitSettlementMatrixCell struct {
	ConsumerGroupID   int64  `json:"consumer_group_id"`
	ConsumerGroupName string `json:"consumer_group_name"`
	OwnerGroupID      int64  `json:"owner_group_id"`
	OwnerGroupName    string `json:"owner_group_name"`
	OfficialUSDMicros int64  `json:"official_cost_usd_micros"`
	RatePPM           int64  `json:"rate_ppm"`
	PayableCNYMicros  int64  `json:"payable_cny_micros"`
	RequestCount      int64  `json:"request_count"`
	TotalTokens       int64  `json:"total_tokens"`
}

type ProfitGroupSettlementSummary struct {
	GroupID             int64  `json:"group_id"`
	GroupName           string `json:"group_name"`
	ReceivableCNYMicros int64  `json:"receivable_cny_micros"`
	PayableCNYMicros    int64  `json:"payable_cny_micros"`
	NetCNYMicros        int64  `json:"net_cny_micros"`
	SelfUsageUSDMicros  int64  `json:"self_usage_usd_micros"`
}

type ProfitSettlementOverview struct {
	OfficialUSDMicros       int64 `json:"official_cost_usd_micros"`
	CrossGroupUSDMicros     int64 `json:"cross_group_usd_micros"`
	SelfUsageUSDMicros      int64 `json:"self_usage_usd_micros"`
	NonSettleableUSDMicros  int64 `json:"non_settleable_usd_micros"`
	ReceivableCNYMicros     int64 `json:"receivable_cny_micros"`
	PayableCNYMicros        int64 `json:"payable_cny_micros"`
	GlobalNetCNYMicros      int64 `json:"global_net_cny_micros"`
	PendingConsumerRequests int64 `json:"pending_consumer_requests"`
	PendingOwnerRequests    int64 `json:"pending_owner_requests"`
}

type ProfitAccountCostAllocation struct {
	MonthlyFixedCostUSDMicros   int64 `json:"monthly_fixed_cost_usd_micros"`
	MonthlyCapacityUSDMicros    int64 `json:"monthly_capacity_usd_micros"`
	UsageInManifestUSDMicros    int64 `json:"usage_in_manifest_usd_micros"`
	MonthTotalUsageUSDMicros    int64 `json:"month_total_usage_usd_micros"`
	AllocatedBeforeUSDMicros    int64 `json:"allocated_before_usd_micros"`
	AllocatedInRangeUSDMicros   int64 `json:"allocated_in_range_usd_micros"`
	AllocatedAfterUSDMicros     int64 `json:"allocated_after_usd_micros"`
	RemainingFixedCostUSDMicros int64 `json:"remaining_fixed_cost_usd_micros"`
	AllocatedInRangeCNYMicros   int64 `json:"allocated_in_range_cny_micros"`
	UtilizationPPM              int64 `json:"utilization_ppm"`
	CostCoveragePPM             int64 `json:"cost_coverage_ppm"`
}

func CalculateProfitAccountCostAllocation(monthlyCost, monthlyCapacity, usageInManifest, monthTotalUsage, allocatedBefore, costFXPPM int64) ProfitAccountCostAllocation {
	if monthlyCost < 0 {
		monthlyCost = 0
	}
	if monthlyCapacity <= 0 {
		monthlyCapacity = DefaultProfitAccountCapacityMicros
	}
	if usageInManifest < 0 {
		usageInManifest = 0
	}
	if monthTotalUsage < 0 {
		monthTotalUsage = 0
	}
	if allocatedBefore < 0 {
		allocatedBefore = 0
	}
	if allocatedBefore > monthlyCost {
		allocatedBefore = monthlyCost
	}
	if costFXPPM <= 0 {
		costFXPPM = ProfitScalePPM
	}
	candidate := profitMulDiv(monthlyCost, usageInManifest, monthlyCapacity)
	available := monthlyCost - allocatedBefore
	allocation := candidate
	if allocation > available {
		allocation = available
	}
	if allocation < 0 {
		allocation = 0
	}
	after := allocatedBefore + allocation
	utilization := profitMulDiv(monthTotalUsage, ProfitScalePPM, monthlyCapacity)
	coverage := int64(0)
	if monthlyCost > 0 {
		coverage = profitMulDiv(after, ProfitScalePPM, monthlyCost)
	}
	return ProfitAccountCostAllocation{
		MonthlyFixedCostUSDMicros: monthlyCost, MonthlyCapacityUSDMicros: monthlyCapacity,
		UsageInManifestUSDMicros: usageInManifest, MonthTotalUsageUSDMicros: monthTotalUsage,
		AllocatedBeforeUSDMicros: allocatedBefore, AllocatedInRangeUSDMicros: allocation,
		AllocatedAfterUSDMicros: after, RemainingFixedCostUSDMicros: monthlyCost - after,
		AllocatedInRangeCNYMicros: profitMulDiv(allocation, costFXPPM, ProfitScalePPM),
		UtilizationPPM:            clampProfitRatio(utilization), CostCoveragePPM: clampProfitRatio(coverage),
	}
}

func clampProfitRatio(value int64) int64 {
	if value < 0 {
		return 0
	}
	if value > math.MaxInt32*ProfitScalePPM {
		return math.MaxInt32 * ProfitScalePPM
	}
	return value
}
