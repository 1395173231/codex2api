package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testConfig(endpoint string) Config {
	return Config{Enabled: true, Endpoint: endpoint, QueueSize: 8, BatchSize: 1, FlushInterval: time.Hour, HTTPTimeout: time.Second}
}

func TestDisabledReporterIsNoop(t *testing.T) {
	r := NewReporter(Config{})
	if r.Enqueue(Credential{}, NewTurnEvent(TurnInput{})) {
		t.Fatal("disabled reporter accepted event")
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeGateSupportsHotEnableAndDropsQueuedEventsOnDisable(t *testing.T) {
	var enabled atomic.Bool
	var uploads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		uploads.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.BatchSize = 20
	cfg.ShouldSend = enabled.Load
	r := NewReporter(cfg)
	event := NewTurnEvent(TurnInput{Status: "completed"})
	if r.Enqueue(Credential{}, event) {
		t.Fatal("runtime-disabled reporter accepted event")
	}
	enabled.Store(true)
	if !r.Enqueue(Credential{}, event) {
		t.Fatal("runtime-enabled reporter rejected event")
	}
	enabled.Store(false)
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := uploads.Load(); got != 0 {
		t.Fatalf("uploads after live disable = %d, want 0", got)
	}
}

func TestHeadersAndBodyAllowlist(t *testing.T) {
	got := make(chan struct {
		h    http.Header
		body []byte
	}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- struct {
			h    http.Header
			body []byte
		}{r.Header.Clone(), b}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	r := NewReporter(testConfig(srv.URL))
	c := Credential{AccessToken: "secret-token", AccountID: "acct"}
	if !r.Enqueue(c, NewTurnEvent(TurnInput{Model: "gpt", Status: "completed", HTTPStatusCode: http.StatusOK, DurationMS: 4, InputTokens: 2, OutputTokens: 3, ReasoningEffort: "high", ServiceTier: "priority"})) {
		t.Fatal("enqueue")
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	req := <-got
	for k, want := range map[string]string{"Authorization": "Bearer secret-token", "chatgpt-account-id": "acct", "Content-Type": "application/json", "Accept": "*/*", "Originator": "codex2api", "User-Agent": "codex2api telemetry"} {
		if req.h.Get(k) != want {
			t.Errorf("%s = %q", k, req.h.Get(k))
		}
	}
	var root map[string]any
	if err := json.Unmarshal(req.body, &root); err != nil {
		t.Fatal(err)
	}
	text := string(req.body)
	for _, forbidden := range []string{"secret-token", "acct", "prompt", "response", "email", "api_key", "error"} {
		if strings.Contains(strings.ToLower(text), forbidden) {
			t.Errorf("body contains %q: %s", forbidden, text)
		}
	}
	events := root["events"].([]any)
	event := events[0].(map[string]any)
	if event["event_type"] != "codex_turn_event" {
		t.Fatalf("event type: %v", event)
	}
	if len(event) != 2 {
		t.Fatalf("unexpected top-level wire fields: %v", event)
	}
	p := event["event_params"].(map[string]any)
	client := p["app_server_client"].(map[string]any)
	if client["client_name"] != "codex2api" || client["product_client_id"] != "codex2api" || client["rpc_transport"] != "http" || client["experimental_api_enabled"] != false || p["thread_source"] != "user" || p["ephemeral"] != true {
		t.Fatalf("fixed metadata: %v", p)
	}
	for _, required := range []string{"thread_id", "session_id", "turn_id", "root_turn_id", "runtime", "model_provider", "initialization_mode", "sampling_request_count"} {
		if _, ok := p[required]; !ok {
			t.Errorf("missing %s", required)
		}
	}
	runtimeParams := p["runtime"].(map[string]any)
	if _, ok := runtimeParams["codex_rs_version"]; !ok {
		t.Fatal("runtime.codex_rs_version missing")
	}
}

func eventParams(t *testing.T, event Event) map[string]any {
	t.Helper()
	b, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	return wire["event_params"].(map[string]any)
}

func TestTurnEventPreservesRequestIDsAndCounts(t *testing.T) {
	in := TurnInput{
		ThreadID: "thread-real", SessionID: "session-real", TurnID: "turn-real", RootTurnID: "root-real",
		ParentThreadID: "parent-real", ThreadSource: "exec", SubagentSource: "review", InitializationMode: "resume",
		SamplingRequestCount: 3, SamplingRetryCount: 2, TotalToolCallCount: 11, DynamicToolCallCount: 4,
		MCPToolCallCount: 5, WebSearchCount: 6, ImageGenerationCount: 7, ShellCommandCount: 8,
	}
	p := eventParams(t, NewTurnEvent(in))
	for key, want := range map[string]any{
		"thread_id": "thread-real", "session_id": "session-real", "turn_id": "turn-real", "root_turn_id": "root-real",
		"parent_thread_id": "parent-real", "thread_source": "exec", "subagent_source": "review", "initialization_mode": "resume",
		"sampling_request_count": float64(3), "sampling_retry_count": float64(2), "total_tool_call_count": float64(11),
		"dynamic_tool_call_count": float64(4), "mcp_tool_call_count": float64(5), "web_search_count": float64(6),
		"image_generation_count": float64(7), "shell_command_count": float64(8),
	} {
		if got := p[key]; got != want {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}
}

func TestTurnEventMissingIDsFallbackAndRelate(t *testing.T) {
	p := eventParams(t, NewTurnEvent(TurnInput{}))
	threadID := p["thread_id"].(string)
	if threadID == "" || p["session_id"] != threadID {
		t.Fatalf("thread/session not related: %v", p)
	}
	turnID := p["turn_id"].(string)
	if turnID == "" || p["root_turn_id"] != turnID {
		t.Fatalf("turn/root not related: %v", p)
	}
	p = eventParams(t, NewTurnEvent(TurnInput{SessionID: "existing-session"}))
	if p["thread_id"] != "existing-session" || p["session_id"] != "existing-session" {
		t.Fatalf("session did not supply thread: %v", p)
	}
}

func TestCompactionEventWireAndCompletedOmitsError(t *testing.T) {
	started := time.Unix(100, 0)
	completed := time.Unix(102, 0)
	e := NewCompactionEvent(CompactionInput{
		ThreadID: "thread", SessionID: "session", TurnID: "turn", ParentThreadID: "parent",
		ThreadSource: "subagent", SubagentSource: "worker", Phase: "post_turn", Implementation: "remote",
		Trigger: "token_limit", Strategy: "summarize", Reason: "context_window", Status: "completed",
		ErrorKind: "must-not-appear", HTTPStatusCode: 500, DurationMS: 2000,
		InputTokens: 10, CachedInputTokens: 2, OutputTokens: 3, ReasoningOutputTokens: 1, TotalTokens: 13,
		StartedAt: started, CompletedAt: completed,
	})
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if !strings.Contains(text, `"event_type":"codex_compaction_event"`) {
		t.Fatalf("event type missing: %s", text)
	}
	p := eventParams(t, e)
	for key, want := range map[string]any{
		"thread_id": "thread", "session_id": "session", "turn_id": "turn", "parent_thread_id": "parent",
		"thread_source": "subagent", "subagent_source": "worker", "phase": "post_turn", "implementation": "remote",
		"trigger": "token_limit", "strategy": "summarize", "reason": "context_window", "status": "completed",
		"duration_ms": float64(2000), "input_tokens": float64(10), "cached_input_tokens": float64(2),
		"output_tokens": float64(3), "reasoning_output_tokens": float64(1), "total_tokens": float64(13),
		"started_at": float64(100), "completed_at": float64(102),
	} {
		if got := p[key]; got != want {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}
	if _, ok := p["codex_error_kind"]; ok {
		t.Fatalf("successful compaction includes error: %v", p)
	}
	if _, ok := p["codex_error_http_status_code"]; ok {
		t.Fatalf("successful compaction includes HTTP error: %v", p)
	}
}

func TestCloseDrains(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count.Add(1); w.WriteHeader(204) }))
	defer srv.Close()
	cfg := testConfig(srv.URL)
	cfg.BatchSize = 20
	r := NewReporter(cfg)
	for i := 0; i < 4; i++ {
		if !r.Enqueue(Credential{}, NewTurnEvent(TurnInput{})) {
			t.Fatal("enqueue")
		}
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 1 {
		t.Fatalf("uploads = %d", count.Load())
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.Enqueue(Credential{}, NewTurnEvent(TurnInput{})) {
		t.Fatal("enqueue after close")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestQueueFullDoesNotBlock(t *testing.T) {
	block := make(chan struct{})
	cfg := testConfig("http://unused")
	cfg.QueueSize = 1
	cfg.ClientFactory = func(Credential) (*http.Client, error) {
		return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			<-block
			return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		})}, nil
	}
	r := NewReporter(cfg)
	if !r.Enqueue(Credential{}, NewTurnEvent(TurnInput{})) {
		t.Fatal("first")
	}
	time.Sleep(20 * time.Millisecond)
	if !r.Enqueue(Credential{}, NewTurnEvent(TurnInput{})) {
		t.Fatal("second")
	}
	start := time.Now()
	if r.Enqueue(Credential{}, NewTurnEvent(TurnInput{})) {
		t.Fatal("expected full")
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("blocked")
	}
	close(block)
	_ = r.Close(context.Background())
}

func TestRedirectDoesNotLeakAuthorization(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		w.WriteHeader(204)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	var reported atomic.Bool
	cfg := testConfig(redirect.URL)
	cfg.OnError = func(error) { reported.Store(true) }
	r := NewReporter(cfg)
	r.Enqueue(Credential{AccessToken: "secret"}, NewTurnEvent(TurnInput{}))
	_ = r.Close(context.Background())
	if leaked.Load() {
		t.Fatal("authorization leaked to redirect target")
	}
	if !reported.Load() {
		t.Fatal("3xx should be reported")
	}
}
