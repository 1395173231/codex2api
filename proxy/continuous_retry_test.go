package proxy

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/codex2api/database"
)

type continuousRetryTestHTTPError struct {
	status int
	body   []byte
}

func (e continuousRetryTestHTTPError) Error() string             { return "upstream websocket handshake failed" }
func (e continuousRetryTestHTTPError) UpstreamStatusCode() int   { return e.status }
func (e continuousRetryTestHTTPError) UpstreamErrorBody() []byte { return e.body }

func TestContinuousRetryHTTPSelectionIsOptInAndExact(t *testing.T) {
	disabled := database.ContinuousRetryPolicy{
		Enabled:     false,
		Categories:  []string{database.ContinuousRetryCategoryHTTP4xx},
		StatusCodes: []int{404},
	}
	general, rate := 0, 0
	if shouldRetryHTTPStatus(http.StatusNotFound, nil, &general, &rate, 0, 0, disabled) {
		t.Fatal("disabled continuous policy enabled a 404 retry")
	}

	selected := database.ContinuousRetryPolicy{
		Enabled:     true,
		StatusCodes: []int{http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented},
	}
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented} {
		general, rate = 0, 0
		if !shouldRetryHTTPStatus(status, nil, &general, &rate, 0, 0, selected) {
			t.Fatalf("selected status %d was not retried", status)
		}
		if general != 1 || rate != 0 {
			t.Fatalf("selected status %d consumed budgets general=%d rate=%d", status, general, rate)
		}
	}
	general, rate = 0, 0
	if shouldRetryHTTPStatus(http.StatusBadRequest, nil, &general, &rate, 0, 0, selected) {
		t.Fatal("unselected 400 was retried")
	}
}

func TestContinuousRetryHTTPSelectionSupportsContextCategory(t *testing.T) {
	policy := database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryContextError},
	}
	general, rate := 0, 0
	body := []byte(`{"error":{"code":"context_length_exceeded"}}`)
	if !shouldRetryHTTPStatus(http.StatusBadRequest, body, &general, &rate, 0, 0, policy) {
		t.Fatal("context category did not select a context-length HTTP error")
	}
	if shouldRetryHTTPStatus(http.StatusBadRequest, []byte(`{"error":{"code":"invalid_request"}}`), &general, &rate, 0, 0, policy) {
		t.Fatal("context category selected an unrelated 400")
	}
}

func TestContinuousRetryCatchAllSelectsUnknownUpstreamFailures(t *testing.T) {
	policy := database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}

	for _, status := range []int{http.StatusTeapot, 499, 520, 599, 600, 701} {
		general, rate := 0, 0
		if !shouldRetryHTTPStatus(status, []byte(`{"error":{"code":"never_seen_before"}}`), &general, &rate, 0, 0, policy) {
			t.Fatalf("catch-all policy did not retry unknown HTTP status %d", status)
		}
	}

	quota := []byte(`{"error":{"code":"insufficient_quota"}}`)
	if !continuousRetryHTTPSelected(policy, http.StatusTooManyRequests, quota) {
		t.Fatal("catch-all policy did not explicitly override the permanent-quota guard")
	}

	unknownFrame := []byte(`{"type":"error","error":{"code":"future_upstream_failure"}}`)
	outcome := classifyResponseFailedOutcome(unknownFrame)
	if !continuousRetryStreamSelected(outcome, unknownFrame, "error", policy) {
		t.Fatal("catch-all policy did not retry an unknown upstream error frame")
	}

	general, rate := 0, 0
	if shouldTransparentRetryStreamWithBudgets(outcome, &general, &rate, 0, 0, true, nil, nil, policy) {
		t.Fatal("catch-all policy replayed a failure after downstream output")
	}
	if shouldTransparentRetryStreamWithBudgets(outcome, &general, &rate, 0, 0, false, context.Canceled, nil, policy) {
		t.Fatal("catch-all policy ignored downstream cancellation")
	}
}

func TestContinuousRetryNeverSelectsStructuredSafetyRefusals(t *testing.T) {
	policy := database.ContinuousRetryPolicy{
		Enabled:     true,
		CatchAll:    true,
		Categories:  []string{database.ContinuousRetryCategoryHTTP4xx, database.ContinuousRetryCategoryHTTP5xx, database.ContinuousRetryCategoryResponseFailed},
		StatusCodes: []int{http.StatusBadRequest, http.StatusInternalServerError},
		ErrorCodes:  []string{"content_policy_violation"},
	}
	for _, body := range [][]byte{
		[]byte(`{"error":{"code":"cyber_policy"}}`),
		[]byte(`{"error":{"type":"content_policy_violation"}}`),
		[]byte(`{"type":"response.failed","response":{"error":{"code":"moderation_blocked"}}}`),
	} {
		if continuousRetryHTTPSelected(policy, http.StatusInternalServerError, body) {
			t.Fatalf("structured safety refusal selected for HTTP retry: %s", body)
		}
		outcome := classifyResponseFailedOutcome(body)
		if continuousRetryStreamSelected(outcome, body, "response.failed", policy) {
			t.Fatalf("structured safety refusal selected for stream retry: %s", body)
		}
		general, rate := 0, 0
		if shouldRetryHTTPStatus(http.StatusInternalServerError, body, &general, &rate, 2, 2, policy) {
			t.Fatalf("structured safety refusal used a legacy HTTP retry: %s", body)
		}
		if general != 0 || rate != 0 {
			t.Fatalf("structured safety refusal consumed retry counters: general=%d rate=%d", general, rate)
		}
		if shouldTransparentRetryStreamWithBudgets(outcome, &general, &rate, 2, 2, false, nil, nil, policy) {
			t.Fatalf("structured safety refusal used a legacy stream retry: %s", body)
		}
	}

	ordinary := []byte(`{"error":{"code":"server_error","message":"content policy service temporarily unavailable"}}`)
	if !continuousRetryHTTPSelected(policy, http.StatusInternalServerError, ordinary) {
		t.Fatal("an unstructured message mention suppressed an otherwise selected 500")
	}
}

func TestContinuousRetryNeverRetriesStructuredHandshakeSafetyRefusal(t *testing.T) {
	policy := database.ContinuousRetryPolicy{
		Enabled:     true,
		Categories:  []string{database.ContinuousRetryCategoryTransport, database.ContinuousRetryCategoryHTTP4xx},
		StatusCodes: []int{http.StatusForbidden},
		ErrorCodes:  []string{"content_policy_violation"},
	}
	err := continuousRetryTestHTTPError{
		status: http.StatusForbidden,
		body:   []byte(`{"error":{"code":"content_policy_violation"}}`),
	}
	general := 0
	if isRetryableRequestErrorForContext(context.Background(), err, policy) {
		t.Fatal("structured handshake safety refusal was classified as retryable")
	}
	if shouldRetryRequestError(err, &general, 2, policy) {
		t.Fatal("structured handshake safety refusal used a finite or continuous retry")
	}
	if general != 0 {
		t.Fatalf("structured handshake safety refusal consumed general retries: %d", general)
	}
}

func TestContinuousRetryTransportAndStreamSelection(t *testing.T) {
	transport := database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryTransport},
	}
	general := 0
	if !shouldRetryRequestError(errors.New("unexpected EOF"), &general, 0, transport) {
		t.Fatal("transport category did not enable transport retry")
	}
	if shouldRetryRequestError(ErrBadRequest("invalid request"), &general, 0, transport) {
		t.Fatal("transport category selected a non-transport error")
	}
	statusless := ErrUpstream(0, "request failed", errors.New("connection reset"))
	if !shouldRetryRequestError(statusless, &general, 0, transport) {
		t.Fatal("transport category did not select a statusless upstream request error")
	}
	serverOnly := database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryHTTP5xx},
	}
	if isRetryableRequestErrorForContext(context.Background(), ErrInternalError("serialization failed", errors.New("bad state")), serverOnly) {
		t.Fatal("an internal 500 was misclassified as an upstream 5xx")
	}

	stream := database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryResponseFailed},
	}
	outcome := streamOutcome{logStatusCode: http.StatusBadRequest, failureKind: "client", failurePayload: []byte(`{"type":"response.failed"}`)}
	general, rate := 0, 0
	if !shouldTransparentRetryStreamWithBudgets(outcome, &general, &rate, 0, 0, false, nil, nil, stream) {
		t.Fatal("response.failed category did not enable a pre-output retry")
	}
	if shouldTransparentRetryStreamWithBudgets(outcome, &general, &rate, -1, -1, true, nil, nil, stream) {
		t.Fatal("response.failed was replayed after downstream output")
	}
}

func TestContinuousRetryStreamErrorDoesNotSelectDeterministicResponseFailed(t *testing.T) {
	defaultPolicy := database.DefaultContinuousRetryPolicy()
	defaultPolicy.Enabled = true
	invalidRequest := []byte(`{"type":"response.failed","response":{"status_code":400,"error":{"code":"invalid_request"}}}`)
	outcome := classifyResponseFailedOutcome(invalidRequest)
	if continuousRetryStreamSelected(outcome, invalidRequest, "response.failed", defaultPolicy) {
		t.Fatal("default stream-error policy selected a deterministic invalid_request response.failed event")
	}

	responseFailedPolicy := database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryResponseFailed},
	}
	if !continuousRetryStreamSelected(outcome, invalidRequest, "response.failed", responseFailedPolicy) {
		t.Fatal("explicit response.failed category did not select the event")
	}

	readFailure := streamOutcome{
		logStatusCode:  logStatusUpstreamStreamBreak,
		failureKind:    "transport",
		failurePayload: []byte("unexpected EOF"),
	}
	streamErrorPolicy := database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryStreamError},
	}
	if !continuousRetryStreamSelected(readFailure, readFailure.failurePayload, "", streamErrorPolicy) {
		t.Fatal("stream-error category did not select a real stream read failure")
	}
}

func TestContinuousRetryImageStreamSelectionHonorsPolicyAndSafety(t *testing.T) {
	policy := database.DefaultContinuousRetryPolicy()
	policy.Enabled = true

	if limit := imageStreamRetryLimit(errors.New("unexpected EOF"), 0, policy); limit != -1 {
		t.Fatalf("image transport retry limit = %d, want -1", limit)
	}
	serverFailure := newImageResponseFailedError([]byte(`{"type":"response.failed","response":{"status_code":503,"error":{"code":"server_error"}}}`))
	if limit := imageStreamRetryLimit(serverFailure, 0, policy); limit != -1 {
		t.Fatalf("image response.failed 503 retry limit = %d, want -1", limit)
	}
	safetyFailure := newImageResponseFailedError([]byte(`{"type":"response.failed","response":{"status_code":500,"error":{"code":"content_policy_violation"}}}`))
	if limit := imageStreamRetryLimit(safetyFailure, 2, policy); limit != 2 {
		t.Fatalf("image safety failure retry limit = %d, want finite limit 2", limit)
	}
	moderationFailure := newImageResponseFailedError([]byte(`{"type":"response.failed","response":{"status_code":500,"error":{"code":"moderation_blocked"}}}`))
	general := 0
	if shouldRetryImageStreamError(moderationFailure, &general, 2, 0, maxImageAttempts, policy) {
		t.Fatal("structured image moderation refusal was retried")
	}
	quotaFailure := newImageResponseFailedError([]byte(`{"type":"response.failed","response":{"status_code":429,"error":{"code":"insufficient_quota"}}}`))
	if shouldRetryImageStreamError(quotaFailure, &general, 2, 0, maxImageAttempts, policy) {
		t.Fatal("unselected permanent image quota failure was retried")
	}
	policy.ErrorCodes = []string{"insufficient_quota"}
	if !shouldRetryImageStreamError(quotaFailure, &general, 0, 0, maxImageAttempts, policy) {
		t.Fatal("explicitly selected image quota code did not use the bounded continuous retry")
	}
	catchAll := database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	if !shouldRetryImageStreamError(quotaFailure, &general, 0, 0, maxImageAttempts, catchAll) {
		t.Fatal("catch-all did not select a permanent image quota failure")
	}
	if shouldRetryImageStreamError(quotaFailure, &general, 0, maxImageAttempts-1, maxImageAttempts, catchAll) {
		t.Fatal("catch-all bypassed the image total-attempt cap")
	}
}

func TestContinuousRetryErrorFrameUsesSelectedPolicyBeforeResponseFailed(t *testing.T) {
	policy := database.ContinuousRetryPolicy{
		Enabled:     true,
		StatusCodes: []int{http.StatusForbidden},
	}
	errorFrame := []byte(`{"type":"error","error":{"status_code":403,"code":"forbidden","message":"account restricted"}}`)
	if !isRetryableUpstreamErrorFrame("error", errorFrame, policy) {
		t.Fatal("selected 403 error frame was not classified as retryable")
	}
	if isRetryableUpstreamErrorFrame("error", errorFrame, database.ContinuousRetryPolicy{}) {
		t.Fatal("disabled policy unexpectedly selected a deterministic error frame")
	}
	if isRetryableUpstreamErrorFrame("response.output_text.delta", errorFrame, policy) {
		t.Fatal("non-error event was classified as a retryable error frame")
	}
}

func TestContinuousRetryBackoffStateUsesSelectedBodyPolicy(t *testing.T) {
	policy := database.ContinuousRetryPolicy{
		Enabled:    true,
		ErrorCodes: []string{"account_temporarily_unavailable"},
	}
	general, rate := 1, 0
	ordinal, limit := retryStateForHTTPStatusWithBody(http.StatusBadRequest,
		[]byte(`{"error":{"code":"account_temporarily_unavailable"}}`),
		general, rate, 0, 0, policy)
	if ordinal != general || limit != -1 {
		t.Fatalf("selected exact-code HTTP state = (%d, %d), want (%d, -1)", ordinal, limit, general)
	}

	outcome := streamOutcome{
		logStatusCode:  http.StatusForbidden,
		failureKind:    "forbidden",
		failurePayload: []byte(`{"type":"response.failed","response":{"status_code":403}}`),
	}
	ordinal, limit = retryStateForStreamOutcome(outcome, 2, 0, 0, 0, database.ContinuousRetryPolicy{
		Enabled:     true,
		StatusCodes: []int{http.StatusForbidden},
	})
	if ordinal != 2 || limit != -1 {
		t.Fatalf("selected stream state = (%d, %d), want (2, -1)", ordinal, limit)
	}
}

func TestContinuousRetryRequestErrorCanSelectHandshakeStatus(t *testing.T) {
	policy := database.ContinuousRetryPolicy{Enabled: true, StatusCodes: []int{http.StatusForbidden}}
	err := continuousRetryTestHTTPError{
		status: http.StatusForbidden,
		body:   []byte(`{"error":{"code":"cloudflare_forbidden","message":"blocked"}}`),
	}
	general := 0
	if !shouldRetryRequestError(err, &general, 0, policy) {
		t.Fatal("selected handshake 403 was not retried with the legacy budget disabled")
	}
	if general != 1 {
		t.Fatalf("handshake retry counter = %d, want 1", general)
	}

	transportOnly := database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryTransport},
	}
	general = 0
	if !shouldRetryRequestError(err, &general, 1, transportOnly) {
		t.Fatal("unselected handshake 403 lost its legacy finite transport retry")
	}
	if shouldRetryRequestError(err, &general, 1, transportOnly) {
		t.Fatal("transport category promoted an unselected handshake 403 to continuous retry")
	}
}

func TestContinuousRetryRequestErrorCanPromoteStructuredStatusFailure(t *testing.T) {
	policy := database.ContinuousRetryPolicy{Enabled: true, StatusCodes: []int{http.StatusForbidden}}
	err := ErrUpstream(http.StatusForbidden, "account restricted", errors.New("upstream denied"))
	if isRetryableRequestErrorForContext(context.Background(), err, policy) != true {
		t.Fatal("selected structured 403 was not classified as retryable")
	}
	if isRetryableRequestErrorForContext(context.Background(), err) {
		t.Fatal("structured 403 became retryable without an opt-in policy")
	}
}

func TestContinuousRetryCatchAllSelectsOnlyStructuredStatuslessUpstreamErrors(t *testing.T) {
	policy := database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	upstreamErr := ErrUpstream(0, "upstream failed without a classified cause", nil)
	if !isRetryableRequestErrorForContext(context.Background(), upstreamErr, policy) {
		t.Fatal("catch-all did not select a statusless structured upstream error")
	}
	general := 0
	if !shouldRetryRequestError(upstreamErr, &general, 0, policy) {
		t.Fatal("catch-all did not override the finite budget for a statusless structured upstream error")
	}
	if isRetryableRequestErrorForContext(context.Background(), ErrBadRequest("invalid local request"), policy) {
		t.Fatal("catch-all selected an internal bad-request error")
	}
	if isRetryableRequestErrorForContext(context.Background(), ErrInternalError("internal failure", nil), policy) {
		t.Fatal("catch-all selected an internal server error")
	}
	safetyErr := &Error{
		Code:    "cyber_policy",
		Message: "blocked",
		Type:    ErrorTypeUpstreamError,
	}
	if isRetryableRequestErrorForContext(context.Background(), safetyErr, policy) {
		t.Fatal("catch-all selected a statusless structured safety refusal")
	}
	canceled := ErrUpstream(0, "upstream request canceled", context.Canceled)
	if isRetryableRequestErrorForContext(context.Background(), canceled, policy) {
		t.Fatal("catch-all selected a canceled upstream request")
	}
	deadline := ErrUpstream(0, "upstream request timed out", context.DeadlineExceeded)
	general = 0
	if !shouldRetryRequestError(deadline, &general, 0, policy) {
		t.Fatal("catch-all did not select an upstream deadline while the downstream context remained active")
	}
	downstreamCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if isRetryableRequestErrorForContext(downstreamCtx, deadline, policy) {
		t.Fatal("catch-all ignored a canceled downstream context")
	}
}

func TestContinuousRetryCatchAllSelectsNonstandardHandshakeStatus(t *testing.T) {
	policy := database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	err := continuousRetryTestHTTPError{
		status: 701,
		body:   []byte(`{"error":{"code":"future_handshake_failure"}}`),
	}
	general := 0
	if !shouldRetryRequestError(err, &general, 0, policy) {
		t.Fatal("catch-all did not select a nonstandard upstream handshake status")
	}
}
