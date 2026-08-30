// Package telemetry provides explicitly enabled, privacy-minimizing Codex
// analytics reporting.
package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/version"
	"github.com/google/uuid"
)

const defaultEndpoint = "https://chatgpt.com/backend-api/codex/analytics-events/events"

// Credential identifies the ChatGPT account used for one upload. It is never
// included in an Event or an error message.
type Credential struct {
	AccessToken string
	AccountID   string
	ProxyURL    string
}

// TurnInput is deliberately an allowlist: it has no fields for user content,
// response content, network identity, account identity, secrets, or error text.
type TurnInput struct {
	ThreadID, SessionID, TurnID, RootTurnID                    string
	ParentThreadID, ThreadSource, SubagentSource               string
	InitializationMode                                         string
	Model, ReasoningEffort, ServiceTier, Status, ErrorKind     string
	HTTPStatusCode, DurationMS, InputTokens, OutputTokens      int
	CachedInputTokens, ReasoningOutputTokens, TotalTokens      int
	SamplingRequestCount, SamplingRetryCount                   int
	TotalToolCallCount, DynamicToolCallCount, MCPToolCallCount int
	WebSearchCount, ImageGenerationCount, ShellCommandCount    int
	StartedAt, CompletedAt                                     time.Time
}

// CompactionInput is the fixed allowlist for a Codex compaction event. ErrorKind
// is a coarse category; callers must never put raw error text in it.
type CompactionInput struct {
	ThreadID, SessionID, TurnID                              string
	ParentThreadID, ThreadSource, SubagentSource             string
	Phase, Implementation, Trigger, Strategy, Reason, Status string
	ErrorKind                                                string
	HTTPStatusCode, DurationMS                               int
	InputTokens, CachedInputTokens, OutputTokens             int
	ReasoningOutputTokens, TotalTokens                       int
	StartedAt, CompletedAt                                   time.Time
}

// AppServerClient is the observed Codex client metadata object.
type AppServerClient struct {
	ProductClientID, ClientName, ClientVersion, RPCTransport string
	ExperimentalAPIEnabled                                   bool
}

// RuntimeMetadata is the observed Codex runtime metadata object.
type RuntimeMetadata struct {
	CodexRSVersion, RuntimeOS, RuntimeOSVersion, RuntimeArch string
}

// EventParams is the concrete, inspectable allowlist of event properties.
type turnParams struct {
	ThreadID, SessionID, TurnID, RootTurnID                    string
	ParentThreadID, SubagentSource                             string
	AppServerClient                                            AppServerClient
	Runtime                                                    RuntimeMetadata
	Model, ModelProvider, ThreadSource, InitializationMode     string
	ReasoningEffort, ServiceTier, Status                       string
	CodexErrorKind                                             string
	CodexErrorHTTPStatusCode                                   int
	InputTokens, CachedInputTokens, OutputTokens               int
	ReasoningOutputTokens, TotalTokens                         int
	SamplingRequestCount, SamplingRetryCount, DurationMS       int
	TotalToolCallCount, DynamicToolCallCount, MCPToolCallCount int
	WebSearchCount, ImageGenerationCount, ShellCommandCount    int
	StartedAt, CompletedAt                                     time.Time
	Ephemeral                                                  bool
}

type compactionParams struct {
	ThreadID, SessionID, TurnID, ParentThreadID, ThreadSource, SubagentSource        string
	AppServerClient                                                                  AppServerClient
	Runtime                                                                          RuntimeMetadata
	Phase, Implementation, Trigger, Strategy, Reason, Status, CodexErrorKind         string
	CodexErrorHTTPStatusCode, DurationMS                                             int
	InputTokens, CachedInputTokens, OutputTokens, ReasoningOutputTokens, TotalTokens int
	StartedAt, CompletedAt                                                           time.Time
}

// Event is a safe typed analytics event. Its payload is intentionally private,
// so events can only be produced by the allowlisted constructors below.
type Event struct {
	eventType  string
	turn       *turnParams
	compaction *compactionParams
}

func fixedMetadata() (AppServerClient, RuntimeMetadata) {
	return AppServerClient{ProductClientID: "codex2api", ClientName: "codex2api", ClientVersion: version.Current(), RPCTransport: "http"},
		RuntimeMetadata{CodexRSVersion: version.Current(), RuntimeOS: runtime.GOOS, RuntimeOSVersion: runtime.Version(), RuntimeArch: runtime.GOARCH}
}

func relatedIDs(threadID, sessionID, turnID string) (string, string, string) {
	if threadID == "" && sessionID == "" {
		threadID = newUUIDv7()
		sessionID = threadID
	}
	if threadID == "" {
		threadID = sessionID
	}
	if sessionID == "" {
		sessionID = threadID
	}
	if turnID == "" {
		turnID = newUUIDv7()
	}
	return threadID, sessionID, turnID
}

// NewTurnEvent creates an allowlisted turn event, preserving request IDs.
func NewTurnEvent(in TurnInput) Event {
	if in.Status == "" && in.HTTPStatusCode != 0 {
		if in.HTTPStatusCode >= 200 && in.HTTPStatusCode < 300 {
			in.Status = "completed"
		} else {
			in.Status = "failed"
		}
	}
	threadID, sessionID, turnID := relatedIDs(in.ThreadID, in.SessionID, in.TurnID)
	rootTurnID := in.RootTurnID
	if rootTurnID == "" {
		rootTurnID = turnID
	}
	threadSource := in.ThreadSource
	if threadSource == "" {
		threadSource = "user"
	}
	initializationMode := in.InitializationMode
	if initializationMode == "" {
		initializationMode = "new"
	}
	samplingRequests := in.SamplingRequestCount
	if samplingRequests == 0 {
		samplingRequests = 1
	}
	errorKind, errorHTTPStatusCode := in.ErrorKind, in.HTTPStatusCode
	if in.Status == "completed" {
		errorKind, errorHTTPStatusCode = "", 0
	}
	client, runtimeMetadata := fixedMetadata()
	return Event{eventType: "codex_turn_event", turn: &turnParams{
		ThreadID: threadID, SessionID: sessionID, TurnID: turnID, RootTurnID: rootTurnID,
		ParentThreadID: in.ParentThreadID, SubagentSource: in.SubagentSource,
		AppServerClient: client, Runtime: runtimeMetadata,
		Model: in.Model, ModelProvider: "openai", ThreadSource: threadSource, InitializationMode: initializationMode,
		ReasoningEffort: in.ReasoningEffort, ServiceTier: in.ServiceTier, Status: in.Status,
		CodexErrorKind: errorKind, CodexErrorHTTPStatusCode: errorHTTPStatusCode,
		InputTokens: in.InputTokens, CachedInputTokens: in.CachedInputTokens, OutputTokens: in.OutputTokens,
		ReasoningOutputTokens: in.ReasoningOutputTokens, TotalTokens: in.TotalTokens,
		SamplingRequestCount: samplingRequests, SamplingRetryCount: in.SamplingRetryCount, DurationMS: in.DurationMS,
		TotalToolCallCount: in.TotalToolCallCount, DynamicToolCallCount: in.DynamicToolCallCount, MCPToolCallCount: in.MCPToolCallCount,
		WebSearchCount: in.WebSearchCount, ImageGenerationCount: in.ImageGenerationCount, ShellCommandCount: in.ShellCommandCount,
		StartedAt: in.StartedAt, CompletedAt: in.CompletedAt, Ephemeral: true,
	}}
}

// NewCompactionEvent creates an allowlisted compaction event.
func NewCompactionEvent(in CompactionInput) Event {
	threadID, sessionID, turnID := relatedIDs(in.ThreadID, in.SessionID, in.TurnID)
	threadSource := in.ThreadSource
	if threadSource == "" {
		threadSource = "user"
	}
	errorKind, errorHTTPStatusCode := in.ErrorKind, in.HTTPStatusCode
	if in.Status == "completed" {
		errorKind, errorHTTPStatusCode = "", 0
	}
	client, runtimeMetadata := fixedMetadata()
	return Event{eventType: "codex_compaction_event", compaction: &compactionParams{
		ThreadID: threadID, SessionID: sessionID, TurnID: turnID, ParentThreadID: in.ParentThreadID,
		ThreadSource: threadSource, SubagentSource: in.SubagentSource, AppServerClient: client, Runtime: runtimeMetadata,
		Phase: in.Phase, Implementation: in.Implementation, Trigger: in.Trigger, Strategy: in.Strategy, Reason: in.Reason, Status: in.Status,
		CodexErrorKind: errorKind, CodexErrorHTTPStatusCode: errorHTTPStatusCode, DurationMS: in.DurationMS,
		InputTokens: in.InputTokens, CachedInputTokens: in.CachedInputTokens, OutputTokens: in.OutputTokens,
		ReasoningOutputTokens: in.ReasoningOutputTokens, TotalTokens: in.TotalTokens, StartedAt: in.StartedAt, CompletedAt: in.CompletedAt,
	}}
}

func newUUIDv7() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.New().String()
	}
	return id.String()
}

// MarshalJSON emits only the fixed analytics allowlist.
func (e Event) MarshalJSON() ([]byte, error) {
	type clientWire struct {
		ProductClientID        string `json:"product_client_id"`
		ClientName             string `json:"client_name"`
		ClientVersion          string `json:"client_version"`
		RPCTransport           string `json:"rpc_transport"`
		ExperimentalAPIEnabled bool   `json:"experimental_api_enabled"`
	}
	type runtimeWire struct {
		CodexRSVersion   string `json:"codex_rs_version"`
		RuntimeOS        string `json:"runtime_os"`
		RuntimeOSVersion string `json:"runtime_os_version"`
		RuntimeArch      string `json:"runtime_arch"`
	}
	type paramsWire struct {
		ThreadID                 string      `json:"thread_id"`
		SessionID                string      `json:"session_id"`
		TurnID                   string      `json:"turn_id"`
		RootTurnID               string      `json:"root_turn_id"`
		ParentThreadID           string      `json:"parent_thread_id,omitempty"`
		SubagentSource           string      `json:"subagent_source,omitempty"`
		AppServerClient          clientWire  `json:"app_server_client"`
		Runtime                  runtimeWire `json:"runtime"`
		Model                    string      `json:"model,omitempty"`
		ModelProvider            string      `json:"model_provider"`
		Ephemeral                bool        `json:"ephemeral"`
		ThreadSource             string      `json:"thread_source"`
		InitializationMode       string      `json:"initialization_mode"`
		ReasoningEffort          string      `json:"reasoning_effort,omitempty"`
		ServiceTier              string      `json:"service_tier,omitempty"`
		Status                   string      `json:"status,omitempty"`
		CodexErrorKind           string      `json:"codex_error_kind,omitempty"`
		CodexErrorHTTPStatusCode int         `json:"codex_error_http_status_code,omitempty"`
		InputTokens              int         `json:"input_tokens,omitempty"`
		CachedInputTokens        int         `json:"cached_input_tokens,omitempty"`
		OutputTokens             int         `json:"output_tokens,omitempty"`
		ReasoningOutputTokens    int         `json:"reasoning_output_tokens,omitempty"`
		TotalTokens              int         `json:"total_tokens,omitempty"`
		SamplingRequestCount     int         `json:"sampling_request_count"`
		SamplingRetryCount       int         `json:"sampling_retry_count,omitempty"`
		TotalToolCallCount       int         `json:"total_tool_call_count,omitempty"`
		DynamicToolCallCount     int         `json:"dynamic_tool_call_count,omitempty"`
		MCPToolCallCount         int         `json:"mcp_tool_call_count,omitempty"`
		WebSearchCount           int         `json:"web_search_count,omitempty"`
		ImageGenerationCount     int         `json:"image_generation_count,omitempty"`
		ShellCommandCount        int         `json:"shell_command_count,omitempty"`
		DurationMS               int         `json:"duration_ms,omitempty"`
		StartedAt                int64       `json:"started_at,omitempty"`
		CompletedAt              int64       `json:"completed_at,omitempty"`
	}
	type wire struct {
		EventType   string     `json:"event_type"`
		EventParams paramsWire `json:"event_params"`
	}
	unix := func(v time.Time) int64 {
		if v.IsZero() {
			return 0
		}
		return v.Unix()
	}
	if e.turn != nil && e.eventType == "codex_turn_event" {
		ep := e.turn
		return json.Marshal(wire{EventType: e.eventType, EventParams: paramsWire{
			ThreadID: ep.ThreadID, SessionID: ep.SessionID, TurnID: ep.TurnID, RootTurnID: ep.RootTurnID,
			ParentThreadID: ep.ParentThreadID, SubagentSource: ep.SubagentSource,
			AppServerClient: clientWire{ep.AppServerClient.ProductClientID, ep.AppServerClient.ClientName, ep.AppServerClient.ClientVersion, ep.AppServerClient.RPCTransport, ep.AppServerClient.ExperimentalAPIEnabled},
			Runtime:         runtimeWire{ep.Runtime.CodexRSVersion, ep.Runtime.RuntimeOS, ep.Runtime.RuntimeOSVersion, ep.Runtime.RuntimeArch},
			Model:           ep.Model, ModelProvider: ep.ModelProvider, Ephemeral: ep.Ephemeral, ThreadSource: ep.ThreadSource, InitializationMode: ep.InitializationMode,
			ReasoningEffort: ep.ReasoningEffort, ServiceTier: ep.ServiceTier, Status: ep.Status,
			CodexErrorKind: ep.CodexErrorKind, CodexErrorHTTPStatusCode: ep.CodexErrorHTTPStatusCode,
			InputTokens: ep.InputTokens, CachedInputTokens: ep.CachedInputTokens, OutputTokens: ep.OutputTokens, ReasoningOutputTokens: ep.ReasoningOutputTokens, TotalTokens: ep.TotalTokens,
			SamplingRequestCount: ep.SamplingRequestCount, SamplingRetryCount: ep.SamplingRetryCount, DurationMS: ep.DurationMS,
			TotalToolCallCount: ep.TotalToolCallCount, DynamicToolCallCount: ep.DynamicToolCallCount, MCPToolCallCount: ep.MCPToolCallCount,
			WebSearchCount: ep.WebSearchCount, ImageGenerationCount: ep.ImageGenerationCount, ShellCommandCount: ep.ShellCommandCount,
			StartedAt: unix(ep.StartedAt), CompletedAt: unix(ep.CompletedAt),
		}})
	}
	if e.compaction != nil && e.eventType == "codex_compaction_event" {
		type compactionWire struct {
			ThreadID                 string      `json:"thread_id"`
			SessionID                string      `json:"session_id"`
			TurnID                   string      `json:"turn_id"`
			ParentThreadID           string      `json:"parent_thread_id,omitempty"`
			AppServerClient          clientWire  `json:"app_server_client"`
			Runtime                  runtimeWire `json:"runtime"`
			ThreadSource             string      `json:"thread_source"`
			SubagentSource           string      `json:"subagent_source,omitempty"`
			Phase                    string      `json:"phase,omitempty"`
			Implementation           string      `json:"implementation,omitempty"`
			Trigger                  string      `json:"trigger,omitempty"`
			Strategy                 string      `json:"strategy,omitempty"`
			Reason                   string      `json:"reason,omitempty"`
			Status                   string      `json:"status,omitempty"`
			CodexErrorKind           string      `json:"codex_error_kind,omitempty"`
			CodexErrorHTTPStatusCode int         `json:"codex_error_http_status_code,omitempty"`
			DurationMS               int         `json:"duration_ms,omitempty"`
			InputTokens              int         `json:"input_tokens,omitempty"`
			CachedInputTokens        int         `json:"cached_input_tokens,omitempty"`
			OutputTokens             int         `json:"output_tokens,omitempty"`
			ReasoningOutputTokens    int         `json:"reasoning_output_tokens,omitempty"`
			TotalTokens              int         `json:"total_tokens,omitempty"`
			StartedAt                int64       `json:"started_at,omitempty"`
			CompletedAt              int64       `json:"completed_at,omitempty"`
		}
		type compactionEventWire struct {
			EventType   string         `json:"event_type"`
			EventParams compactionWire `json:"event_params"`
		}
		ep := e.compaction
		return json.Marshal(compactionEventWire{EventType: e.eventType, EventParams: compactionWire{
			ThreadID: ep.ThreadID, SessionID: ep.SessionID, TurnID: ep.TurnID, ParentThreadID: ep.ParentThreadID,
			AppServerClient: clientWire{ep.AppServerClient.ProductClientID, ep.AppServerClient.ClientName, ep.AppServerClient.ClientVersion, ep.AppServerClient.RPCTransport, ep.AppServerClient.ExperimentalAPIEnabled},
			Runtime:         runtimeWire{ep.Runtime.CodexRSVersion, ep.Runtime.RuntimeOS, ep.Runtime.RuntimeOSVersion, ep.Runtime.RuntimeArch},
			ThreadSource:    ep.ThreadSource, SubagentSource: ep.SubagentSource, Phase: ep.Phase, Implementation: ep.Implementation,
			Trigger: ep.Trigger, Strategy: ep.Strategy, Reason: ep.Reason, Status: ep.Status,
			CodexErrorKind: ep.CodexErrorKind, CodexErrorHTTPStatusCode: ep.CodexErrorHTTPStatusCode, DurationMS: ep.DurationMS,
			InputTokens: ep.InputTokens, CachedInputTokens: ep.CachedInputTokens, OutputTokens: ep.OutputTokens,
			ReasoningOutputTokens: ep.ReasoningOutputTokens, TotalTokens: ep.TotalTokens, StartedAt: unix(ep.StartedAt), CompletedAt: unix(ep.CompletedAt),
		}})
	}
	return nil, errors.New("invalid telemetry event")
}

// Sink is the non-blocking event submission surface.
type Sink interface{ Enqueue(Credential, Event) bool }

// ClientFactory builds an HTTP client for a credential. Implementations must
// arrange their own timeout and redirect policy when overriding the default.
type ClientFactory func(Credential) (*http.Client, error)

// Config controls a Reporter. Enabled must be true; telemetry is opt-in.
type Config struct {
	Enabled       bool
	ShouldSend    func() bool
	Endpoint      string
	QueueSize     int
	BatchSize     int
	FlushInterval time.Duration
	HTTPTimeout   time.Duration
	ClientFactory ClientFactory
	OnError       func(error)
}

type queued struct {
	credential Credential
	event      Event
}

// Reporter owns exactly one upload worker and a bounded queue.
type Reporter struct {
	cfg            Config
	queue          chan queued
	done           chan struct{}
	mu             sync.Mutex
	closed         bool
	closeOnce      sync.Once
	errMu          sync.Mutex
	lastQueueError time.Time
	lastSendError  time.Time
}

// NewReporter constructs a reporter. A disabled reporter is a safe no-op sink,
// so merely constructing one never opts a process into reporting.
func NewReporter(cfg Config) *Reporter {
	if !cfg.Enabled {
		done := make(chan struct{})
		close(done)
		return &Reporter{cfg: cfg, done: done, closed: true}
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = defaultEndpoint
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 256
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 20
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 2 * time.Second
	}
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = 5 * time.Second
	}
	if cfg.ClientFactory == nil {
		cfg.ClientFactory = defaultClientFactory(cfg.HTTPTimeout)
	}
	r := &Reporter{cfg: cfg, queue: make(chan queued, cfg.QueueSize), done: make(chan struct{})}
	go r.run()
	return r
}

func defaultClientFactory(timeout time.Duration) ClientFactory {
	return func(c Credential) (*http.Client, error) {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		if err := auth.ConfigureTransportProxy(transport, c.ProxyURL, &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}); err != nil {
			return nil, err
		}
		return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}, nil
	}
}

// Enqueue never waits. It returns false after Close or when the queue is full.
func (r *Reporter) Enqueue(c Credential, e Event) bool {
	if r.cfg.ShouldSend != nil && !r.cfg.ShouldSend() {
		return false
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return false
	}
	select {
	case r.queue <- queued{credential: c, event: e}:
		r.mu.Unlock()
		return true
	default:
		r.mu.Unlock()
		r.reportQueueFull()
		return false
	}
}

func (r *Reporter) reportQueueFull() {
	if r.cfg.OnError == nil {
		return
	}
	r.errMu.Lock()
	defer r.errMu.Unlock()
	if time.Since(r.lastQueueError) >= time.Minute {
		r.lastQueueError = time.Now()
		r.cfg.OnError(errors.New("telemetry queue full; event dropped"))
	}
}

// Close stops acceptance, drains the queue, and waits for the worker. It is
// idempotent and safe to race with Enqueue.
func (r *Reporter) Close(ctx context.Context) error {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		if r.queue != nil {
			close(r.queue)
		}
		r.mu.Unlock()
	})
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Reporter) run() {
	defer close(r.done)
	ticker := time.NewTicker(r.cfg.FlushInterval)
	defer ticker.Stop()
	batch := make([]queued, 0, r.cfg.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		groups := make(map[Credential][]Event)
		order := make([]Credential, 0)
		for _, q := range batch {
			if _, ok := groups[q.credential]; !ok {
				order = append(order, q.credential)
			}
			groups[q.credential] = append(groups[q.credential], q.event)
		}
		for _, c := range order {
			if err := r.send(c, groups[c]); err != nil {
				r.report(err)
			}
		}
		batch = batch[:0]
	}
	for {
		select {
		case q, ok := <-r.queue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, q)
			if len(batch) >= r.cfg.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (r *Reporter) report(err error) {
	if r.cfg.OnError == nil {
		return
	}
	r.errMu.Lock()
	defer r.errMu.Unlock()
	if time.Since(r.lastSendError) >= time.Minute {
		r.lastSendError = time.Now()
		r.cfg.OnError(err)
	}
}

func (r *Reporter) send(c Credential, events []Event) error {
	// Re-check at flush time so a live settings change also drops events that
	// were queued before reporting was disabled.
	if r.cfg.ShouldSend != nil && !r.cfg.ShouldSend() {
		return nil
	}
	payload, err := json.Marshal(struct {
		Events []Event `json:"events"`
	}{events})
	if err != nil {
		return fmt.Errorf("encode telemetry: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, r.cfg.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return errors.New("create telemetry request")
	}
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("chatgpt-account-id", c.AccountID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Originator", "codex2api")
	req.Header.Set("User-Agent", "codex2api telemetry")
	client, err := r.cfg.ClientFactory(c)
	if err != nil {
		return errors.New("create telemetry client")
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("send telemetry request")
	}
	defer resp.Body.Close()
	const maxResponse = 32 << 10
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if readErr != nil {
		return errors.New("read telemetry response")
	}
	if len(body) > maxResponse {
		return errors.New("telemetry response too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telemetry returned HTTP %d", resp.StatusCode)
	}
	if len(strings.TrimSpace(string(body))) != 0 {
		var ack struct {
			SkippedEvents int `json:"skipped_events"`
		}
		if json.Unmarshal(body, &ack) == nil && ack.SkippedEvents > 0 {
			return fmt.Errorf("telemetry acknowledgement skipped %d events", ack.SkippedEvents)
		}
	}
	return nil
}
