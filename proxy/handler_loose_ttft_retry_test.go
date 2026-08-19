package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func newAttemptSequenceSSEServer(t *testing.T, attempts [][]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempt := int(calls.Add(1)) - 1
		if attempt >= len(attempts) {
			attempt = len(attempts) - 1
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range attempts[attempt] {
			_, _ = io.WriteString(w, "data: "+event+"\n\n")
		}
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func enableLooseResponseFailedContinuousRetry(t *testing.T) {
	t.Helper()
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	next := previous
	next.FirstTokenMode = FirstTokenModeLoose
	next.CodexPreflightSSEPassthrough = false
	next.CodexWSSilentRetry = false
	next.CodexWSSilentRetries = 0
	next.CodexWSHideErrors = false
	next.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryResponseFailed},
	}
	ApplyRuntimeSettings(next)
}

func looseTTFTFailureThenSuccessEvents(successText string) [][]string {
	return [][]string{
		{
			`{"type":"response.created","response":{"id":"resp_retry_1"}}`,
			`{"type":"response.output_item.added","item":{"type":"reasoning"}}`,
			`{"type":"response.failed","response":{"status":"failed","status_code":503,"error":{"code":"server_error","message":"temporary upstream failure"}}}`,
		},
		{
			`{"type":"response.created","response":{"id":"resp_retry_2"}}`,
			`{"type":"response.output_text.delta","delta":"` + successText + `"}`,
			`{"type":"response.completed","response":{"id":"resp_retry_2","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		},
	}
}

func TestChatCompletionsLooseTTFTDoesNotBlockPreContentContinuousRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableLooseResponseFailedContinuousRetry(t)

	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })
	upstream, calls := newAttemptSequenceSSEServer(t, looseTTFTFailureThenSuccessEvents("recovered-chat"))
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      1,
		TestConcurrency:     1,
		TestModel:           "gpt-5.4",
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	recorder := invokeChatCompletionsStream(t, handler)
	body := recorder.Body.String()
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 after a pre-content response.failed retry; body=%q", got, body)
	}
	if recorder.Code != http.StatusOK || !strings.Contains(body, `"content":"recovered-chat"`) {
		t.Fatalf("recovered chat response missing: status=%d body=%q", recorder.Code, body)
	}
	if strings.Contains(body, "temporary upstream failure") || strings.Count(body, "data: [DONE]\n\n") != 1 {
		t.Fatalf("failed attempt leaked or success terminal is invalid: %q", body)
	}
}

func TestRelayResponsesLooseTTFTDefersMetadataForContinuousRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableLooseResponseFailedContinuousRetry(t)

	attempts := looseTTFTFailureThenSuccessEvents("recovered-responses")
	attempts[0][1] = `{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":10}}}`
	upstream, calls := newAttemptSequenceSSEServer(t, attempts)
	store := newOpenAIResponsesRelayStore(upstream.URL)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, nil, nil, nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(
		`{"model":"gpt-4.1-direct","input":"hello","stream":true}`,
	))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.Responses(ctx)

	body := recorder.Body.String()
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 after buffered preflight metadata; body=%q", got, body)
	}
	if recorder.Code != http.StatusOK || !strings.Contains(body, "recovered-responses") {
		t.Fatalf("recovered Responses stream missing: status=%d body=%q", recorder.Code, body)
	}
	if strings.Contains(body, "temporary upstream failure") || strings.Contains(body, "codex.rate_limits") {
		t.Fatalf("buffered failed-attempt events leaked downstream: %q", body)
	}
}

func TestResponsesWebSocketLooseTTFTDefersPreContentEventsForContinuousRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableLooseResponseFailedContinuousRetry(t)

	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })
	attempts := [][]string{
		{
			`{"type":"response.created","response":{"id":"resp_retry_1"}}`,
			`{"type":"codex.rate_limits","plan_type":"plus"}`,
			`{"type":"response.failed","response":{"status":"failed","status_code":503,"error":{"code":"server_error","message":"temporary upstream failure"}}}`,
		},
		{
			`{"type":"response.created","response":{"id":"resp_retry_2"}}`,
			`{"type":"response.output_text.delta","delta":"recovered-websocket"}`,
			`{"type":"response.completed","response":{"id":"resp_retry_2","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		},
	}
	var calls atomic.Int32
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		attempt := int(calls.Add(1)) - 1
		if attempt >= len(attempts) {
			attempt = len(attempts) - 1
		}
		var sse strings.Builder
		for _, event := range attempts[attempt] {
			sse.WriteString("data: ")
			sse.WriteString(event)
			sse.WriteString("\n\n")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(sse.String())),
		}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      1,
		TestConcurrency:     1,
		TestModel:           "gpt-5.4",
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		if response != nil {
			t.Fatalf("dial Responses websocket: %v status=%d", err, response.StatusCode)
		}
		t.Fatalf("dial Responses websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.4","input":"hello"}`)); err != nil {
		t.Fatalf("write Responses websocket request: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var downstream strings.Builder
	terminalType := ""
	for i := 0; i < 8; i++ {
		_, event, readErr := conn.ReadMessage()
		if readErr != nil {
			t.Fatalf("read Responses websocket event: %v; downstream=%s", readErr, downstream.String())
		}
		downstream.Write(event)
		eventType := gjson.GetBytes(event, "type").String()
		if eventType == "response.completed" || eventType == "response.failed" || eventType == "error" {
			terminalType = eventType
			break
		}
	}

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 after a pre-content response.failed retry; downstream=%s", got, downstream.String())
	}
	if terminalType != "response.completed" {
		t.Fatalf("terminal event = %q, want response.completed; downstream=%s", terminalType, downstream.String())
	}
	if body := downstream.String(); !strings.Contains(body, "recovered-websocket") || strings.Contains(body, "temporary upstream failure") {
		t.Fatalf("Responses websocket retry was not transparent: %s", body)
	}
}
