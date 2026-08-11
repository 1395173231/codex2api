package proxy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

const (
	promptConversationLockedReasonCode = "conversation_cyber_locked"
	promptConversationLockedMessage    = "此对话因触发网络安全策略（CYB）已被锁定，请新建对话后继续。本次锁定拦截不会重复累计；在新对话中再次触发 CYB 可能会停用账号。如果确认是误判，请联系管理员解锁。"
	promptUserCyberCooldownReasonCode  = "user_cyber_cooldown"
	promptUserCyberCooldownMessage     = "该用户刚刚触发网络安全策略（CYB），现处于 30 分钟安全冷却期。冷却期间的新请求不会继续转发，也不会重复累计处罚；如确认是误判，请联系管理员处理。"
	promptConversationLockCacheTTL     = 30 * time.Second
	promptUserCyberCooldownTTL         = 30 * time.Minute
)

type promptConversationLockIdentity struct {
	LockKey            string
	Platform           string
	NewAPIUserID       string
	SessionFingerprint string
	SessionHash        string
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

func promptCyberRestrictionDecision(item *database.PromptConversationLock) (string, string) {
	if item != nil && item.ReasonCode == promptUserCyberCooldownReasonCode {
		return promptUserCyberCooldownReasonCode, promptUserCyberCooldownMessage
	}
	return promptConversationLockedReasonCode, promptConversationLockedMessage
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
	identity, ok := verifiedPromptConversationLockIdentity(c, policyContext)
	if !ok {
		return nil, false
	}
	lockTTL := promptConversationLockTTL(cfg)
	if h.cache != nil {
		if raw, found, err := h.cache.GetRuntime(c.Request.Context(), database.PromptConversationLockCacheNamespace, identity.LockKey); err == nil && found {
			var item database.PromptConversationLock
			if json.Unmarshal(raw, &item) == nil && item.Status == database.PromptConversationLockStatusActive && !promptConversationLockExpired(&item, lockTTL) {
				return promptCyberRestrictionLock(&item, true), true
			}
			_ = h.cache.DeleteRuntime(c.Request.Context(), database.PromptConversationLockCacheNamespace, identity.LockKey)
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	item, exactConversation, err := h.db.GetActivePromptConversationRestriction(
		ctx, identity.LockKey, identity.Platform, identity.NewAPIUserID,
		lockTTL, promptUserCyberCooldownTTL,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false
	}
	if err != nil {
		log.Printf("check prompt conversation lock failed key=%s: %v", identity.LockKey[:12], err)
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
	identity, ok := verifiedPromptConversationLockIdentity(c, policyContext)
	if !ok {
		return false
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
	h.cachePromptConversationLock(c.Request.Context(), item, promptConversationLockTTL(cfg))
	return item != nil && item.Status == database.PromptConversationLockStatusActive
}

func (h *Handler) rejectLockedPromptConversation(c *gin.Context, cfg promptfilter.Config, signedBody, responseBody []byte, endpoint, model string) bool {
	item, locked := h.activePromptConversationLock(c, cfg, signedBody)
	if !locked {
		return false
	}
	reasonCode, message := promptCyberRestrictionDecision(item)
	profile := strings.ToLower(strings.TrimSpace(cfg.Advanced.Guard.DefaultProfile))
	switch profile {
	case promptfilter.GuardProfileBalanced, promptfilter.GuardProfileStrict, promptfilter.GuardProfileResearch:
	default:
		profile = promptfilter.GuardProfileBalanced
	}
	decision := promptfilter.Decision{
		Action: promptfilter.ActionBlock, Profile: profile,
		ReasonCode: reasonCode, StrikeEligible: false, Terminal: true,
	}
	verdict := promptfilter.Verdict{Action: promptfilter.ActionBlock, Reason: message, FullText: reasonCode}
	if h.sendNewAPIPolicyDecision(c, cfg, decision, verdict, responseBody, endpoint, model, signedBody) {
		return true
	}
	if requestUsesAnthropicErrorEnvelope(c) {
		sendAnthropicError(c, http.StatusBadRequest, "invalid_request_error", message)
		return true
	}
	api.SendErrorWithStatus(c, api.NewAPIError(api.ErrorCode(reasonCode), message, api.ErrorTypeInvalidRequest), http.StatusBadRequest)
	return true
}
