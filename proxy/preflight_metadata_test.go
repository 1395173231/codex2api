package proxy

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestPreflightMetadataClientBoundary(t *testing.T) {
	cases := []struct {
		name        string
		eventType   string
		seen        bool
		terminal    bool
		passthrough bool
		wantHide    bool
		wantPing    bool
	}{
		{name: "rate limits", eventType: "codex.rate_limits", passthrough: true, wantHide: true, wantPing: true},
		{name: "codex metadata", eventType: "codex.response.metadata", passthrough: true, wantHide: true, wantPing: true},
		{name: "http metadata", eventType: "response.metadata", passthrough: true, wantHide: true, wantPing: true},
		{name: "case-insensitive metadata", eventType: "CODEX.RATE_LIMITS", passthrough: true, wantHide: true, wantPing: true},
		{name: "disabled passthrough", eventType: "codex.rate_limits", passthrough: false, wantHide: true},
		{name: "after content", eventType: "codex.rate_limits", seen: true, passthrough: true, wantHide: true},
		{name: "after terminal", eventType: "codex.rate_limits", terminal: true, passthrough: true, wantHide: true},
		{name: "lifecycle", eventType: "response.created", passthrough: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldHidePreflightSSEEventFromClient(tc.eventType); got != tc.wantHide {
				t.Fatalf("shouldHidePreflightSSEEventFromClient(%q) = %t, want %t", tc.eventType, got, tc.wantHide)
			}
			if got := shouldEmitPreflightSSEPing(tc.eventType, tc.seen, tc.terminal, tc.passthrough); got != tc.wantPing {
				t.Fatalf("shouldEmitPreflightSSEPing(%q) = %t, want %t", tc.eventType, got, tc.wantPing)
			}
		})
	}
	if preflightSSEPingComment != ": ping\n\n" {
		t.Fatalf("preflight comment = %q, want fixed SSE ping comment", preflightSSEPingComment)
	}
	if !shouldHidePreflightSSEEventFromClient("error", []byte(`{"type":"codex.rate_limits","plan_type":"pro"}`)) {
		t.Fatal("payload-level codex metadata should still be hidden when the SSE event name is non-metadata")
	}
}

func TestWriteSSECommentImmediateDrainsScannerAndCoalescedData(t *testing.T) {
	var out bytes.Buffer
	w := &streamFlushWriter{writer: &out, policy: StreamFlushPolicyCoalesce, interval: time.Hour}
	w.lastFlush = time.Now()
	if err := w.WriteSSEData([]byte(`{"type":"response.created"}`)); err != nil {
		t.Fatalf("WriteSSEData: %v", err)
	}
	if err := w.WriteSSECommentImmediate(preflightSSEPingComment); err != nil {
		t.Fatalf("WriteSSECommentImmediate: %v", err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "data: {\"type\":\"response.created\"}\n\n: ping\n\n") {
		t.Fatalf("immediate comment reordered output: %q", got)
	}
}

func TestForwardGrokNativeResponsesHidesPreflightMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	next := previous
	next.CodexPreflightSSEPassthrough = true
	next.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(next)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := "data: {\"type\":\"codex.rate_limits\",\"plan_type\":\"pro\",\"rate_limits\":{\"primary\":{\"used_percent\":33}}}\n\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n" +
		"data: {\"type\":\"codex.response.metadata\",\"headers\":{\"x-account-email\":\"secret@example.com\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\"}}\n\n"
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}

	_, outcome, wrote, _ := forwardGrokNativeResponse(ctx, resp, GrokProtocolResponses, true, time.Now(), nil)
	if outcome.logStatusCode != http.StatusOK || !wrote {
		t.Fatalf("outcome/wrote = %#v %v", outcome, wrote)
	}
	got := recorder.Body.String()
	if strings.Contains(got, "codex.rate_limits") || strings.Contains(got, "codex.response.metadata") || strings.Contains(got, "secret@example.com") {
		t.Fatalf("preflight metadata leaked: %q", got)
	}
	if strings.Count(got, preflightSSEPingComment) != 1 {
		t.Fatalf("preflight ping count = %d, body=%q", strings.Count(got, preflightSSEPingComment), got)
	}
	if !strings.Contains(got, `data: {"type":"response.created",`) || !strings.Contains(got, `data: {"type":"response.output_text.delta"`) {
		t.Fatalf("real response events missing: %q", got)
	}
}

func TestForwardGrokNativeResponsesSuppressesPreflightMetadataWhenPassthroughDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	next := previous
	next.CodexPreflightSSEPassthrough = false
	next.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(next)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := "data: {\"type\":\"codex.rate_limits\",\"plan_type\":\"pro\"}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
	_, outcome, _, _ := forwardGrokNativeResponse(ctx, resp, GrokProtocolResponses, true, time.Now(), nil)
	if outcome.logStatusCode != http.StatusOK || strings.Contains(recorder.Body.String(), "codex.rate_limits") || strings.Contains(recorder.Body.String(), preflightSSEPingComment) {
		t.Fatalf("disabled passthrough still exposed preflight data: outcome=%#v body=%q", outcome, recorder.Body.String())
	}
}

func TestResponsesRelayHidesPreflightMetadataAndEmitsPing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	next := previous
	next.CodexPreflightSSEPassthrough = true
	next.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	next.StreamFlushPolicy = StreamFlushPolicyImmediate
	ApplyRuntimeSettings(next)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"codex.rate_limits","plan_type":"pro","rate_limits":{"primary":{"used_percent":33}}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp_safe"}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"codex.response.metadata","headers":{"x-account-email":"secret@example.com"}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.metadata","metadata":{"account":"secret-account"}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"safe"}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_safe","status":"completed"}}`+"\n\n")
	}))
	t.Cleanup(upstream.Close)

	store := newOpenAIResponsesRelayStore(upstream.URL)
	t.Cleanup(store.Stop)
	h := NewHandler(store, nil, nil, nil)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4.1-direct","input":"hello","stream":true}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.Responses(ctx)

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", recorder.Code, body)
	}
	if strings.Contains(body, "codex.rate_limits") || strings.Contains(body, "codex.response.metadata") || strings.Contains(body, "secret@example.com") || strings.Contains(body, "secret-account") {
		t.Fatalf("preflight metadata leaked through Responses relay: %q", body)
	}
	if strings.Count(body, preflightSSEPingComment) != 1 || !strings.Contains(body, `"delta":"safe"`) {
		t.Fatalf("preflight ping or real output missing: %q", body)
	}
}

func TestResponsesDirectCodexHidesPreflightMetadataAndEmitsPing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	next := previousSettings
	next.CodexPreflightSSEPassthrough = true
	next.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	next.CodexForceWebsocket = false
	next.StreamFlushPolicy = StreamFlushPolicyImmediate
	ApplyRuntimeSettings(next)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"codex.rate_limits","plan_type":"pro","rate_limits":{"primary":{"used_percent":33}}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp_direct_safe"}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"direct-safe"}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_direct_safe","status":"completed"}}`+"\n\n")
	}))
	t.Cleanup(upstream.Close)
	useDirectCodexUpstream(t, upstream.URL)

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4", MaxRetries: 0, MaxRateLimitRetries: 0})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	t.Cleanup(store.Stop)
	h := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hello","stream":true}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.Responses(ctx)

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, `"delta":"direct-safe"`) {
		t.Fatalf("direct Responses output missing: status=%d body=%q", recorder.Code, body)
	}
	if strings.Contains(body, "codex.rate_limits") || strings.Contains(body, "plan_type") || strings.Count(body, preflightSSEPingComment) != 1 {
		t.Fatalf("direct preflight metadata was not safely replaced: %q", body)
	}
}

func TestResponsesNativeWebSocketHidesPreflightMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousExec := WebsocketExecuteFunc
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		ApplyRuntimeSettings(previousSettings)
	})
	next := previousSettings
	next.CodexPreflightSSEPassthrough = true
	next.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	next.CodexWSSilentRetry = false
	next.CodexWSSilentRetries = 0
	ApplyRuntimeSettings(next)

	WebsocketExecuteFunc = func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header, string) (*http.Response, error) {
		body := "data: {\"type\":\"codex.rate_limits\",\"plan_type\":\"pro\"}\n\n" +
			"data: {\"type\":\"codex.response.metadata\",\"headers\":{\"x-account-email\":\"ws-secret\"}}\n\n" +
			"data: {\"type\":\"response.metadata\",\"metadata\":{\"account\":\"ws-secret-2\"}}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ws-safe\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ws_safe\",\"status\":\"completed\"}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	t.Cleanup(store.Stop)
	h := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	h.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial websocket: %v (status %d)", err, resp.StatusCode)
		}
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.4","input":"hello"}`)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	seenTerminal := false
	for i := 0; i < 8; i++ {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, message, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read websocket message: %v", err)
		}
		if bytes.Contains(message, []byte("codex.rate_limits")) || bytes.Contains(message, []byte("plan_type")) || bytes.Contains(message, []byte("ws-secret")) {
			t.Fatalf("preflight metadata leaked over native WebSocket: %s", message)
		}
		if gjson.GetBytes(message, "type").String() == "response.completed" {
			seenTerminal = true
			break
		}
	}
	if !seenTerminal {
		t.Fatal("native WebSocket did not deliver a terminal response")
	}
}

func TestResponsesContinuousRetryNeverPublishesPreflightMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableCatchAllContinuousRetry(t)

	upstream, calls := newAttemptSequenceSSEServer(t, [][]string{
		{
			`{"type":"codex.rate_limits","plan_type":"pro","rate_limits":{"primary":{"used_percent":33}}}`,
			`{"type":"response.created","response":{"id":"resp_failed_attempt"}}`,
			`{"type":"response.failed","response":{"status":"failed","status_code":503,"error":{"code":"future_failure","message":"private failure"}}}`,
		},
		{
			`{"type":"codex.rate_limits","plan_type":"pro","rate_limits":{"primary":{"used_percent":44}}}`,
			`{"type":"response.output_text.delta","delta":"retry-safe"}`,
			`{"type":"response.completed","response":{"id":"resp_success_attempt","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		},
	})
	store := newOpenAIResponsesRelayStore(upstream.URL)
	t.Cleanup(store.Stop)
	h := NewHandler(store, nil, nil, nil)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4.1-direct","input":"hello","stream":true}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.Responses(ctx)

	body := recorder.Body.String()
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2; body=%q", got, body)
	}
	if recorder.Code != http.StatusOK || !strings.Contains(body, "retry-safe") {
		t.Fatalf("successful retry missing: status=%d body=%q", recorder.Code, body)
	}
	for _, leaked := range []string{"codex.rate_limits", "private failure", "resp_failed_attempt"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("continuous retry leaked %q: %q", leaked, body)
		}
	}
	if strings.Contains(body, preflightSSEPingComment) {
		t.Fatalf("continuous retry should keep preflight marker private: %q", body)
	}
}

func TestResponsesPreflightPingKeepsRetryBoundaryLogical(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	next := previous
	next.CodexPreflightSSEPassthrough = true
	next.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	next.StreamFlushPolicy = StreamFlushPolicyImmediate
	ApplyRuntimeSettings(next)

	upstream, calls := newAttemptSequenceSSEServer(t, [][]string{
		{
			`{"type":"codex.rate_limits","plan_type":"pro"}`,
			`{"type":"response.failed","response":{"status":"failed","status_code":503,"error":{"code":"server_error","message":"retry-me"}}}`,
		},
		{
			`{"type":"response.output_text.delta","delta":"after-ping-retry"}`,
			`{"type":"response.completed","response":{"status":"completed"}}`,
		},
	})
	store := newOpenAIResponsesRelayStore(upstream.URL)
	// Permit one ordinary stream retry for this focused boundary test.
	store.SetMaxRetries(1)
	t.Cleanup(store.Stop)
	h := NewHandler(store, nil, nil, nil)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4.1-direct","input":"hello","stream":true}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.Responses(ctx)

	body := recorder.Body.String()
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2; body=%q", got, body)
	}
	if !strings.Contains(body, preflightSSEPingComment) || !strings.Contains(body, "after-ping-retry") || strings.Contains(body, "retry-me") {
		t.Fatalf("ping/retry boundary incorrect: %q", body)
	}
}

func TestWriteSSECommentImmediateDrainsOutputScannerSafely(t *testing.T) {
	cfg := promptfilter.DefaultConfig()
	cfg.Enabled = true
	cfg.Advanced.Output.Enabled = true
	cfg.Advanced.Output.BufferBytes = 512
	cfg.Advanced.Output.OverlapBytes = 64
	var out bytes.Buffer
	w := &streamFlushWriter{writer: &out, outputScanner: promptfilter.NewOutputScanner(cfg)}
	// No scanner bytes are pending yet, so the fixed marker can bypass the
	// scanner and reach the network immediately.
	if err := w.WriteSSECommentImmediate(preflightSSEPingComment); err != nil {
		t.Fatalf("initial WriteSSECommentImmediate: %v", err)
	}
	if !strings.Contains(out.String(), preflightSSEPingComment) {
		t.Fatalf("marker was not emitted with an empty scanner: %q", out.String())
	}
	if err := w.WriteSSEData([]byte(`{"type":"response.output_text.delta","delta":"hello"}`)); err != nil {
		t.Fatalf("WriteSSEData: %v", err)
	}
	if err := w.WriteSSECommentImmediate(preflightSSEPingComment); err != nil {
		t.Fatalf("WriteSSECommentImmediate: %v", err)
	}
	// A transport flush must not release the scanner's cross-frame safety window;
	// a terminal event provides the semantic boundary that releases both bytes.
	if err := w.WriteSSEData([]byte(`{"type":"response.completed"}`)); err != nil {
		t.Fatalf("terminal WriteSSEData: %v", err)
	}
	if !strings.Contains(out.String(), preflightSSEPingComment) || !strings.Contains(out.String(), `"delta":"hello"`) {
		t.Fatalf("scanner bytes/comment were not flushed safely: %q", out.String())
	}
}

func TestResponsesRelayPreflightPingArrivesBeforeUpstreamContent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	next := previous
	next.CodexPreflightSSEPassthrough = true
	next.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	next.StreamFlushPolicy = StreamFlushPolicyImmediate
	next.CodexContinueThinking = false
	ApplyRuntimeSettings(next)

	metadataSent := make(chan struct{})
	releaseContent := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		_, _ = io.WriteString(w, `data: {"type":"codex.rate_limits","plan_type":"pro"}`+"\n\n")
		flusher.Flush()
		close(metadataSent)
		select {
		case <-releaseContent:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"network-safe"}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"status":"completed"}}`+"\n\n")
		flusher.Flush()
	}))
	t.Cleanup(func() {
		select {
		case <-releaseContent:
		default:
			close(releaseContent)
		}
		upstream.Close()
	})
	store := newOpenAIResponsesRelayStore(upstream.URL)
	t.Cleanup(store.Stop)
	h := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	h.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-4.1-direct","input":"hello","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("downstream request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("downstream status = %d, want 200", resp.StatusCode)
	}
	select {
	case <-metadataSent:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not send preflight metadata")
	}
	reader := bufio.NewReader(resp.Body)
	frameCh := make(chan string, 1)
	go func() {
		var frame strings.Builder
		for {
			line, readErr := reader.ReadString('\n')
			frame.WriteString(line)
			if strings.HasSuffix(frame.String(), "\n\n") || readErr != nil {
				frameCh <- frame.String()
				return
			}
		}
	}()
	var firstFrame string
	select {
	case firstFrame = <-frameCh:
	case <-time.After(2 * time.Second):
		t.Fatal("preflight ping did not reach downstream before content release")
	}
	if firstFrame != preflightSSEPingComment {
		t.Fatalf("first downstream frame = %q, want %q", firstFrame, preflightSSEPingComment)
	}
	close(releaseContent)
	remaining, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read downstream remainder: %v", err)
	}
	body := firstFrame + string(remaining)
	if strings.Contains(body, "codex.rate_limits") || !strings.Contains(body, "network-safe") {
		t.Fatalf("downstream body leaked metadata or missed content: %q", body)
	}
}

func TestChatCompletionsDropsPreflightMetadataBeforeGenericDeltaTranslation(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	next := previous
	next.CodexPreflightSSEPassthrough = true
	next.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(next)

	h, _ := newChatStreamTerminalTestHandler(t, []string{
		`{"type":"codex.rate_limits","plan_type":"pro","delta":"account-secret"}`,
		`{"type":"response.output_text.delta","delta":"visible-chat"}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
	})
	recorder := invokeChatCompletionsStream(t, h)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, `"content":"visible-chat"`) {
		t.Fatalf("chat response missing visible content: status=%d body=%q", recorder.Code, body)
	}
	if strings.Contains(body, "account-secret") || strings.Contains(body, "codex.rate_limits") {
		t.Fatalf("preflight metadata fell through generic Chat translation: %q", body)
	}
}

func TestResponsesPreflightPingCommitsLegacySSEFailureSemantics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	next := previous
	next.CodexPreflightSSEPassthrough = true
	next.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	next.StreamFlushPolicy = StreamFlushPolicyImmediate
	next.OverflowAutoCompact = false
	ApplyRuntimeSettings(next)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"codex.rate_limits","plan_type":"pro"}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.failed","response":{"status":"failed","status_code":400,"error":{"type":"invalid_request_error","code":"invalid_request_error","message":"bad request"}}}`+"\n\n")
	}))
	t.Cleanup(upstream.Close)
	store := newOpenAIResponsesRelayStore(upstream.URL)
	t.Cleanup(store.Stop)
	h := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4.1-direct","input":"hello","stream":true}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.Responses(ctx)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, preflightSSEPingComment) || !strings.Contains(body, "response.failed") {
		t.Fatalf("legacy ping/failure semantics changed: status=%d body=%q", recorder.Code, body)
	}
	if strings.Contains(body, "codex.rate_limits") {
		t.Fatalf("rate-limit metadata leaked in failure path: %q", body)
	}
}

func TestAnthropicMessagesDropsPreflightMetadataBeforeTranslation(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	next := previous
	next.CodexPreflightSSEPassthrough = true
	next.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(next)

	h, _ := newAnthropicStreamFailureTestHandler(t, func(_ int32, w http.ResponseWriter) {
		writeCodexSSE(w,
			`{"type":"codex.rate_limits","plan_type":"pro","delta":"anthropic-secret"}`,
			`{"type":"response.created","response":{"id":"resp_messages_safe"}}`,
			`{"type":"response.output_item.added","item":{"type":"message"}}`,
			`{"type":"response.output_text.delta","delta":"visible-message"}`,
			`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		)
	})
	recorder := invokeAnthropicMessagesStream(t, h)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "visible-message") {
		t.Fatalf("Anthropic response missing visible content: status=%d body=%q", recorder.Code, body)
	}
	if strings.Contains(body, "anthropic-secret") || strings.Contains(body, "codex.rate_limits") {
		t.Fatalf("preflight metadata fell through Anthropic translation: %q", body)
	}
}

func TestTranslatorsDropPreflightMetadataBeforeFallbackDeltaHandling(t *testing.T) {
	payload := []byte(`{"type":"codex.rate_limits","delta":"account-secret"}`)
	if chunk, done := TranslateStreamChunk(payload, "gpt-5.4", "chatcmpl-test", time.Now().Unix()); chunk != nil || done {
		t.Fatalf("stateless translator exposed preflight metadata: chunk=%s done=%t", chunk, done)
	}
	translator := NewStreamTranslator("chatcmpl-test", "gpt-5.4", time.Now().Unix())
	result := translator.TranslateParsedResult(gjson.ParseBytes(payload))
	if result.Chunk != nil || result.Terminal || result.Failed {
		t.Fatalf("stateful translator exposed preflight metadata: %#v", result)
	}
}
