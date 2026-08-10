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
	result, err := h.db.RefreshProfitDailyLedger(c.Request.Context(), request.Limit)
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
	_ = writer.Write([]string{"日期", "结算分组", "API Key", "上游账号", "账号状态", "模型", "渠道", "请求数", "Token", "官方成本(USD)", "结算成本(CNY)", "收入(CNY)", "利润(CNY)", "结算比例", "分组倍率", "账本行ID", "来源日志范围", "来源哈希"})
	for _, item := range detail.Items {
		accountStatus := "正常"
		if item.AccountDeleted {
			accountStatus = "已删除"
		}
		_ = writer.Write([]string{
			item.LedgerDate, item.GroupName, item.APIKeyName, item.AccountName, accountStatus, item.Model, item.Channel,
			strconv.FormatInt(item.RequestCount, 10), strconv.FormatInt(item.TotalTokens, 10),
			fmt.Sprintf("%.6f", float64(item.OfficialUSDMicros)/float64(database.ProfitScalePPM)),
			fmt.Sprintf("%.6f", float64(item.SettlementCNYMicros)/float64(database.ProfitScalePPM)),
			fmt.Sprintf("%.6f", float64(item.RevenueCNYMicros)/float64(database.ProfitScalePPM)),
			fmt.Sprintf("%.6f", float64(item.ProfitCNYMicros)/float64(database.ProfitScalePPM)),
			fmt.Sprintf("%.6f", float64(detail.Run.SettlementRatioPPM)/float64(database.ProfitScalePPM)),
			fmt.Sprintf("%.6f", float64(item.MultiplierPPM)/float64(database.ProfitScalePPM)),
			strconv.FormatInt(item.LedgerRowID, 10),
			fmt.Sprintf("%d-%d", item.SourceFirstLogID, item.SourceLastLogID), item.SourceHash,
		})
	}
	writer.Flush()
}
