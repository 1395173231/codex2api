package proxy

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

const promptRiskSessionObservationContextKey = "prompt_risk_session_observation_recorded"
const promptRiskSessionObservationNamespace = "prompt-risk-session-observation"

// observePromptRiskSession creates a zero-score profile observation after the
// gateway has verified the signed identity. It is deliberately asynchronous and
// best-effort: a database hiccup must never delay or block account dispatch.
func (h *Handler) observePromptRiskSession(c *gin.Context, cfg promptfilter.Config, signedBody []byte, endpoint, model string) {
	if h == nil || h.db == nil || c == nil || !cfg.Advanced.NewAPI.Enabled {
		return
	}
	if c.GetBool(promptRiskSessionObservationContextKey) {
		return
	}
	c.Set(promptRiskSessionObservationContextKey, true)
	policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, signedBody)
	if !verified || !isSub2APIPlatform(policyContext.Platform) || !policyContext.MetaVerified || strings.TrimSpace(policyContext.Identity.UserID) == "" || strings.TrimSpace(policyContext.Platform) == "" {
		return
	}

	clientIP := strings.TrimSpace(policyContext.Identity.ClientIP)
	if clientIP == "" {
		clientIP = strings.TrimSpace(c.ClientIP())
	}
	sessionFingerprint := strings.TrimSpace(policyContext.Meta.SessionFingerprint)
	if sessionFingerprint == "" {
		return
	}
	if h.cache == nil {
		return
	}
	markerKey := newAPIRuntimeScope(policyContext.APIKeyID, policyContext.Platform) + ":" + hashRiskIdentity(policyContext.Identity.UserID+"\x00"+sessionFingerprint)
	observationTTL := time.Duration(cfg.Advanced.Session.WindowSeconds) * time.Second
	if observationTTL <= 0 {
		observationTTL = time.Hour
	}
	ctx := promptGuardRequestContext(c)
	unlock, acquired := acquirePromptRuntimeLease(ctx, h.cache, promptRiskSessionObservationNamespace, markerKey)
	if !acquired {
		return
	}
	defer unlock()
	if _, exists, err := h.cache.GetRuntime(ctx, promptRiskSessionObservationNamespace, markerKey); err != nil || exists {
		return
	}
	if err := h.cache.SetRuntime(ctx, promptRiskSessionObservationNamespace, markerKey, json.RawMessage(`1`), observationTTL); err != nil {
		return
	}
	observation := database.PromptRiskSessionObservation{
		RequestCorrelationID: ensurePromptPolicyRequestCorrelationID(c),
		Platform:             policyContext.Platform,
		ExternalUserID:       policyContext.Identity.UserID,
		SessionHash:          hashRiskIdentity(sessionFingerprint),
		ClientIPHash:         hashRiskIdentity(clientIP),
		Endpoint:             endpoint,
		Model:                model,
	}
	observation.UserName = policyContext.Meta.UserName
	observation.UserEmail = policyContext.Meta.UserEmail
	observation.UserGroup = policyContext.Meta.UserGroup
	if value, exists := c.Get(contextAPIKeyID); exists {
		switch typed := value.(type) {
		case int64:
			observation.APIKeyID = typed
		case int:
			observation.APIKeyID = int64(typed)
		}
	}
	if value, exists := c.Get(contextAPIKeyName); exists {
		if name, ok := value.(string); ok {
			observation.APIKeyName = name
		}
	}
	if value, exists := c.Get(contextAPIKeyMasked); exists {
		if masked, ok := value.(string); ok {
			observation.APIKeyMasked = masked
		}
	}
	h.db.RunBackgroundTask(func(ctx context.Context) {
		if err := h.db.RecordPromptRiskSessionObservation(ctx, observation); err != nil {
			log.Printf("record prompt risk session observation failed: %v", err)
		}
	})
}
