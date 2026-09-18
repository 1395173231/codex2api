package admin

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func (h *Handler) StartCodexTurnStates(ctx context.Context) error {
	h.codexTurnStatesStartOnce.Do(func() {
		loadCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		manager, err := auth.NewCodexTurnStateManager(loadCtx, h.db, h.store, proxy.HarvestCodexTurnState)
		if err != nil {
			h.codexTurnStatesStartErr = err
			return
		}
		h.codexTurnStates = manager
		if !h.startDBBackgroundTaskWithParent(ctx, manager.Run) {
			h.codexTurnStatesStartErr = errors.New("cannot start turn-state background service")
		}
	})
	return h.codexTurnStatesStartErr
}

func (h *Handler) requireTurnStates(c *gin.Context) bool {
	if h.codexTurnStates == nil {
		writeError(c, http.StatusServiceUnavailable, "turn-state service unavailable")
		return false
	}
	return true
}

func (h *Handler) GetCodexTurnStateSettings(c *gin.Context) {
	if !h.requireTurnStates(c) {
		return
	}
	cfg := h.codexTurnStates.Config()
	configured := cfg.HarvestProxyURL != ""
	cfg.HarvestProxyURL = auth.MaskCodexHarvestProxy(cfg.HarvestProxyURL)
	c.JSON(http.StatusOK, struct {
		auth.CodexTurnStateSettings
		ProxyConfigured bool `json:"proxy_configured"`
	}{cfg, configured})
}

func (h *Handler) UpdateCodexTurnStateSettings(c *gin.Context) {
	if !h.requireTurnStates(c) {
		return
	}
	h.settingsUpdateMu.Lock()
	defer h.settingsUpdateMu.Unlock()
	current := h.codexTurnStates.Config()
	req := struct {
		auth.CodexTurnStateSettings
		ClearProxy bool `json:"clear_proxy"`
	}{CodexTurnStateSettings: current}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "invalid turn-state settings")
		return
	}
	if req.ClearProxy {
		req.HarvestProxyURL = ""
	} else if strings.TrimSpace(req.HarvestProxyURL) == "" || req.HarvestProxyURL == auth.MaskCodexHarvestProxy(current.HarvestProxyURL) {
		req.HarvestProxyURL = current.HarvestProxyURL
	} else if strings.Contains(req.HarvestProxyURL, "***") || strings.Contains(strings.ToLower(req.HarvestProxyURL), "%2a%2a%2a") {
		writeError(c, http.StatusBadRequest, "replace the masked proxy with a complete URL")
		return
	}
	cfg, err := auth.NormalizeCodexTurnStateSettings(req.CodexTurnStateSettings)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.codexTurnStates.SaveConfig(c.Request.Context(), cfg); err != nil {
		writeError(c, http.StatusInternalServerError, "cannot save turn-state settings")
		return
	}
	h.GetCodexTurnStateSettings(c)
}

func (h *Handler) turnStateAccount(c *gin.Context) *auth.Account {
	if !h.requireTurnStates(c) {
		return nil
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "invalid account id")
		return nil
	}
	row, err := h.db.GetAccountByID(c.Request.Context(), id)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "cannot load account")
		return nil
	}
	if row == nil {
		writeError(c, http.StatusNotFound, "account not found")
		return nil
	}
	upstream := strings.ToLower(strings.TrimSpace(row.GetCredential("upstream_type")))
	if upstream != "" && upstream != "codex" {
		writeError(c, http.StatusBadRequest, "Codex account required")
		return nil
	}
	if a := h.store.FindByID(id); a != nil {
		return a
	}
	return &auth.Account{DBID: id, AccessToken: row.GetCredential("access_token"), AccountID: row.GetCredential("account_id"),
		Email: row.GetCredential("email"), PlanType: row.GetCredential("plan_type"), CredentialFamilyID: row.CredentialFamilyID,
		CustomHeaders: row.GetCredentialStringMap("custom_headers"), DispatchPaused: 1}
}

func (h *Handler) GetAccountTurnStates(c *gin.Context) {
	a := h.turnStateAccount(c)
	if a == nil {
		return
	}
	c.JSON(http.StatusOK, gin.H{"enabled": h.codexTurnStates.Config().Enabled, "items": h.codexTurnStates.Statuses(a.ID(), a)})
}

func (h *Handler) UpdateAccountTurnState(c *gin.Context) {
	a := h.turnStateAccount(c)
	if a == nil {
		return
	}
	var req struct {
		Model string  `json:"model"`
		Token *string `json:"token"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Token == nil || strings.TrimSpace(*req.Token) == "" {
		writeError(c, http.StatusBadRequest, "model and token are required")
		return
	}
	if err := h.codexTurnStates.Replace(c.Request.Context(), a, req.Model, *req.Token); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": h.codexTurnStates.Statuses(a.ID(), a)})
}

func (h *Handler) DeleteAccountTurnState(c *gin.Context) {
	a := h.turnStateAccount(c)
	if a == nil {
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "model is required")
		return
	}
	if err := h.codexTurnStates.Replace(c.Request.Context(), a, req.Model, ""); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) RefreshAccountTurnState(c *gin.Context) {
	a := h.turnStateAccount(c)
	if a == nil {
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "model is required")
		return
	}
	if err := h.codexTurnStates.RequestRefresh(c.Request.Context(), a, req.Model); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"status": "queued"})
}
