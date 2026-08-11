package proxy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

const (
	promptConversationLockedReasonCode = "conversation_cyber_locked"
	promptConversationLockedMessage    = "当前对话因上游 CYB 已被锁定。本次锁定拦截不会重复累计处罚；可等待自动到期，或由管理员在「Prompt 检查 → 风险画像 → 会话详情」手动解锁。解除后再次触发 CYB 可能会停用账号。"
	promptUserCyberCooldownReasonCode  = "user_cyber_cooldown"
	promptUserCyberCooldownMessage     = "该用户因上游 CYB 现处于 30 分钟安全冷却期。冷却期间的新请求不会继续转发，也不会重复累计处罚；可等待自动到期，或由管理员在「Prompt 检查 → 风险画像 → 用户详情」手动解除冷却。"
	promptConversationLockCacheTTL     = 30 * time.Second
	promptUserCyberCooldownTTL         = database.PromptUserCyberCooldownTTL
)

type promptCyberRestriction struct {
	ReasonCode        string
	Message           string
	Scope             string
	LockedAt          time.Time
	ExpiresAt         time.Time
	RetryAfterSeconds int64
	IncidentID        string
}

type promptConversationLockIdentity struct {
	LockKey            string
	Platform           string
	NewAPIUserID       string
	SessionFingerprint string
	SessionHash        string
}

func verifiedPromptUserCooldownIdentity(c *gin.Context, policyContext verifiedNewAPIPolicyContext) (promptConversationLockIdentity, bool) {
	if c == nil || !policyContext.MetaVerified {
		return promptConversationLockIdentity{}, false
	}
	platform := normalizedNewAPIPlatform(policyContext.Platform)
	userID := strings.TrimSpace(policyContext.Identity.UserID)
	if platform == "" || userID == "" {
		return promptConversationLockIdentity{}, false
	}
	digest := sha256.Sum256([]byte("prompt-user-cooldown-v1\x00" + platform + "\x00" + userID))
	return promptConversationLockIdentity{
		LockKey: hex.EncodeToString(digest[:]), Platform: platform, NewAPIUserID: userID,
	}, true
}

func verifiedPromptConversationLockIdentity(c *gin.Context, policyContext verifiedNewAPIPolicyContext) (promptConversationLockIdentity, bool) {
	if c == nil || !policyContext.MetaVerified {
		return promptConversationLockIdentity{}, false
	}
	platform := normalizedNewAPIPlatform(policyContext.Platform)
	userID := strings.TrimSpace(policyContext.Identity.UserID)
	fingerprint := strings.ToLower(strings.TrimSpace(policyContext.Meta.SessionFingerprint))
	if platform == "" || userID == "" || len(fingerprint) != 32 {
		return promptConversationLockIdentity{}, false
	}
	digest := sha256.Sum256([]byte("prompt-conversation-lock-v1\x00" + platform + "\x00" + userID + "\x00" + fingerprint))
	return promptConversationLockIdentity{
		LockKey: hex.EncodeToString(digest[:]), Platform: platform, NewAPIUserID: userID,
		SessionFingerprint: fingerprint, SessionHash: hashRiskIdentity(fingerprint),
	}, true
}

func promptConversationLockTTL(cfg promptfilter.Config) time.Duration {
	hours := promptfilter.NormalizeAdvancedConfig(cfg.Advanced).Enforcement.ConversationLockTTLHours
	return time.Duration(hours) * time.Hour
}

func promptConversationLockExpired(item *database.PromptConversationLock, ttl time.Duration) bool {
	return item == nil || (ttl > 0 && !item.LockedAt.After(time.Now().UTC().Add(-ttl)))
}

func promptCyberRestrictionDecision(item *database.PromptConversationLock, cfg promptfilter.Config) promptCyberRestriction {
	result := promptCyberRestriction{
		ReasonCode: promptConversationLockedReasonCode,
		Message:    promptConversationLockedMessage,
		Scope:      database.PromptConversationRestrictionScopeConversation,
	}
	ttl := promptConversationLockTTL(cfg)
	if item != nil {
		result.LockedAt = item.LockedAt.UTC()
		result.IncidentID = strings.TrimSpace(item.IncidentID)
		if item.ReasonCode == promptUserCyberCooldownReasonCode {
			result.ReasonCode = promptUserCyberCooldownReasonCode
			result.Message = promptUserCyberCooldownMessage
			result.Scope = database.PromptConversationRestrictionScopeUserCooldown
			ttl = promptUserCyberCooldownTTL
		}
	}
	if !result.LockedAt.IsZero() && ttl > 0 {
		result.ExpiresAt = result.LockedAt.Add(ttl)
		remaining := time.Until(result.ExpiresAt)
		if remaining > 0 {
			result.RetryAfterSeconds = int64((remaining + time.Second - 1) / time.Second)
		}
	}
	remainingText := promptCyberRestrictionRemainingText(result.RetryAfterSeconds)
	if result.Scope == database.PromptConversationRestrictionScopeUserCooldown {
		result.Message = fmt.Sprintf("该用户因上游 CYB 进入安全冷却，剩余约 %s；冷却期间所有新请求均不会转发，也不会重复累计处罚。管理员可在「Prompt 检查 → 风险画像 → 用户详情」解除冷却。错误码：%s。", remainingText, result.ReasonCode)
	} else {
		result.Message = fmt.Sprintf("当前对话因上游 CYB 已锁定，剩余约 %s；后续请求不会转发或重复累计处罚。管理员可在「Prompt 检查 → 风险画像 → 会话详情」手动解锁。错误码：%s。", remainingText, result.ReasonCode)
	}
	return result
}

func promptCyberRestrictionRemainingText(seconds int64) string {
	if seconds <= 0 {
		return "不到 1 分钟"
	}
	minutes := (seconds + 59) / 60
	if minutes < 60 {
		return fmt.Sprintf("%d 分钟", minutes)
	}
	hours := minutes / 60
	remainingMinutes := minutes % 60
	if remainingMinutes == 0 {
		return fmt.Sprintf("%d 小时", hours)
	}
	return fmt.Sprintf("%d 小时 %d 分钟", hours, remainingMinutes)
}

func promptCyberRestrictionDetails(restriction promptCyberRestriction, signedDetails gin.H) gin.H {
	details := gin.H{}
	for key, value := range signedDetails {
		details[key] = value
	}
	details["reason_code"] = restriction.ReasonCode
	details["restriction_scope"] = restriction.Scope
	details["retry_after_seconds"] = restriction.RetryAfterSeconds
	details["manual_unlock_path"] = "/admin/prompt-filter/profiles"
	if !restriction.LockedAt.IsZero() {
		details["locked_at"] = restriction.LockedAt.Format(time.RFC3339)
	}
	if !restriction.ExpiresAt.IsZero() {
		details["expires_at"] = restriction.ExpiresAt.Format(time.RFC3339)
	}
	if restriction.IncidentID != "" {
		details["incident_id"] = restriction.IncidentID
	}
	return details
}

func promptCyberRestrictionAPIError(restriction promptCyberRestriction, signedDetails gin.H) *api.APIError {
	return api.NewAPIErrorWithDetails(
		api.ErrorCode(restriction.ReasonCode), restriction.Message,
		api.ErrorTypeInvalidRequest, promptCyberRestrictionDetails(restriction, signedDetails),
	)
}

func writePromptCyberRestrictionHeaders(c *gin.Context, restriction promptCyberRestriction) {
	if c == nil {
		return
	}
	c.Header("X-Codex2API-Policy-Restriction-Scope", restriction.Scope)
	if restriction.RetryAfterSeconds > 0 {
		c.Header("Retry-After", strconv.FormatInt(restriction.RetryAfterSeconds, 10))
	}
	if !restriction.ExpiresAt.IsZero() {
		c.Header("X-Codex2API-Policy-Restriction-Expires-At", restriction.ExpiresAt.Format(time.RFC3339))
	}
}

func (h *Handler) sendNewAPIPromptCyberRestriction(c *gin.Context, cfg promptfilter.Config, decision promptfilter.Decision, verdict promptfilter.Verdict, body []byte, endpoint, model string, signedBody []byte, restriction promptCyberRestriction) bool {
	policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, signedBody)
	if !verified {
		return false
	}
	metadata := buildNewAPIPolicyDecisionMetadataWithSecret(policyContext.Identity, decision, verdict, cfg, body, endpoint, model, "", policyContext.VerificationSecret)
	writeNewAPIPolicyDecisionHeaders(c, metadata)
	writePromptCyberRestrictionHeaders(c, restriction)
	apiErr := promptCyberRestrictionAPIError(restriction, newAPIPolicyDecisionDetails(metadata))
	if requestUsesAnthropicErrorEnvelope(c) {
		c.JSON(http.StatusBadRequest, gin.H{"type": "error", "error": gin.H{
			"type": string(apiErr.Type), "message": apiErr.Message, "details": apiErr.Details,
		}})
		return true
	}
	api.SendErrorWithStatus(c, apiErr, http.StatusBadRequest)
	return true
}

func promptCyberRestrictionLock(item *database.PromptConversationLock, exactConversation bool) *database.PromptConversationLock {
	if item == nil {
		return nil
	}
	copy := *item
	if exactConversation {
		copy.ReasonCode = promptConversationLockedReasonCode
	} else {
		copy.ReasonCode = promptUserCyberCooldownReasonCode
	}
	return &copy
}

func (h *Handler) activePromptConversationLock(c *gin.Context, cfg promptfilter.Config, signedBody []byte) (*database.PromptConversationLock, bool) {
	if h == nil || h.db == nil || c == nil || !cfg.Advanced.Enforcement.ConversationLockEnabled {
		return nil, false
	}
	policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, signedBody)
	if !verified {
		return nil, false
	}
	userIdentity, ok := verifiedPromptUserCooldownIdentity(c, policyContext)
	if !ok {
		return nil, false
	}
	conversationIdentity, hasConversationIdentity := verifiedPromptConversationLockIdentity(c, policyContext)
	lockKey := ""
	if hasConversationIdentity {
		lockKey = conversationIdentity.LockKey
	}
	lockTTL := promptConversationLockTTL(cfg)
	if hasConversationIdentity && h.cache != nil {
		if raw, found, err := h.cache.GetRuntime(c.Request.Context(), database.PromptConversationLockCacheNamespace, conversationIdentity.LockKey); err == nil && found {
			var item database.PromptConversationLock
			if json.Unmarshal(raw, &item) == nil && item.Status == database.PromptConversationLockStatusActive && !promptConversationLockExpired(&item, lockTTL) {
				return promptCyberRestrictionLock(&item, true), true
			}
			_ = h.cache.DeleteRuntime(c.Request.Context(), database.PromptConversationLockCacheNamespace, conversationIdentity.LockKey)
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	item, exactConversation, err := h.db.GetActivePromptConversationRestriction(
		ctx, lockKey, userIdentity.Platform, userIdentity.NewAPIUserID,
		lockTTL, promptUserCyberCooldownTTL,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false
	}
	if err != nil {
		log.Printf("check prompt conversation restriction failed identity=%s: %v", userIdentity.LockKey[:12], err)
		return nil, false
	}
	if exactConversation {
		h.cachePromptConversationLock(c.Request.Context(), item, lockTTL)
	}
	return promptCyberRestrictionLock(item, exactConversation), true
}

func (h *Handler) cachePromptConversationLock(ctx context.Context, item *database.PromptConversationLock, lockTTL time.Duration) {
	if h == nil || h.cache == nil || item == nil || item.Status != database.PromptConversationLockStatusActive {
		return
	}
	cacheTTL := promptConversationLockCacheTTL
	if lockTTL > 0 {
		remaining := time.Until(item.LockedAt.Add(lockTTL))
		if remaining <= 0 {
			return
		}
		if remaining < cacheTTL {
			cacheTTL = remaining
		}
	}
	raw, err := json.Marshal(item)
	if err == nil {
		_ = h.cache.SetRuntime(ctx, database.PromptConversationLockCacheNamespace, item.LockKey, raw, cacheTTL)
	}
}

func (h *Handler) lockPromptConversationAfterUpstreamCYB(c *gin.Context, endpoint, model, incidentID string, metadata newAPIPolicyDecisionMetadata) bool {
	if h == nil || h.db == nil || c == nil || metadata.ReasonCode != newAPIUpstreamCyberPolicyReasonCode || metadata.DecisionID == "" {
		return false
	}
	cfg := h.promptFilterConfigForRequest(c)
	if !cfg.Advanced.Enforcement.ConversationLockEnabled {
		return false
	}
	policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, ingressRequestBody(c, nil))
	if !verified {
		return false
	}
	userIdentity, ok := verifiedPromptUserCooldownIdentity(c, policyContext)
	if !ok {
		return false
	}
	identity := userIdentity
	conversationIdentity, hasConversationIdentity := verifiedPromptConversationLockIdentity(c, policyContext)
	if hasConversationIdentity {
		identity = conversationIdentity
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	item, _, err := h.db.LockPromptConversation(ctx, database.PromptConversationLockInput{
		LockKey: identity.LockKey, Platform: identity.Platform, NewAPIUserID: identity.NewAPIUserID,
		SessionFingerprint: identity.SessionFingerprint, SessionHash: identity.SessionHash,
		IncidentID: incidentID, DecisionID: metadata.DecisionID, RequestID: metadata.RequestID,
		ReasonCode: metadata.ReasonCode, Endpoint: endpoint, Model: model, LockedAt: time.Now().UTC(),
	})
	if err != nil {
		log.Printf("persist prompt conversation lock failed decision=%s: %v", metadata.DecisionID, err)
		return false
	}
	if !hasConversationIdentity {
		return false
	}
	h.cachePromptConversationLock(c.Request.Context(), item, promptConversationLockTTL(cfg))
	return item != nil && item.Status == database.PromptConversationLockStatusActive
}

func (h *Handler) rejectLockedPromptConversation(c *gin.Context, cfg promptfilter.Config, signedBody, responseBody []byte, endpoint, model string) bool {
	item, locked := h.activePromptConversationLock(c, cfg, signedBody)
	if !locked {
		return false
	}
	restriction := promptCyberRestrictionDecision(item, cfg)
	profile := strings.ToLower(strings.TrimSpace(cfg.Advanced.Guard.DefaultProfile))
	switch profile {
	case promptfilter.GuardProfileBalanced, promptfilter.GuardProfileStrict, promptfilter.GuardProfileResearch:
	default:
		profile = promptfilter.GuardProfileBalanced
	}
	decision := promptfilter.Decision{
		Action: promptfilter.ActionBlock, Profile: profile,
		ReasonCode: restriction.ReasonCode, StrikeEligible: false, Terminal: true,
	}
	verdict := promptfilter.Verdict{Action: promptfilter.ActionBlock, Reason: restriction.Message, FullText: restriction.ReasonCode}
	if h.sendNewAPIPromptCyberRestriction(c, cfg, decision, verdict, responseBody, endpoint, model, signedBody, restriction) {
		return true
	}
	writePromptCyberRestrictionHeaders(c, restriction)
	apiErr := promptCyberRestrictionAPIError(restriction, nil)
	if requestUsesAnthropicErrorEnvelope(c) {
		c.JSON(http.StatusBadRequest, gin.H{"type": "error", "error": gin.H{
			"type": string(apiErr.Type), "message": apiErr.Message, "details": apiErr.Details,
		}})
		return true
	}
	api.SendErrorWithStatus(c, apiErr, http.StatusBadRequest)
	return true
}
