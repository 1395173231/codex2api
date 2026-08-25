package proxy

import (
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
)

func TestSub2APIHeaderNamespaceReusesSignedRiskProfile(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":"safe request"}`)
	secret := "sub2api-integration-secret-0123456789"
	handler := newPromptFilterBindingTestHandler(t, promptGuardTestConfig(), []database.PromptFilterNewAPIBinding{{
		APIKeyID: 101, PlatformCode: "sub2api", Secret: secret, Enabled: true, RequireSignedIdentity: true,
	}})
	c := signedBoundNewAPIPolicyContext(t, "sub2api-alias-request", newAPIIdentity{UserID: "user-42", ClientIP: "203.0.113.42"}, body, 101, "sub2api", secret, "0123456789abcdef0123456789abcdef")
	for _, suffix := range []string{
		"User-ID", "Client-IP", "Request-ID", "Timestamp", "Method", "Path",
		"Body-SHA256", "Signature", "Policy-Meta", "Policy-Meta-Signature",
	} {
		value := c.GetHeader("X-NewAPI-" + suffix)
		c.Request.Header.Del("X-NewAPI-" + suffix)
		c.Request.Header.Set("X-Sub2API-"+suffix, value)
	}
	policy, ok := handler.verifyNewAPIPolicyContext(c, promptfilter.NewAPIConfig{Enabled: true, MaxClockSkewSeconds: 300}, body)
	if !ok {
		t.Fatal("Sub2API namespaced signed context was rejected")
	}
	if policy.Identity.UserID != "user-42" || policy.Platform != "sub2api" || !policy.MetaVerified {
		t.Fatalf("unexpected verified Sub2API context: %+v", policy)
	}
	audit := handler.capturePromptFilterAuditContext(c)
	if audit.NewAPIPolicyStatus != "verified" || audit.NewAPIPlatform != "sub2api" || audit.NewAPIUserID != "user-42" {
		t.Fatalf("Sub2API identity did not reach risk audit context: %+v", audit)
	}
	if audit.SessionHash == "" || audit.ClientIPHash == "" {
		t.Fatalf("Sub2API session/client identity was not hashed into audit context: %+v", audit)
	}
}

func TestSub2APIHeaderNamespaceRejectsConflictingCanonicalHeaders(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":"safe request"}`)
	secret := "sub2api-conflict-secret-0123456789"
	handler := newPromptFilterBindingTestHandler(t, promptGuardTestConfig(), []database.PromptFilterNewAPIBinding{{
		APIKeyID: 101, PlatformCode: "sub2api", Secret: secret, Enabled: true, RequireSignedIdentity: true,
	}})
	c := signedBoundNewAPIPolicyContext(t, "sub2api-conflict-request", newAPIIdentity{UserID: "user-42", ClientIP: "203.0.113.42"}, body, 101, "sub2api", secret, "0123456789abcdef0123456789abcdef")
	c.Request.Header.Set("X-Sub2API-User-ID", "different-user")
	if _, ok := handler.verifyNewAPIPolicyContext(c, promptfilter.NewAPIConfig{Enabled: true, MaxClockSkewSeconds: 300}, body); ok {
		t.Fatal("conflicting canonical and Sub2API identity headers were accepted")
	}
}

func TestSub2APISignedRequestCreatesObservationAtIngress(t *testing.T) {
	handler, db := newPromptConversationLockTestHandler(t)
	secret := "sub2api-observation-secret-0123456789"
	handler.store.ReplacePromptFilterNewAPIBindings([]*database.PromptFilterNewAPIBinding{{
		APIKeyID: 101, PlatformCode: "sub2api", Secret: secret, Enabled: true, RequireSignedIdentity: true,
	}})
	body := []byte(`{"model":"gpt-5.5","input":"safe request"}`)
	c := signedBoundNewAPIPolicyContext(t, "sub2api-observation-request", newAPIIdentity{UserID: "user-100", ClientIP: "203.0.113.100"}, body, 101, "sub2api", secret, "0123456789abcdef0123456789abcdef")
	cfg := handler.promptFilterConfigForRequest(c)
	handler.observePromptRiskSession(c, cfg, body, "/v1/responses", "gpt-5.5")
	second := signedBoundNewAPIPolicyContext(t, "sub2api-observation-request-2", newAPIIdentity{UserID: "user-100", ClientIP: "203.0.113.100"}, body, 101, "sub2api", secret, "0123456789abcdef0123456789abcdef")
	handler.observePromptRiskSession(second, handler.promptFilterConfigForRequest(second), body, "/v1/responses", "gpt-5.5")
	if !db.DrainBackgroundTasks(2 * time.Second) {
		t.Fatal("background risk observation did not drain")
	}
	profiles, total, err := db.ListPromptRiskProfiles(t.Context(), database.PromptRiskProfileQuery{Page: 1, PageSize: 20, Platform: "sub2api"})
	if err != nil || total != 4 || len(profiles) != 4 {
		t.Fatalf("ingress observation profiles total=%d items=%#v err=%v", total, profiles, err)
	}
	var user *database.PromptRiskProfile
	for _, profile := range profiles {
		if profile.SubjectType == database.PromptRiskSubjectNewAPIUser {
			user = profile
			break
		}
	}
	if user == nil || user.NewAPIUserID != "user-100" || user.RiskScore != 0 || user.EventCount != 1 {
		t.Fatalf("ingress observed user profile=%#v", user)
	}
}
