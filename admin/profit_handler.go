package admin

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

func writeProfitError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		c.JSON(http.StatusNotFound, gin.H{"error": "记录不存在"})
	case errors.Is(err, database.ErrProfitPendingAssignment):
		c.JSON(http.StatusConflict, gin.H{"error": "所选时间范围仍有待确认分组的账号，请先完成分组确认", "code": "profit_pending_assignment"})
	case errors.Is(err, database.ErrProfitLedgerBehind):
		c.JSON(http.StatusConflict, gin.H{"error": "利润日账本尚未聚合完成，请先继续聚合", "code": "profit_ledger_behind"})
	case errors.Is(err, database.ErrProfitSettlementEmpty):
		c.JSON(http.StatusBadRequest, gin.H{"error": "该时间范围没有可结算的账本数据", "code": "profit_settlement_empty"})
	case errors.Is(err, database.ErrProfitSettlementConflict):
		c.JSON(http.StatusConflict, gin.H{"error": "结算来源已变化或已被其他结算占用，请刷新草稿后重试", "code": "profit_settlement_conflict"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

func (h *Handler) GetProfitSettings(c *gin.Context) {
	settings, err := h.db.GetProfitSettings(c.Request.Context())
	if err != nil {
		writeProfitError(c, err)
		return
	}
	c.JSON(http.StatusOK, settings)
}

func (h *Handler) UpdateProfitSettings(c *gin.Context) {
	var request database.ProfitSettingsUpdate
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数无效: " + err.Error()})
		return
	}
	settings, err := h.db.UpdateProfitSettings(c.Request.Context(), request)
	if err != nil {
		writeProfitError(c, err)
		return
	}
	c.JSON(http.StatusOK, settings)
}

func (h *Handler) ListProfitGroups(c *gin.Context) {
	groups, err := h.db.ListProfitGroupSettings(c.Request.Context())
	if err != nil {
		writeProfitError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"groups": groups})
}

func (h *Handler) UpdateProfitGroup(c *gin.Context) {
	groupID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || groupID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "分组 ID 无效"})
		return
	}
	var request struct {
		MultiplierPPM int64 `json:"multiplier_ppm" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数无效: " + err.Error()})
		return
	}
	group, err := h.db.UpdateProfitGroupMultiplier(c.Request.Context(), groupID, request.MultiplierPPM)
	if err != nil {
		writeProfitError(c, err)
		return
	}
	c.JSON(http.StatusOK, group)
}

func (h *Handler) ListProfitAPIKeyAssignments(c *gin.Context) {
	items, err := h.db.ListProfitAPIKeyAssignments(c.Request.Context())
	if err != nil {
		writeProfitError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"api_keys": items})
}

func (h *Handler) AssignProfitAPIKeyConsumerGroup(c *gin.Context) {
	apiKeyID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || apiKeyID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "API Key ID 无效"})
		return
	}
	var request struct {
		ConsumerGroupID int64  `json:"consumer_group_id" binding:"required"`
		ApplyHistory    bool   `json:"apply_history"`
		Reason          string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || request.ConsumerGroupID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "使用方分组无效"})
		return
	}
	item, err := h.db.AssignProfitAPIKeyConsumerGroup(c.Request.Context(), apiKeyID, database.ProfitAPIKeyAssignmentUpdate{
		ConsumerGroupID: request.ConsumerGroupID,
		ApplyHistory:    request.ApplyHistory,
		Actor:           "admin@" + c.ClientIP(),
		Reason:          request.Reason,
		Source:          "manual",
	})
	if err != nil {
		writeProfitError(c, err)
		return
	}
	security.SecurityAuditLog("PROFIT_API_KEY_OWNER_ASSIGNED", fmt.Sprintf(
		"api_key_id=%d consumer_group_id=%d apply_history=%t ip=%s", apiKeyID, request.ConsumerGroupID,
		request.ApplyHistory, c.ClientIP()))
	c.JSON(http.StatusOK, item)
}

func (h *Handler) ListProfitPairRates(c *gin.Context) {
	items, err := h.db.ListProfitPairRates(c.Request.Context())
	if err != nil {
		writeProfitError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"rates": items, "system_default_rate_ppm": database.DefaultProfitPairRatePPM})
}

func (h *Handler) UpdateProfitPairRate(c *gin.Context) {
	var request struct {
		ConsumerGroupID int64  `json:"consumer_group_id" binding:"required"`
		OwnerGroupID    int64  `json:"owner_group_id" binding:"required"`
		RatePPM         int64  `json:"rate_ppm"`
		EffectiveDate   string `json:"effective_date"`
		Reason          string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数无效: " + err.Error()})
		return
	}
	item, err := h.db.UpdateProfitPairRate(c.Request.Context(), request.ConsumerGroupID, request.OwnerGroupID,
		request.RatePPM, request.EffectiveDate, "admin@"+c.ClientIP(), request.Reason)
	if err != nil {
		writeProfitError(c, err)
		return
	}
	security.SecurityAuditLog("PROFIT_PAIR_RATE_UPDATED", fmt.Sprintf("consumer_group_id=%d owner_group_id=%d rate_ppm=%d effective_date=%s ip=%s",
		request.ConsumerGroupID, request.OwnerGroupID, request.RatePPM, item.EffectiveDate, c.ClientIP()))
	c.JSON(http.StatusOK, item)
}

func (h *Handler) ListProfitAccountEconomics(c *gin.Context) {
	items, err := h.db.ListProfitAccountEconomics(c.Request.Context(), c.Query("month"))
	if err != nil {
		writeProfitError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"accounts": items,
		"defaults": gin.H{"monthly_fixed_cost_usd_micros": database.DefaultProfitAccountCostMicros,
			"monthly_capacity_usd_micros": database.DefaultProfitAccountCapacityMicros},
	})
}

func (h *Handler) UpdateProfitAccountEconomic(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "账号 ID 无效"})
		return
	}
	var request struct {
		EffectiveMonth            string `json:"effective_month" binding:"required"`
		MonthlyFixedCostUSDMicros int64  `json:"monthly_fixed_cost_usd_micros"`
		MonthlyCapacityUSDMicros  int64  `json:"monthly_capacity_usd_micros" binding:"required"`
		Reason                    string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数无效: " + err.Error()})
		return
	}
	item, err := h.db.UpdateProfitAccountEconomic(c.Request.Context(), accountID, request.EffectiveMonth,
		request.MonthlyFixedCostUSDMicros, request.MonthlyCapacityUSDMicros, "admin@"+c.ClientIP(), request.Reason)
	if err != nil {
		writeProfitError(c, err)
		return
	}
	security.SecurityAuditLog("PROFIT_ACCOUNT_ECONOMIC_UPDATED", fmt.Sprintf("account_id=%d effective_month=%s cost=%d capacity=%d ip=%s",
		accountID, item.EffectiveMonth, item.MonthlyFixedCostUSDMicros, item.MonthlyCapacityUSDMicros, c.ClientIP()))
	c.JSON(http.StatusOK, item)
}

func (h *Handler) ListProfitPendingAccounts(c *gin.Context) {
	items, err := h.db.ListProfitPendingAccounts(c.Request.Context())
	if err != nil {
		writeProfitError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"accounts": items})
}

func (h *Handler) AssignProfitSettlementGroup(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "账号 ID 无效"})
		return
	}
	var request struct {
		GroupID int64 `json:"group_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || request.GroupID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "结算分组无效"})
		return
	}
	if err := h.db.AssignProfitSettlementGroup(c.Request.Context(), accountID, request.GroupID); err != nil {
		writeProfitError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "已回填历史待确认用量，并设置未来默认结算分组"})
}

const purgeProfitAccountConfirmToken = "PURGE-PROFIT-ACCOUNT-DATA"

func (h *Handler) IgnoreProfitPendingAccount(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "账号 ID 无效"})
		return
	}
	var request struct {
		Purge   bool   `json:"purge"`
		Confirm string `json:"confirm"`
	}
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数无效: " + err.Error()})
			return
		}
	}
	if request.Purge && strings.TrimSpace(request.Confirm) != purgeProfitAccountConfirmToken {
		c.JSON(http.StatusBadRequest, gin.H{"error": "彻底删除需要完成二次确认"})
		return
	}

	if !request.Purge {
		if err := h.db.IgnoreProfitPendingAccount(c.Request.Context(), accountID); err != nil {
			h.writeProfitAccountIgnoreError(c, err)
			return
		}
		security.SecurityAuditLog("PROFIT_ACCOUNT_IGNORED", fmt.Sprintf("account_id=%d ip=%s", accountID, c.ClientIP()))
		c.JSON(http.StatusOK, gin.H{"message": "已忽略该账号，后续不再出现在待确认列表", "purged": false})
		return
	}

	h.db.FlushUsageLogs()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Minute)
	defer cancel()
	cleanup, err := h.db.PurgeProfitPendingAccountData(ctx, accountID)
	if err != nil {
		h.writeProfitAccountIgnoreError(c, err)
		return
	}
	h.store.RemoveAccount(accountID)
	security.SecurityAuditLog("PROFIT_ACCOUNT_DATA_PURGED", fmt.Sprintf(
		"account_id=%d usage_logs=%d ledger_rows=%d prompt_incidents=%d prompt_risk_events=%d account_events=%d ip=%s",
		accountID, cleanup.UsageLogs, cleanup.ProfitLedgerRows, cleanup.PromptPolicyIncidents,
		cleanup.PromptRiskEvents, cleanup.AccountEvents, c.ClientIP()))
	c.JSON(http.StatusOK, gin.H{
		"message": "账号及未结算关联数据已彻底删除",
		"purged":  true,
		"cleanup": cleanup,
	})
}

func (h *Handler) writeProfitAccountIgnoreError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		c.JSON(http.StatusNotFound, gin.H{"error": "待确认列表中不存在该账号"})
	case errors.Is(err, database.ErrProfitAccountNotDeleted):
		c.JSON(http.StatusConflict, gin.H{"error": "只能忽略或清理已删除、已进入回收站的账号", "code": "profit_account_not_deleted"})
	case errors.Is(err, database.ErrProfitAccountHasSettlement):
		c.JSON(http.StatusConflict, gin.H{
			"error": "该账号已有结算草稿或已确认结算记录，不能彻底删除；请保留历史数据或先处理相关草稿",
			"code":  "profit_account_has_settlement",
		})
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		c.JSON(http.StatusRequestTimeout, gin.H{"error": "清理未在限定时间内完成，可重新执行以继续分批清理"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "账号处理失败: " + err.Error()})
	}
}

func (h *Handler) RefreshProfitLedger(c *gin.Context) {
	var request struct {
		Limit int `json:"limit"`
	}
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数无效: " + err.Error()})
			return
		}
	}
	h.db.FlushUsageLogs()
	result, err := h.db.RefreshProfitDailyLedgerBatched(c.Request.Context(), request.Limit)
	if err != nil {
		writeProfitError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func defaultProfitDateRange() (string, string) {
	loc, err := time.LoadLocation(database.ProfitTimezone)
	if err != nil {
		loc = time.FixedZone(database.ProfitTimezone, 8*60*60)
	}
	now := time.Now().In(loc)
	start := now.AddDate(0, 0, -6)
	end := now.AddDate(0, 0, 1)
	return start.Format("2006-01-02"), end.Format("2006-01-02")
}

func (h *Handler) GetProfitDashboard(c *gin.Context) {
	startDate, endDate := defaultProfitDateRange()
	if value := strings.TrimSpace(c.Query("start_date")); value != "" {
		startDate = value
	}
	if value := strings.TrimSpace(c.Query("end_date")); value != "" {
		endDate = value
	}
	var ratioPPM int64
	if value := strings.TrimSpace(c.Query("ratio_ppm")); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "结算比例无效"})
			return
		}
		ratioPPM = parsed
	}
	result, err := h.db.GetProfitDashboard(c.Request.Context(), startDate, endDate, ratioPPM)
	if err != nil {
		writeProfitError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) GetProfitDashboardDimension(c *gin.Context) {
	startDate, endDate := defaultProfitDateRange()
	if value := strings.TrimSpace(c.Query("start_date")); value != "" {
		startDate = value
	}
	if value := strings.TrimSpace(c.Query("end_date")); value != "" {
		endDate = value
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "100"))
	items, err := h.db.GetProfitDashboardDimension(c.Request.Context(), startDate, endDate, c.Param("dimension"), page, pageSize)
	if err != nil {
		writeProfitError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "page": page, "page_size": pageSize})
}

type profitSettlementRequest struct {
	StartDate          string `json:"start_date"`
	EndDate            string `json:"end_date"`
	SettlementRatioPPM int64  `json:"settlement_ratio_ppm"`
	Notes              string `json:"notes"`
}

func (h *Handler) CreateProfitSettlement(c *gin.Context) {
	var request profitSettlementRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数无效: " + err.Error()})
		return
	}
	h.db.FlushUsageLogs()
	detail, err := h.db.CreateProfitSettlementDraft(c.Request.Context(), request.StartDate, request.EndDate,
		request.SettlementRatioPPM, request.Notes)
	if err != nil {
		writeProfitError(c, err)
		return
	}
	c.JSON(http.StatusCreated, detail)
}

func (h *Handler) ListProfitSettlements(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	runs, err := h.db.ListProfitSettlements(c.Request.Context(), limit)
	if err != nil {
		writeProfitError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"settlements": runs})
}

func (h *Handler) GetProfitSettlement(c *gin.Context) {
	detail, err := h.db.GetProfitSettlement(c.Request.Context(), c.Param("id"))
	if err != nil {
		writeProfitError(c, err)
		return
	}
	c.JSON(http.StatusOK, detail)
}

func (h *Handler) UpdateProfitSettlement(c *gin.Context) {
	var request profitSettlementRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数无效: " + err.Error()})
		return
	}
	h.db.FlushUsageLogs()
	detail, err := h.db.UpdateProfitSettlementDraft(c.Request.Context(), c.Param("id"), request.SettlementRatioPPM, request.Notes)
	if err != nil {
		writeProfitError(c, err)
		return
	}
	c.JSON(http.StatusOK, detail)
}

func (h *Handler) ConfirmProfitSettlement(c *gin.Context) {
	h.db.FlushUsageLogs()
	detail, err := h.db.ConfirmProfitSettlement(c.Request.Context(), c.Param("id"))
	if err != nil {
		writeProfitError(c, err)
		return
	}
	c.JSON(http.StatusOK, detail)
}

func (h *Handler) ReviseProfitSettlement(c *gin.Context) {
	var request profitSettlementRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数无效: " + err.Error()})
		return
	}
	detail, err := h.db.CreateProfitSettlementRevision(c.Request.Context(), c.Param("id"), request.SettlementRatioPPM, request.Notes)
	if err != nil {
		writeProfitError(c, err)
		return
	}
	c.JSON(http.StatusCreated, detail)
}

func (h *Handler) ExportProfitSettlement(c *gin.Context) {
	detail, err := h.db.GetProfitSettlement(c.Request.Context(), c.Param("id"))
	if err != nil {
		writeProfitError(c, err)
		return
	}
	filename := fmt.Sprintf("profit-settlement-%s-r%d.csv", detail.Run.ID, detail.Run.RevisionNo)
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Status(http.StatusOK)
	_, _ = c.Writer.Write([]byte{0xEF, 0xBB, 0xBF})
	writer := csv.NewWriter(c.Writer)
	money := func(value int64) string {
		return fmt.Sprintf("%.6f", float64(value)/float64(database.ProfitScalePPM))
	}
	_ = writer.Write([]string{"结算方向明细"})
	_ = writer.Write([]string{"日期", "使用方分组", "账号所有方分组", "API Key", "上游账号", "账号状态", "模型", "渠道", "请求数", "Token", "官方用量(USD)", "方向比例(CNY/USD)", "应付(CNY)", "应收(CNY)", "自己账号用量", "不可结算原因", "账本行ID", "账本版本", "来源日志范围", "来源哈希"})
	for _, item := range detail.Items {
		accountStatus := "正常"
		if item.AccountDeleted {
			accountStatus = "已删除"
		}
		_ = writer.Write([]string{
			item.LedgerDate, item.ConsumerGroupName, item.OwnerGroupName, item.APIKeyName, item.AccountName,
			accountStatus, item.Model, item.Channel,
			strconv.FormatInt(item.RequestCount, 10), strconv.FormatInt(item.TotalTokens, 10),
			money(item.OfficialUSDMicros), money(item.RatePPM), money(item.PayableCNYMicros),
			money(item.ReceivableCNYMicros), strconv.FormatBool(item.SelfUsage), item.NonSettleableReason,
			strconv.FormatInt(item.LedgerRowID, 10), strconv.FormatInt(item.LedgerVersion, 10),
			fmt.Sprintf("%d-%d", item.SourceFirstLogID, item.SourceLastLogID), item.SourceHash,
		})
	}
	_ = writer.Write(nil)
	_ = writer.Write([]string{"账号固定成本回收"})
	_ = writer.Write([]string{"账号", "账号状态", "账号所有方分组", "月份", "月固定成本(USD)", "月估算额度(USD)", "本范围用量(USD)", "本月总用量(USD)", "此前已回收(USD)", "本次回收(USD)", "本次回收(CNY)", "回收后累计(USD)", "尚未回收(USD)", "额度利用率", "成本覆盖率", "成本折算比例", "状态"})
	for _, item := range detail.AccountROI {
		accountStatus := "正常"
		if item.AccountDeleted {
			accountStatus = "已删除"
		}
		_ = writer.Write([]string{
			item.AccountName, accountStatus, item.OwnerGroupName, item.EffectiveMonth,
			money(item.MonthlyFixedCostUSDMicros), money(item.MonthlyCapacityUSDMicros),
			money(item.UsageInManifestUSDMicros), money(item.MonthTotalUsageUSDMicros),
			money(item.AllocatedBeforeUSDMicros), money(item.AllocatedInRangeUSDMicros),
			money(item.AllocatedInRangeCNYMicros), money(item.AllocatedAfterUSDMicros),
			money(item.RemainingFixedCostUSDMicros), money(item.UtilizationPPM), money(item.CostCoveragePPM),
			money(item.CostFXPPM), item.Status,
		})
	}
	writer.Flush()
}
