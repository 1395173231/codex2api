package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestShouldRetryHTTPStatusUnlimitedBudgets(t *testing.T) {
	t.Run("general transient statuses", func(t *testing.T) {
		for _, statusCode := range []int{
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout,
		} {
			t.Run(http.StatusText(statusCode), func(t *testing.T) {
				generalRetries := 0
				rateLimitRetries := 0
				for attempt := 0; attempt < 64; attempt++ {
					if !shouldRetryHTTPStatus(statusCode, nil, &generalRetries, &rateLimitRetries, -1, 0) {
						t.Fatalf("status %d stopped at attempt %d with an unlimited general budget", statusCode, attempt+1)
					}
				}
				if rateLimitRetries != 0 {
					t.Fatalf("status %d consumed rate-limit budget: %d", statusCode, rateLimitRetries)
				}
			})
		}
	})

	t.Run("rate limit budget stays independent", func(t *testing.T) {
		generalRetries := 0
		rateLimitRetries := 0
		for attempt := 0; attempt < 64; attempt++ {
			if !shouldRetryHTTPStatus(http.StatusTooManyRequests, nil, &generalRetries, &rateLimitRetries, 0, -1) {
				t.Fatalf("429 stopped at attempt %d with an unlimited rate-limit budget", attempt+1)
			}
		}
		if generalRetries != 0 {
			t.Fatalf("429 consumed general budget: %d", generalRetries)
		}
	})

	t.Run("404 remains non-retryable", func(t *testing.T) {
		generalRetries := 0
		rateLimitRetries := 0
		if shouldRetryHTTPStatus(http.StatusNotFound, []byte(`{"error":{"message":"not found"}}`), &generalRetries, &rateLimitRetries, -1, -1) {
			t.Fatal("404 must not become globally retryable in unlimited mode")
		}
		if generalRetries != 0 || rateLimitRetries != 0 {
			t.Fatalf("404 changed retry counters: general=%d rate_limit=%d", generalRetries, rateLimitRetries)
		}
	})
}

func TestShouldRetryHTTPStatusFiniteAndDisabledBudgets(t *testing.T) {
	t.Run("zero disables retries", func(t *testing.T) {
		generalRetries := 0
		rateLimitRetries := 0
		if shouldRetryHTTPStatus(http.StatusServiceUnavailable, nil, &generalRetries, &rateLimitRetries, 0, -1) {
			t.Fatal("general retry budget 0 must disable 503 retries")
		}
		if shouldRetryHTTPStatus(http.StatusTooManyRequests, nil, &generalRetries, &rateLimitRetries, -1, 0) {
			t.Fatal("rate-limit retry budget 0 must disable 429 retries")
		}
	})

	t.Run("positive limits keep exact existing semantics", func(t *testing.T) {
		generalRetries := 0
		rateLimitRetries := 0
		for attempt := 0; attempt < 2; attempt++ {
			if !shouldRetryHTTPStatus(http.StatusBadGateway, nil, &generalRetries, &rateLimitRetries, 2, 1) {
				t.Fatalf("502 retry %d unexpectedly denied", attempt+1)
			}
		}
		if shouldRetryHTTPStatus(http.StatusBadGateway, nil, &generalRetries, &rateLimitRetries, 2, 1) {
			t.Fatal("502 retry exceeded the finite general budget")
		}

		if !shouldRetryHTTPStatus(http.StatusTooManyRequests, nil, &generalRetries, &rateLimitRetries, 2, 1) {
			t.Fatal("first 429 retry unexpectedly denied")
		}
		if shouldRetryHTTPStatus(http.StatusTooManyRequests, nil, &generalRetries, &rateLimitRetries, 2, 1) {
			t.Fatal("429 retry exceeded the finite rate-limit budget")
		}
	})
}

func TestHTTPRetryBackoffStateUsesMatchingBudget(t *testing.T) {
	if ordinal, limit := retryStateForHTTPStatus(http.StatusTooManyRequests, 9, 2, -1, 3); ordinal != 2 || limit != 3 {
		t.Fatalf("429 retry state = (%d, %d), want (2, 3)", ordinal, limit)
	}
	if ordinal, limit := retryStateForHTTPStatus(http.StatusServiceUnavailable, 4, 11, -1, 1); ordinal != 4 || limit != -1 {
		t.Fatalf("503 retry state = (%d, %d), want (4, -1)", ordinal, limit)
	}
}

func TestShouldRetryRequestErrorBudgetModes(t *testing.T) {
	retryable := errors.New("read tcp: connection reset by peer")

	t.Run("unlimited", func(t *testing.T) {
		generalRetries := 0
		for attempt := 0; attempt < 64; attempt++ {
			if !shouldRetryRequestError(retryable, &generalRetries, -1) {
				t.Fatalf("transport retry stopped at attempt %d with an unlimited budget", attempt+1)
			}
		}
	})

	t.Run("zero", func(t *testing.T) {
		generalRetries := 0
		if shouldRetryRequestError(retryable, &generalRetries, 0) {
			t.Fatal("transport retry budget 0 must disable retries")
		}
	})

	t.Run("finite", func(t *testing.T) {
		generalRetries := 0
		if !shouldRetryRequestError(retryable, &generalRetries, 1) {
			t.Fatal("first transport retry unexpectedly denied")
		}
		if shouldRetryRequestError(retryable, &generalRetries, 1) {
			t.Fatal("transport retry exceeded the finite budget")
		}
	})
}

func TestRetryableRequestErrorStructuredPrecedence(t *testing.T) {
	networkCause := errors.New("connection reset by peer")
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "bad request", err: ErrBadRequest("invalid payload")},
		{name: "wrapped bad request", err: fmt.Errorf("executor: %w", ErrBadRequest("invalid payload"))},
		{name: "internal error with network-looking cause", err: ErrInternalError("serialization failed", networkCause)},
		{name: "wrapped internal error", err: fmt.Errorf("executor: %w", ErrInternalError("serialization failed", networkCause))},
		{name: "statusful non-retryable upstream error", err: ErrUpstream(http.StatusNotFound, "missing endpoint", networkCause)},
		{name: "statusless upstream transport error", err: ErrUpstream(0, "request failed", networkCause), want: true},
		{name: "plain transport error", err: networkCause, want: true},
		{name: "canceled error", err: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableRequestError(tt.err); got != tt.want {
				t.Fatalf("isRetryableRequestError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if isRetryableRequestErrorForContext(ctx, networkCause) {
		t.Fatal("a downstream-canceled request must not classify a transport error as retryable")
	}
}

func TestUnlimitedTransparentStreamRetryBoundaries(t *testing.T) {
	retryable := streamOutcome{
		logStatusCode:  logStatusUpstreamStreamBreak,
		failureKind:    "transport",
		failureMessage: "upstream failed before first downstream byte",
		penalize:       true,
	}

	if !shouldTransparentRetryStream(retryable, 100_000, -1, false, nil, nil) {
		t.Fatal("an early stream break should remain retryable with an unlimited budget")
	}
	if shouldTransparentRetryStream(retryable, 0, -1, true, nil, nil) {
		t.Fatal("a stream must never be replayed after downstream body bytes were written")
	}
	if shouldTransparentRetryStream(retryable, 0, -1, false, context.Canceled, nil) {
		t.Fatal("an unlimited retry loop must stop when the downstream context is canceled")
	}
	if shouldTransparentRetryStream(retryable, 0, -1, false, nil, errors.New("downstream write failed")) {
		t.Fatal("an unlimited retry loop must stop after a downstream write failure")
	}
	if shouldTransparentRetryStream(streamOutcome{penalize: false}, 0, -1, false, nil, nil) {
		t.Fatal("a non-retryable stream outcome must stay non-retryable in unlimited mode")
	}
	if shouldTransparentRetryStream(retryable, 0, 0, false, nil, nil) {
		t.Fatal("stream retry budget 0 must disable transparent retries")
	}
}

func TestTransparentStreamRetryBudgetsStayIndependent(t *testing.T) {
	rateLimited := streamOutcome{
		logStatusCode:  http.StatusTooManyRequests,
		failureKind:    "rate_limited",
		failureMessage: "temporarily rate limited",
		penalize:       true,
	}
	generalRetries := 0
	rateLimitRetries := 0
	for attempt := 0; attempt < 32; attempt++ {
		if !shouldTransparentRetryStreamWithBudgets(rateLimited, &generalRetries, &rateLimitRetries, 0, -1, false, nil, nil) {
			t.Fatalf("stream 429 stopped at attempt %d despite an unlimited rate-limit budget", attempt+1)
		}
	}
	if generalRetries != 0 || rateLimitRetries != 32 {
		t.Fatalf("stream 429 counters = general:%d rate_limit:%d, want 0/32", generalRetries, rateLimitRetries)
	}

	serverFailure := streamOutcome{
		logStatusCode:  http.StatusServiceUnavailable,
		failureKind:    "server",
		failureMessage: "temporarily unavailable",
		penalize:       true,
	}
	generalRetries = 0
	rateLimitRetries = 0
	if !shouldTransparentRetryStreamWithBudgets(serverFailure, &generalRetries, &rateLimitRetries, -1, 1, false, nil, nil) {
		t.Fatal("503 did not consume the unlimited general budget")
	}
	if !shouldTransparentRetryStreamWithBudgets(rateLimited, &generalRetries, &rateLimitRetries, -1, 1, false, nil, nil) {
		t.Fatal("first stream 429 retry was denied by the general counter")
	}
	if shouldTransparentRetryStreamWithBudgets(rateLimited, &generalRetries, &rateLimitRetries, -1, 1, false, nil, nil) {
		t.Fatal("stream 429 exceeded its finite independent rate-limit budget")
	}
}

func TestUnlimitedImageRetryStillHonorsTotalAttemptCap(t *testing.T) {
	generalRetries := 0
	streamErr := errors.New("upstream image stream disconnected")
	for attempt := 0; attempt < maxImageAttempts-1; attempt++ {
		if !shouldRetryImageStreamError(streamErr, &generalRetries, -1, attempt, maxImageAttempts) {
			t.Fatalf("image retry %d was denied before the total-attempt cap", attempt+1)
		}
	}
	if shouldRetryImageStreamError(streamErr, &generalRetries, -1, maxImageAttempts-1, maxImageAttempts) {
		t.Fatal("unlimited text retry budget bypassed the image total-attempt cap")
	}
}

func TestUnlimitedResponseFailedSuppressionBoundaries(t *testing.T) {
	retryableFailed := []byte(`{"type":"response.failed","response":{"error":{"message":"upstream overloaded","status_code":503,"code":"server_error"}}}`)

	if !shouldSuppressRetryableResponseFailedBeforeFirstToken("response.failed", retryableFailed, false, false, 100_000, -1, nil, nil) {
		t.Fatal("a retryable response.failed should stay hidden before the first token in unlimited mode")
	}
	if shouldSuppressRetryableResponseFailedBeforeFirstToken("response.failed", retryableFailed, true, false, 0, -1, nil, nil) {
		t.Fatal("response.failed must not be hidden after first-token progress")
	}
	if shouldSuppressRetryableResponseFailedBeforeFirstToken("response.failed", retryableFailed, false, true, 0, -1, nil, nil) {
		t.Fatal("response.failed must not be hidden after downstream body bytes were written")
	}
	if shouldSuppressRetryableResponseFailedBeforeFirstToken("response.failed", retryableFailed, false, false, 0, -1, context.Canceled, nil) {
		t.Fatal("response.failed must not be hidden after downstream cancellation")
	}
	if shouldSuppressRetryableResponseFailedBeforeFirstToken("response.failed", retryableFailed, false, false, 0, -1, nil, errors.New("downstream write failed")) {
		t.Fatal("response.failed must not be hidden after a downstream write failure")
	}
	if shouldSuppressRetryableResponseFailedBeforeFirstToken("response.failed", retryableFailed, false, false, 0, 0, nil, nil) {
		t.Fatal("response.failed must not be hidden when retries are disabled")
	}
}

func TestWaitBeforeRetryDeadlineCancelsLongInterval(t *testing.T) {
	h, store := newRetryTestHandler(t)
	store.SetRetryIntervalMS(30_000)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	if h.waitBeforeRetry(ctx) {
		t.Fatal("waitBeforeRetry returned true after the downstream deadline")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("downstream deadline did not interrupt retry wait promptly: %v", elapsed)
	}
}

func TestUnlimitedRetryInvalidRetryAfterFallsBackToBackoff(t *testing.T) {
	h, store := newRetryTestHandler(t)
	store.SetRetryIntervalMS(0)
	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("Retry-After", "not-a-valid-delay")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if h.waitBeforeRetryWithBudget(ctx, 1, -1, resp) {
		t.Fatal("invalid Retry-After bypassed unlimited retry backoff")
	}
}

func TestResponsesContinuousRetryCyclesSingleAccountAfter503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousRuntime := CurrentRuntimeSettings()
	t.Cleanup(func() {
		UpdateRuntimeSettings(func(RuntimeSettings) RuntimeSettings { return previousRuntime })
	})
	UpdateRuntimeSettings(func(current RuntimeSettings) RuntimeSettings {
		current.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
			Enabled:    true,
			Categories: []string{database.ContinuousRetryCategoryHTTP5xx},
		}
		return current
	})

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"temporarily unavailable"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_retried","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	t.Cleanup(upstream.Close)

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      1,
		TestConcurrency:     1,
		TestModel:           "gpt-5.4",
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      upstream.URL,
		APIKey:       "test-relay-key",
		Models:       []string{"gpt-5.4"},
		PlanType:     "api",
	})
	handler := NewHandler(store, nil, nil, nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	requestCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.4","input":"hello","stream":true}`)).WithContext(requestCtx)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.Responses(ctx)

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (503 then success on the same account)", got)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("downstream status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"type":"response.completed"`) || strings.Contains(body, "temporarily unavailable") {
		t.Fatalf("retry was not transparent: %s", body)
	}
}

func TestResponsesContinuousRetrySelectedDeterministicStatuses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, statusCode := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(statusCode)
					_, _ = io.WriteString(w, fmt.Sprintf(`{"error":{"code":"status_%d","message":"selected deterministic failure"}}`, statusCode))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_selected_status","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
			}))
			t.Cleanup(upstream.Close)

			policy := database.ContinuousRetryPolicy{Enabled: true, StatusCodes: []int{statusCode}}
			previousRuntime := CurrentRuntimeSettings()
			t.Cleanup(func() {
				UpdateRuntimeSettings(func(RuntimeSettings) RuntimeSettings { return previousRuntime })
			})
			UpdateRuntimeSettings(func(current RuntimeSettings) RuntimeSettings {
				current.ContinuousRetryPolicy = policy
				current.CodexForceWebsocket = false
				current.CodexWSSilentRetry = false
				current.CodexWSSilentRetries = 0
				return current
			})

			store := auth.NewStore(nil, nil, &database.SystemSettings{
				MaxConcurrency:      1,
				TestConcurrency:     1,
				TestModel:           "gpt-5.4",
				MaxRetries:          0,
				MaxRateLimitRetries: 0,
			})
			t.Cleanup(store.Stop)
			for _, id := range []int64{1, 2} {
				store.AddAccount(&auth.Account{
					DBID:         id,
					UpstreamType: auth.UpstreamOpenAIResponses,
					BaseURL:      upstream.URL,
					APIKey:       fmt.Sprintf("test-relay-key-%d", id),
					Models:       []string{"gpt-5.4"},
					PlanType:     "api",
				})
			}
			handler := NewHandler(store, nil, nil, nil)

			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			requestCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.4","input":"hello","stream":true}`)).WithContext(requestCtx)
			ctx.Request.Header.Set("Content-Type", "application/json")
			handler.Responses(ctx)

			if got := calls.Load(); got != 2 {
				t.Fatalf("status %d upstream calls = %d, want 2", statusCode, got)
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("status %d downstream status = %d, want 200; body=%s", statusCode, recorder.Code, recorder.Body.String())
			}
			body := recorder.Body.String()
			if !strings.Contains(body, `"type":"response.completed"`) || strings.Contains(body, "selected deterministic failure") {
				t.Fatalf("status %d retry was not transparent: %s", statusCode, body)
			}
		})
	}
}

func TestResponsesCompactContinuousRetryCyclesSingleAccountAfter503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousRuntime := CurrentRuntimeSettings()
	t.Cleanup(func() {
		UpdateRuntimeSettings(func(RuntimeSettings) RuntimeSettings { return previousRuntime })
	})
	UpdateRuntimeSettings(func(current RuntimeSettings) RuntimeSettings {
		current.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
			Enabled:    true,
			Categories: []string{database.ContinuousRetryCategoryHTTP5xx},
		}
		return current
	})

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"temporarily unavailable"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_compact_retried","object":"response.compaction","output":[]}`)
	}))
	t.Cleanup(upstream.Close)

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      1,
		TestConcurrency:     1,
		TestModel:           "gpt-5.4",
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      upstream.URL,
		APIKey:       "test-relay-key",
		Models:       []string{"gpt-5.4"},
		PlanType:     "api",
	})
	handler := NewHandler(store, nil, nil, nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	requestCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewBufferString(`{"model":"gpt-5.4","input":"hello"}`)).WithContext(requestCtx)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.ResponsesCompact(ctx)

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (503 then compact success on the same account)", got)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("downstream status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"id":"resp_compact_retried"`) || strings.Contains(body, "temporarily unavailable") {
		t.Fatalf("compact retry was not transparent: %s", body)
	}
}

func TestResponsesCompactContinuousRetrySelectsResponseFailedEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousRuntime := CurrentRuntimeSettings()
	previousResin := resinCfg.Load()
	t.Cleanup(func() {
		UpdateRuntimeSettings(func(RuntimeSettings) RuntimeSettings { return previousRuntime })
		resinCfg.Store(previousResin)
	})
	UpdateRuntimeSettings(func(current RuntimeSettings) RuntimeSettings {
		current.CompactViaResponses = true
		current.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
			Enabled:    true,
			Categories: []string{database.ContinuousRetryCategoryResponseFailed},
		}
		return current
	})

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/backend-api/codex/responses") {
			t.Fatalf("upstream path = %q, want Resin path ending /backend-api/codex/responses", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, `data: {"type":"response.failed","response":{"id":"resp_failed_once","status":"failed","status_code":503,"error":{"code":"server_error","message":"temporary compact failure"}}}`+"\n\n")
			return
		}
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_compact_recovered","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`+"\n\n")
	}))
	t.Cleanup(upstream.Close)
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      1,
		TestConcurrency:     1,
		TestModel:           "gpt-5.4",
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{
		DBID:        1,
		AccessToken: "test-token",
		AccountID:   "test-account",
		Models:      []string{"gpt-5.4"},
		PlanType:    "pro",
	})
	handler := NewHandler(store, nil, nil, nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	requestCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewBufferString(`{"model":"gpt-5.4","input":"hello"}`)).WithContext(requestCtx)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.ResponsesCompact(ctx)

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (response.failed then success on the same account)", got)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("downstream status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"id":"resp_compact_recovered"`) || strings.Contains(body, "temporary compact failure") {
		t.Fatalf("compact body-signal retry was not transparent: %s", body)
	}
}
