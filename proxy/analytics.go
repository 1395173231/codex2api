package proxy

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/telemetry"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	analyticsPendingContextKey  = "codex2api.analytics.pending"
	analyticsResponseContextKey = "codex2api.analytics.response"
	analyticsSessionContextKey  = "codex2api.analytics.upstream_session"
	analyticsContinueRound      = "analytics_continue_thinking_round"
	analyticsMaxActiveTurns     = 1024
)

var analyticsLabelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

type pendingAnalyticsEvent struct {
	credential telemetry.Credential
	event      telemetry.Event
}

// analyticsResponseObservation contains only structural Responses data. It
// deliberately never retains arguments, tool outputs, prompts, or model text.
type analyticsResponseObservation struct {
	terminal             bool
	responseID           string
	seen                 map[string]struct{}
	totalToolCalls       int
	dynamicToolCalls     int
	mcpToolCalls         int
	webSearchCalls       int
	imageGenerationCalls int
	shellCommandCalls    int
}

type analyticsOutboundIdentityContextKey struct{}

type analyticsOutboundIdentityObservation struct {
	mu       sync.RWMutex
	identity analyticsRequestIdentity
	observed bool
}

func newAnalyticsResponseObservation() *analyticsResponseObservation {
	return &analyticsResponseObservation{seen: make(map[string]struct{})}
}

func resetAnalyticsResponseObservation(c *gin.Context) {
	if c != nil {
		c.Set(analyticsResponseContextKey, newAnalyticsResponseObservation())
		if c.Request != nil {
			observation := &analyticsOutboundIdentityObservation{}
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), analyticsOutboundIdentityContextKey{}, observation))
		}
	}
}

func analyticsResponseObservationFromContext(c *gin.Context) *analyticsResponseObservation {
	if c == nil {
		return nil
	}
	value, exists := c.Get(analyticsResponseContextKey)
	if !exists {
		return nil
	}
	observation, _ := value.(*analyticsResponseObservation)
	return observation
}

// observeAnalyticsResponsePayload maps protocol lifecycle items to aggregate
// counters. Local tool execution results are intentionally not inferred here.
func observeAnalyticsResponsePayload(c *gin.Context, payload []byte) {
	if c == nil || len(payload) == 0 || !gjson.ValidBytes(payload) {
		return
	}
	observation := analyticsResponseObservationFromContext(c)
	if observation == nil {
		observation = newAnalyticsResponseObservation()
		c.Set(analyticsResponseContextKey, observation)
	}
	root := gjson.ParseBytes(payload)
	eventType := strings.ToLower(strings.TrimSpace(root.Get("type").String()))
	if eventType == "response.output_item.done" {
		observation.addOutputItem(root.Get("item"))
	}
	if output := root.Get("response.output"); output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			observation.addOutputItem(item)
			return true
		})
	} else if output := root.Get("output"); eventType == "" && output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			observation.addOutputItem(item)
			return true
		})
	}
	switch eventType {
	case "response.completed", "response.incomplete", "response.failed", "error":
		observation.terminal = true
		observation.responseID = analyticsLabel(root.Get("response.id").String())
	}
	if eventType == "" {
		status := strings.ToLower(strings.TrimSpace(root.Get("status").String()))
		if status == "completed" || status == "incomplete" || status == "failed" {
			observation.terminal = true
			observation.responseID = analyticsLabel(root.Get("id").String())
		}
	}
}

func (o *analyticsResponseObservation) addOutputItem(item gjson.Result) {
	if o == nil || !item.IsObject() {
		return
	}
	itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
	var category *int
	switch itemType {
	case "custom_tool_call":
		category = &o.dynamicToolCalls
	case "mcp_call", "mcp_tool_call":
		category = &o.mcpToolCalls
	case "web_search_call":
		category = &o.webSearchCalls
	case "image_generation_call":
		category = &o.imageGenerationCalls
	case "local_shell_call", "shell_call":
		category = &o.shellCommandCalls
	case "function_call", "computer_call", "code_interpreter_call", "file_search_call", "tool_search_call":
		// The wire proves a tool call, but not which Codex executor handled it.
	default:
		return
	}
	key := analyticsLabel(firstAnalyticsValue(item.Get("id").String(), item.Get("call_id").String()))
	if key != "" {
		if _, exists := o.seen[key]; exists {
			return
		}
		o.seen[key] = struct{}{}
	}
	o.totalToolCalls++
	if category != nil {
		(*category)++
	}
}

type analyticsRequestIdentity struct {
	ThreadID, SessionID, TurnID, RootTurnID      string
	ParentThreadID, ThreadSource, SubagentSource string
	InitializationMode                           string
	TurnStartedAt                                time.Time
	Correlated                                   bool
}

// observeAnalyticsOutboundRequest snapshots only allowlisted identifiers from
// the already-transformed outbound request. It never retains the request body,
// headers, prompts, tool arguments, or outputs.
func observeAnalyticsOutboundRequest(ctx context.Context, body []byte, headers http.Header) {
	if ctx == nil {
		return
	}
	observation, _ := ctx.Value(analyticsOutboundIdentityContextKey{}).(*analyticsOutboundIdentityObservation)
	if observation == nil {
		return
	}
	identity := analyticsRequestIdentityFromWire(body, headers)
	observation.mu.Lock()
	observation.identity = identity
	observation.observed = true
	observation.mu.Unlock()
}

func observedAnalyticsOutboundIdentity(c *gin.Context) (analyticsRequestIdentity, bool) {
	if c == nil || c.Request == nil {
		return analyticsRequestIdentity{}, false
	}
	observation, _ := c.Request.Context().Value(analyticsOutboundIdentityContextKey{}).(*analyticsOutboundIdentityObservation)
	if observation == nil {
		return analyticsRequestIdentity{}, false
	}
	observation.mu.RLock()
	defer observation.mu.RUnlock()
	return observation.identity, observation.observed
}

func rememberAnalyticsUpstreamSession(c *gin.Context, sessionID string) {
	if c != nil && strings.TrimSpace(sessionID) != "" {
		c.Set(analyticsSessionContextKey, strings.TrimSpace(sessionID))
	}
}

// analyticsRequestIdentityFromContext applies the same account fingerprint
// convergence used by the actual outbound request before reading identifiers.
func analyticsRequestIdentityFromContext(c *gin.Context, account *auth.Account) analyticsRequestIdentity {
	if identity, ok := observedAnalyticsOutboundIdentity(c); ok {
		return analyticsIdentityWithContextSession(c, identity)
	}
	body, _ := rawRequestBodyFromContext(c)
	var headers http.Header
	if c != nil && c.Request != nil {
		headers = c.Request.Header.Clone()
		body = ApplyCodexFingerprintToBody(body, account, c.Request.Header)
		ApplyCodexFingerprintHeaders(headers, account, c.Request.Header)
	}
	return analyticsIdentityWithContextSession(c, analyticsRequestIdentityFromWire(body, headers))
}

func analyticsIdentityWithContextSession(c *gin.Context, identity analyticsRequestIdentity) analyticsRequestIdentity {
	if identity.SessionID == "" && c != nil {
		if value, exists := c.Get(analyticsSessionContextKey); exists {
			if sessionID, ok := value.(string); ok {
				identity.SessionID = analyticsLabel(sessionID)
			}
		}
	}
	if identity.ThreadID == "" {
		identity.ThreadID = identity.SessionID
	}
	identity.Correlated = identity.TurnID != "" && (identity.ThreadID != "" || identity.SessionID != "")
	return identity
}

func analyticsRequestIdentityFromWire(body []byte, headers http.Header) analyticsRequestIdentity {
	var identity analyticsRequestIdentity
	root := gjson.ParseBytes(body)
	clientMetadata := root.Get("client_metadata")
	var embeddedMetadata gjson.Result
	if encoded := clientMetadata.Get("x-codex-turn-metadata"); encoded.Type == gjson.String && gjson.Valid(encoded.String()) {
		embeddedMetadata = gjson.Parse(encoded.String())
	}
	var headerMetadata gjson.Result
	if encoded := strings.TrimSpace(headers.Get(codexTurnMetadataHeader)); encoded != "" && gjson.Valid(encoded) {
		headerMetadata = gjson.Parse(encoded)
	}
	lookup := func(name string) string {
		for _, source := range []gjson.Result{clientMetadata, embeddedMetadata, headerMetadata, root} {
			if value := analyticsLabel(source.Get(name).String()); value != "" {
				return value
			}
		}
		return ""
	}
	identity.ThreadID = lookup("thread_id")
	identity.SessionID = lookup("session_id")
	identity.TurnID = lookup("turn_id")
	identity.RootTurnID = lookup("root_turn_id")
	identity.ParentThreadID = lookup("parent_thread_id")
	identity.ThreadSource = lookup("thread_source")
	identity.SubagentSource = firstAnalyticsValue(lookup("subagent_source"), lookup("subagent_kind"))
	identity.InitializationMode = lookup("initialization_mode")
	if identity.SessionID == "" {
		identity.SessionID = analyticsLabel(firstAnalyticsValue(headers.Get("Session-Id"), headers.Get("Session_id")))
	}
	if identity.ThreadID == "" {
		identity.ThreadID = analyticsLabel(headers.Get(codexThreadIDHeader))
	}
	if identity.SessionID == "" {
		identity.SessionID = analyticsLabel(root.Get("prompt_cache_key").String())
	}
	if identity.ThreadID == "" {
		identity.ThreadID = identity.SessionID
	}
	if identity.SessionID == "" {
		identity.SessionID = identity.ThreadID
	}
	if identity.RootTurnID == "" {
		identity.RootTurnID = identity.TurnID
	}
	for _, source := range []gjson.Result{clientMetadata, embeddedMetadata, headerMetadata, root} {
		if unixMS := source.Get("turn_started_at_unix_ms").Int(); unixMS > 0 {
			candidate := time.UnixMilli(unixMS).UTC()
			if candidate.After(time.Unix(0, 0)) && candidate.Before(time.Now().Add(24*time.Hour)) {
				identity.TurnStartedAt = candidate
				break
			}
		}
	}
	identity.Correlated = identity.TurnID != "" && (identity.ThreadID != "" || identity.SessionID != "")
	return identity
}

type analyticsThreadKey struct {
	accountID string
	threadID  string
}

type analyticsTurnState struct {
	credential telemetry.Credential
	input      telemetry.TurnInput
	lastSeen   time.Time
}

type analyticsTurnTracker struct {
	mu     sync.Mutex
	active map[analyticsThreadKey]*analyticsTurnState
}

func newAnalyticsTurnTracker() *analyticsTurnTracker {
	return &analyticsTurnTracker{active: make(map[analyticsThreadKey]*analyticsTurnState)}
}

func (t *analyticsTurnTracker) record(credential telemetry.Credential, input telemetry.TurnInput, correlated, final bool) []pendingAnalyticsEvent {
	if t == nil || !correlated {
		return []pendingAnalyticsEvent{{credential: credential, event: telemetry.NewTurnEvent(input)}}
	}
	key := analyticsThreadKey{accountID: credential.AccountID, threadID: firstAnalyticsValue(input.ThreadID, input.SessionID, input.TurnID)}
	now := time.Now().UTC()
	t.mu.Lock()
	defer t.mu.Unlock()
	var ready []pendingAnalyticsEvent
	if previous := t.active[key]; previous != nil && previous.input.TurnID != input.TurnID {
		ready = append(ready, previous.pending())
		delete(t.active, key)
	}
	state := t.active[key]
	if state == nil {
		if len(t.active) >= analyticsMaxActiveTurns {
			var oldestKey analyticsThreadKey
			var oldest *analyticsTurnState
			for candidateKey, candidate := range t.active {
				if oldest == nil || candidate.lastSeen.Before(oldest.lastSeen) {
					oldestKey, oldest = candidateKey, candidate
				}
			}
			if oldest != nil {
				ready = append(ready, oldest.pending())
				delete(t.active, oldestKey)
			}
		}
		state = &analyticsTurnState{credential: credential}
		t.active[key] = state
	}
	state.credential = credential
	state.lastSeen = now
	mergeAnalyticsTurnInput(&state.input, input)
	if final {
		ready = append(ready, state.pending())
		delete(t.active, key)
	}
	return ready
}

func (t *analyticsTurnTracker) flush() []pendingAnalyticsEvent {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	ready := make([]pendingAnalyticsEvent, 0, len(t.active))
	for key, state := range t.active {
		ready = append(ready, state.pending())
		delete(t.active, key)
	}
	return ready
}

func (t *analyticsTurnTracker) discard() {
	if t == nil {
		return
	}
	t.mu.Lock()
	clear(t.active)
	t.mu.Unlock()
}

func (s *analyticsTurnState) pending() pendingAnalyticsEvent {
	input := s.input
	if !input.StartedAt.IsZero() && !input.CompletedAt.IsZero() && input.CompletedAt.After(input.StartedAt) {
		input.DurationMS = int(input.CompletedAt.Sub(input.StartedAt).Milliseconds())
	}
	return pendingAnalyticsEvent{credential: s.credential, event: telemetry.NewTurnEvent(input)}
}

func mergeAnalyticsTurnInput(dst *telemetry.TurnInput, src telemetry.TurnInput) {
	if dst.ThreadID == "" {
		dst.ThreadID = src.ThreadID
	}
	if dst.SessionID == "" {
		dst.SessionID = src.SessionID
	}
	if dst.TurnID == "" {
		dst.TurnID = src.TurnID
	}
	if dst.RootTurnID == "" {
		dst.RootTurnID = src.RootTurnID
	}
	if dst.ParentThreadID == "" {
		dst.ParentThreadID = src.ParentThreadID
	}
	if dst.ThreadSource == "" {
		dst.ThreadSource = src.ThreadSource
	}
	if dst.SubagentSource == "" {
		dst.SubagentSource = src.SubagentSource
	}
	if dst.InitializationMode == "" {
		dst.InitializationMode = src.InitializationMode
	}
	if src.Model != "" {
		dst.Model = src.Model
	}
	if src.ReasoningEffort != "" {
		dst.ReasoningEffort = src.ReasoningEffort
	}
	if src.ServiceTier != "" {
		dst.ServiceTier = src.ServiceTier
	}
	if src.Status != "" {
		dst.Status = src.Status
	}
	if src.ErrorKind != "" {
		dst.ErrorKind = src.ErrorKind
	}
	if src.HTTPStatusCode >= 400 {
		dst.HTTPStatusCode = src.HTTPStatusCode
	}
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.CachedInputTokens += src.CachedInputTokens
	dst.ReasoningOutputTokens += src.ReasoningOutputTokens
	dst.TotalTokens += src.TotalTokens
	dst.SamplingRequestCount += max(src.SamplingRequestCount, 1)
	dst.SamplingRetryCount += src.SamplingRetryCount
	dst.TotalToolCallCount += src.TotalToolCallCount
	dst.DynamicToolCallCount += src.DynamicToolCallCount
	dst.MCPToolCallCount += src.MCPToolCallCount
	dst.WebSearchCount += src.WebSearchCount
	dst.ImageGenerationCount += src.ImageGenerationCount
	dst.ShellCommandCount += src.ShellCommandCount
	if dst.StartedAt.IsZero() || (!src.StartedAt.IsZero() && src.StartedAt.Before(dst.StartedAt)) {
		dst.StartedAt = src.StartedAt
	}
	if src.CompletedAt.After(dst.CompletedAt) {
		dst.CompletedAt = src.CompletedAt
	}
}

// SetAnalyticsReporter installs the optional, non-blocking Codex analytics sink.
// The caller owns its lifecycle and must flush the turn tracker before closing it.
func (h *Handler) SetAnalyticsReporter(sink telemetry.Sink) {
	if h == nil {
		return
	}
	h.analytics = sink
	if sink != nil {
		h.analyticsTurns = newAnalyticsTurnTracker()
	}
}

// AnalyticsMiddleware sends events produced by an HTTP request after the handler
// finishes. Native Responses WebSockets send per-frame events immediately.
func (h *Handler) AnalyticsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if h == nil || h.analytics == nil {
			return
		}
		if !CurrentRuntimeSettings().CodexAnalyticsEnabled {
			h.analyticsTurns.discard()
			return
		}
		value, exists := c.Get(analyticsPendingContextKey)
		if !exists {
			return
		}
		pending, _ := value.([]pendingAnalyticsEvent)
		for _, item := range pending {
			h.analytics.Enqueue(item.credential, item.event)
		}
	}
}

// FlushAnalytics finalizes turns that have no following request (for example the
// last turn before shutdown). Call it only after inbound requests have drained.
func (h *Handler) FlushAnalytics() {
	if h == nil || h.analytics == nil || h.analyticsTurns == nil {
		return
	}
	if !CurrentRuntimeSettings().CodexAnalyticsEnabled {
		h.analyticsTurns.discard()
		return
	}
	for _, item := range h.analyticsTurns.flush() {
		h.analytics.Enqueue(item.credential, item.event)
	}
}

func (h *Handler) queueAnalytics(c *gin.Context, events []pendingAnalyticsEvent) {
	if h == nil || h.analytics == nil || c == nil || len(events) == 0 {
		return
	}
	if !CurrentRuntimeSettings().CodexAnalyticsEnabled {
		h.analyticsTurns.discard()
		return
	}
	if isResponsesWebSocketUpgradeRequest(c.Request) {
		for _, item := range events {
			h.analytics.Enqueue(item.credential, item.event)
		}
		return
	}
	var pending []pendingAnalyticsEvent
	if value, exists := c.Get(analyticsPendingContextKey); exists {
		pending, _ = value.([]pendingAnalyticsEvent)
	}
	c.Set(analyticsPendingContextKey, append(pending, events...))
}

func (h *Handler) captureAnalyticsTurn(c *gin.Context, input *database.UsageLogInput) {
	if h == nil || h.analytics == nil || h.store == nil || c == nil || input == nil || input.AccountID <= 0 || input.IsRetryAttempt {
		return
	}
	if !CurrentRuntimeSettings().CodexAnalyticsEnabled {
		h.analyticsTurns.discard()
		return
	}
	if !analyticsTurnEndpoint(input.InboundEndpoint, c.Request) {
		return
	}
	account := h.store.FindByID(input.AccountID)
	if account == nil || account.IsRelayStyle() {
		return
	}
	proxyURL := analyticsProxyURL(c, "")
	if proxyURL == "" {
		proxyURL = h.resolveProxyForAttempt(account, account.GetProxyURL())
	}
	credential := telemetry.Credential{AccessToken: account.GetAccessToken(), AccountID: account.EffectiveAccountID(), ProxyURL: EffectiveProxyURLForAccount(account, proxyURL)}
	if strings.TrimSpace(credential.AccessToken) == "" || strings.TrimSpace(credential.AccountID) == "" {
		return
	}

	identity := analyticsRequestIdentityFromContext(c, account)
	completedAt := time.Now().UTC()
	durationMS := max(input.DurationMs, 0)
	startedAt := completedAt.Add(-time.Duration(durationMS) * time.Millisecond)
	if !identity.TurnStartedAt.IsZero() && identity.TurnStartedAt.Before(completedAt) {
		startedAt = identity.TurnStartedAt
	}
	inputTokens := input.InputTokens
	if inputTokens <= 0 {
		inputTokens = input.PromptTokens
	}
	outputTokens := input.OutputTokens
	if outputTokens <= 0 {
		outputTokens = input.CompletionTokens
	}
	totalTokens := max(input.TotalTokens, max(inputTokens, 0)+max(outputTokens, 0))
	status := analyticsTurnStatus(input.StatusCode, c.Request)
	observation := analyticsResponseObservationFromContext(c)
	if input.InternalReason == analyticsContinueRound {
		observation = nil
	}
	turnInput := telemetry.TurnInput{
		ThreadID: identity.ThreadID, SessionID: identity.SessionID, TurnID: identity.TurnID, RootTurnID: identity.RootTurnID,
		ParentThreadID: identity.ParentThreadID, ThreadSource: identity.ThreadSource, SubagentSource: identity.SubagentSource, InitializationMode: identity.InitializationMode,
		Model: analyticsLabel(firstAnalyticsValue(input.EffectiveModel, input.Model)), ReasoningEffort: analyticsLabel(input.ReasoningEffort),
		ServiceTier: analyticsLabel(firstAnalyticsValue(input.ActualServiceTier, input.ServiceTier, input.RequestedServiceTier)),
		Status:      status, ErrorKind: analyticsErrorKind(input.UpstreamErrorKind, input.StatusCode), HTTPStatusCode: input.StatusCode,
		DurationMS: durationMS, InputTokens: max(inputTokens, 0), OutputTokens: max(outputTokens, 0), CachedInputTokens: max(input.CachedTokens, 0),
		ReasoningOutputTokens: max(input.ReasoningTokens, 0), TotalTokens: max(totalTokens, 0), SamplingRequestCount: 1,
		SamplingRetryCount: max(input.AttemptIndex-1, 0), StartedAt: startedAt, CompletedAt: completedAt,
	}
	if observation != nil {
		turnInput.TotalToolCallCount = observation.totalToolCalls
		turnInput.DynamicToolCallCount = observation.dynamicToolCalls
		turnInput.MCPToolCallCount = observation.mcpToolCalls
		turnInput.WebSearchCount = observation.webSearchCalls
		turnInput.ImageGenerationCount = observation.imageGenerationCalls
		turnInput.ShellCommandCount = observation.shellCommandCalls
	}
	finalTurn := status != "completed" || (observation != nil && observation.terminal && observation.totalToolCalls == 0 && !input.Compact)
	events := h.analyticsTurns.record(credential, turnInput, identity.Correlated, finalTurn)
	if input.Compact {
		trigger := "metadata"
		if isExplicitCompactUsageRequest(c, input) {
			trigger = "explicit"
		} else if meta, ok := cachedRequestCompactionMeta(c); ok && meta.ProtocolTriggered {
			trigger = "protocol"
		}
		compaction := telemetry.NewCompactionEvent(telemetry.CompactionInput{
			ThreadID: identity.ThreadID, SessionID: identity.SessionID, TurnID: identity.TurnID,
			ParentThreadID: identity.ParentThreadID, ThreadSource: identity.ThreadSource, SubagentSource: identity.SubagentSource,
			Phase: "completed", Implementation: "remote", Trigger: trigger, Strategy: "server", Status: status,
			ErrorKind: analyticsErrorKind(input.UpstreamErrorKind, input.StatusCode), HTTPStatusCode: input.StatusCode, DurationMS: durationMS,
			InputTokens: max(inputTokens, 0), CachedInputTokens: max(input.CachedTokens, 0), OutputTokens: max(outputTokens, 0),
			ReasoningOutputTokens: max(input.ReasoningTokens, 0), TotalTokens: max(totalTokens, 0), StartedAt: startedAt, CompletedAt: completedAt,
		})
		events = append(events, pendingAnalyticsEvent{credential: credential, event: compaction})
	}
	h.queueAnalytics(c, events)
}

func analyticsTurnEndpoint(inbound string, request *http.Request) bool {
	inbound = strings.TrimSpace(inbound)
	if inbound == "" && request != nil && request.URL != nil {
		inbound = request.URL.Path
	}
	switch inbound {
	case "/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/v1/messages", "/backend-api/codex/responses":
		return true
	default:
		return false
	}
}

func analyticsProxyURL(c *gin.Context, fallback string) string {
	if c != nil {
		if value, exists := c.Get("x-account-proxy"); exists {
			if proxyURL, ok := value.(string); ok && strings.TrimSpace(proxyURL) != "" {
				return strings.TrimSpace(proxyURL)
			}
		}
	}
	return strings.TrimSpace(fallback)
}

func analyticsTurnStatus(statusCode int, request *http.Request) string {
	if statusCode >= 200 && statusCode < 400 {
		return "completed"
	}
	if statusCode == logStatusClientClosed || (request != nil && request.Context().Err() != nil) {
		return "interrupted"
	}
	return "failed"
}

func analyticsErrorKind(kind string, statusCode int) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch {
	case statusCode == http.StatusTooManyRequests || strings.Contains(kind, "rate_limit") || strings.Contains(kind, "overload"):
		return "rate_limit"
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden || strings.Contains(kind, "auth") || strings.Contains(kind, "token"):
		return "authentication"
	case strings.Contains(kind, "timeout") || strings.Contains(kind, "deadline"):
		return "timeout"
	case strings.Contains(kind, "transport") || strings.Contains(kind, "network") || strings.Contains(kind, "stream"):
		return "transport"
	case statusCode >= 500:
		return "server"
	case statusCode >= 400:
		return "client"
	default:
		return ""
	}
}

func analyticsLabel(value string) string {
	value = strings.TrimSpace(value)
	if !analyticsLabelPattern.MatchString(value) {
		return ""
	}
	return value
}

func firstAnalyticsValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
