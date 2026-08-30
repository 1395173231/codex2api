package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/telemetry"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type recordedAnalyticsCall struct {
	credential telemetry.Credential
	event      telemetry.Event
}

type recordingAnalyticsSink struct {
	calls []recordedAnalyticsCall
}

func (s *recordingAnalyticsSink) Enqueue(credential telemetry.Credential, event telemetry.Event) bool {
	s.calls = append(s.calls, recordedAnalyticsCall{credential: credential, event: event})
	return true
}

func newAnalyticsTestHandler(t *testing.T, sink telemetry.Sink) *Handler {
	t.Helper()
	previousRuntime := CurrentRuntimeSettings()
	enabledRuntime := previousRuntime
	enabledRuntime.CodexAnalyticsEnabled = true
	ApplyRuntimeSettings(enabledRuntime)
	t.Cleanup(func() { ApplyRuntimeSettings(previousRuntime) })
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{
		DBID:        1,
		AccessToken: "account-access-secret",
		AccountID:   "workspace-1",
		PlanType:    "plus",
	})
	handler := NewHandler(store, nil, nil, nil)
	handler.SetAnalyticsReporter(sink)
	return handler
}

func TestAnalyticsAggregatesSamplingRequestsWithConvergedFingerprint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sink := &recordingAnalyticsSink{}
	handler := newAnalyticsTestHandler(t, sink)
	account := handler.store.FindByID(1)
	account.CodexFingerprintMode = auth.CodexFingerprintModeSession

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Request.Header.Set("Connection", "Upgrade")
	c.Request.Header.Set("Upgrade", "websocket")
	c.Request.Header.Set("Session_id", "client-session")
	c.Request.Header.Set("X-Codex-Turn-Metadata", `{"session_id":"client-session","thread_id":"client-thread","turn_id":"turn-real"}`)
	rawBody := []byte(`{"model":"gpt-5.6-sol","client_metadata":{"x-codex-installation-id":"client-install","session_id":"client-session","thread_id":"client-thread","turn_id":"turn-real","root_turn_id":"root-real","x-codex-turn-metadata":"{\"session_id\":\"client-session\",\"thread_id\":\"client-thread\",\"turn_id\":\"turn-real\"}"}}`)
	setRawRequestBody(c, rawBody)
	wantIDs := resolveCodexFingerprintIDs(account, c.Request.Header)

	resetAnalyticsResponseObservation(c)
	outboundBody := ApplyCodexFingerprintToBody(rawBody, account, c.Request.Header)
	outboundHeaders := c.Request.Header.Clone()
	ApplyCodexFingerprintHeaders(outboundHeaders, account, c.Request.Header)
	observeAnalyticsOutboundRequest(c.Request.Context(), outboundBody, outboundHeaders)
	observeAnalyticsResponsePayload(c, []byte(`{"type":"response.output_item.done","item":{"type":"custom_tool_call","id":"tool-1"}}`))
	observeAnalyticsResponsePayload(c, []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[]}}`))
	handler.captureAnalyticsTurn(c, &database.UsageLogInput{
		AccountID: 1, InboundEndpoint: "/v1/responses", Model: "gpt-5.6-sol", StatusCode: http.StatusOK,
		InputTokens: 10, OutputTokens: 2, TotalTokens: 12,
	})
	if len(sink.calls) != 0 {
		t.Fatalf("first sampling emitted %d events, want aggregate pending", len(sink.calls))
	}

	resetAnalyticsResponseObservation(c)
	observeAnalyticsOutboundRequest(c.Request.Context(), outboundBody, outboundHeaders)
	observeAnalyticsResponsePayload(c, []byte(`{"type":"response.completed","response":{"id":"resp-2","output":[]}}`))
	handler.captureAnalyticsTurn(c, &database.UsageLogInput{
		AccountID: 1, InboundEndpoint: "/v1/responses", Model: "gpt-5.6-sol", StatusCode: http.StatusOK,
		InputTokens: 7, OutputTokens: 3, TotalTokens: 10,
	})
	if len(sink.calls) != 1 {
		t.Fatalf("analytics calls = %d, want one turn event", len(sink.calls))
	}
	wire, err := json.Marshal(sink.calls[0].event)
	if err != nil {
		t.Fatal(err)
	}
	params := gjson.GetBytes(wire, "event_params")
	for field, want := range map[string]string{
		"thread_id": wantIDs.threadID, "session_id": wantIDs.sessionID, "turn_id": "turn-real", "root_turn_id": "root-real",
	} {
		if got := params.Get(field).String(); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
	if params.Get("thread_id").String() == "client-thread" || params.Get("session_id").String() == "client-session" {
		t.Fatalf("event retained pre-convergence identity: %s", wire)
	}
	for field, want := range map[string]int64{
		"sampling_request_count": 2, "input_tokens": 17, "output_tokens": 5, "total_tokens": 22,
		"total_tool_call_count": 1, "dynamic_tool_call_count": 1,
	} {
		if got := params.Get(field).Int(); got != want {
			t.Errorf("%s = %d, want %d", field, got, want)
		}
	}
}

func TestAnalyticsMiddlewareEmitsOnlyFinalAllowlistedTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sink := &recordingAnalyticsSink{}
	handler := newAnalyticsTestHandler(t, sink)
	router := gin.New()
	router.Use(handler.AnalyticsMiddleware())
	router.POST("/v1/responses", func(c *gin.Context) {
		c.Set("x-account-proxy", "http://proxy-user:proxy-secret@proxy.example:8080")
		handler.logUsageForRequest(c, &database.UsageLogInput{
			AccountID: 1, InboundEndpoint: "/v1/responses", Model: "gpt-5.6-sol",
			StatusCode: http.StatusBadGateway, DurationMs: 5, AttemptIndex: 1,
			IsRetryAttempt: true, UpstreamErrorKind: "transport_reset",
		})
		handler.logUsageForRequest(c, &database.UsageLogInput{
			AccountID: 1, InboundEndpoint: "/v1/responses", Model: "gpt-5.6-sol",
			EffectiveModel: "gpt-5.6-sol", StatusCode: http.StatusOK, DurationMs: 12,
			AttemptIndex: 2, InputTokens: 100, CachedTokens: 40, OutputTokens: 20,
			ReasoningTokens: 5, TotalTokens: 120, ReasoningEffort: "high", ActualServiceTier: "priority",
		})
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"secret-prompt"}`))
	req.Header.Set("Authorization", "Bearer downstream-api-secret")
	req.Header.Set("User-Agent", "private-client-user-agent")
	req.RemoteAddr = "203.0.113.9:4321"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("analytics calls = %d, want 1", len(sink.calls))
	}
	call := sink.calls[0]
	if call.credential.AccessToken != "account-access-secret" || call.credential.AccountID != "workspace-1" {
		t.Fatalf("credential routing mismatch: %#v", call.credential)
	}
	if call.credential.ProxyURL != "http://proxy-user:proxy-secret@proxy.example:8080" {
		t.Fatalf("proxy routing mismatch: %q", call.credential.ProxyURL)
	}

	wire, err := json.Marshal(call.event)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret-prompt", "downstream-api-secret", "private-client-user-agent", "203.0.113.9", "account-access-secret", "workspace-1", "proxy-secret"} {
		if strings.Contains(string(wire), forbidden) {
			t.Fatalf("analytics event leaked %q: %s", forbidden, wire)
		}
	}
	if got := gjson.GetBytes(wire, "event_type").String(); got != "codex_turn_event" {
		t.Fatalf("event_type = %q", got)
	}
	if got := gjson.GetBytes(wire, "event_params.status").String(); got != "completed" {
		t.Fatalf("status = %q, want completed", got)
	}
	if got := gjson.GetBytes(wire, "event_params.model").String(); got != "gpt-5.6-sol" {
		t.Fatalf("model = %q", got)
	}
	if got := gjson.GetBytes(wire, "event_params.sampling_retry_count").Int(); got != 1 {
		t.Fatalf("sampling_retry_count = %d, want 1", got)
	}
	if gjson.GetBytes(wire, "event_params.codex_error_http_status_code").Exists() || gjson.GetBytes(wire, "event_params.codex_error_kind").Exists() {
		t.Fatalf("successful event contains error metadata: %s", wire)
	}
}

func TestAnalyticsWebSocketEmitsEachTerminalTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sink := &recordingAnalyticsSink{}
	handler := newAnalyticsTestHandler(t, sink)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Request.Header.Set("Connection", "Upgrade")
	c.Request.Header.Set("Upgrade", "websocket")

	handler.captureAnalyticsTurn(c, &database.UsageLogInput{
		AccountID: 1, InboundEndpoint: "/v1/responses", Model: "gpt-5.6-sol",
		StatusCode: http.StatusBadGateway, AttemptIndex: 1, IsRetryAttempt: true,
	})
	handler.captureAnalyticsTurn(c, &database.UsageLogInput{
		AccountID: 1, InboundEndpoint: "/v1/responses", Model: "gpt-5.6-sol",
		StatusCode: http.StatusOK, AttemptIndex: 2,
	})
	handler.captureAnalyticsTurn(c, &database.UsageLogInput{
		AccountID: 1, InboundEndpoint: "/v1/responses", Model: "gpt-5.6-sol",
		StatusCode: http.StatusOK, AttemptIndex: 1,
	})

	if len(sink.calls) != 2 {
		t.Fatalf("terminal WebSocket analytics calls = %d, want 2", len(sink.calls))
	}
}
