package proxy

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/codex2api/database"
)

func continuousRetryPolicyForCall(policies []database.ContinuousRetryPolicy) database.ContinuousRetryPolicy {
	if len(policies) > 0 {
		return database.NormalizeContinuousRetryPolicy(policies[0])
	}
	return database.NormalizeContinuousRetryPolicy(CurrentRuntimeSettings().ContinuousRetryPolicy)
}

func continuousRetryHTTPSelected(policy database.ContinuousRetryPolicy, status int, body []byte) bool {
	if !policy.Enabled || isExplicitUpstreamSafetyPolicy(body) {
		return false
	}
	// A generic 429 category should not turn a permanent billing/quota failure
	// into an endless account-rotation loop. An operator can still opt into that
	// exact marker or deliberately enable the high-risk catch-all override.
	if isPermanentQuotaFailure(body) && !policy.CatchesAllUpstreamFailures() && !policy.MatchesErrorCodes(body) {
		return false
	}
	if policy.MatchesHTTP(status, body) {
		return true
	}
	// Context-window failures are commonly returned as a 400 body (or inside a
	// response.failed frame) rather than a dedicated status. Keep the category
	// useful for both HTTP and streaming transports without making every 400
	// retryable by default.
	return policy.HasCategory(database.ContinuousRetryCategoryContextError) && isContextRetryError(body)
}

func continuousRetryLimitsForHTTP(status int, body []byte, generalLimit, rateLimit int, policies ...database.ContinuousRetryPolicy) (int, int) {
	policy := continuousRetryPolicyForCall(policies)
	if !continuousRetryHTTPSelected(policy, status, body) {
		return generalLimit, rateLimit
	}
	if status == http.StatusTooManyRequests {
		return generalLimit, -1
	}
	return -1, rateLimit
}

func continuousRetryLimitForRequestError(err error, generalLimit int, policies ...database.ContinuousRetryPolicy) int {
	policy := continuousRetryPolicyForCall(policies)
	if err == nil || errors.Is(err, context.Canceled) {
		return generalLimit
	}
	if status, body, ok := continuousRetryHTTPErrorDetails(err); ok {
		if continuousRetryHTTPSelected(policy, status, body) {
			return -1
		}
		// A WebSocket handshake HTTP response is not a transport failure. Keep
		// its legacy finite budget unless its status/body was selected explicitly.
		return generalLimit
	}
	if policy.MatchesTransport(err.Error()) {
		return -1
	}
	return generalLimit
}

// continuousRetryRequestErrorSelected recognizes status-bearing upstream
// failures that never materialize as an *http.Response, notably WebSocket
// handshake errors. The small interface keeps proxy independent from the
// wsrelay package while allowing exact status/error-code selectors to work on
// both transports.
type continuousRetryHTTPError interface {
	UpstreamStatusCode() int
	UpstreamErrorBody() []byte
}

func continuousRetryHTTPErrorDetails(err error) (int, []byte, bool) {
	var upstreamErr continuousRetryHTTPError
	if !errors.As(err, &upstreamErr) {
		return 0, nil, false
	}
	status := upstreamErr.UpstreamStatusCode()
	if status < 100 || status > 999 {
		return 0, nil, false
	}
	return status, upstreamErr.UpstreamErrorBody(), true
}

func continuousRetryRequestErrorSelected(policy database.ContinuousRetryPolicy, err error) bool {
	if !policy.Enabled || err == nil {
		return false
	}
	status, body, ok := continuousRetryHTTPErrorDetails(err)
	if !ok {
		if !policy.CatchesAllUpstreamFailures() || errors.Is(err, context.Canceled) {
			return false
		}
		var upstreamErr *Error
		return errors.As(err, &upstreamErr) &&
			upstreamErr.Type == ErrorTypeUpstreamError &&
			!isExplicitUpstreamSafetyPolicy(upstreamErr.UpstreamErrorBody())
	}
	return continuousRetryHTTPSelected(policy, status, body)
}

func continuousRetryStreamSelected(outcome streamOutcome, payload []byte, eventType string, policies ...database.ContinuousRetryPolicy) bool {
	policy := continuousRetryPolicyForCall(policies)
	if !policy.Enabled || isExplicitUpstreamSafetyPolicy(payload) {
		return false
	}
	if eventType == "" && strings.Contains(strings.ToLower(string(payload)), "response.failed") {
		eventType = "response.failed"
	}
	if isPermanentQuotaFailure(responseFailedErrorBody(payload)) && !policy.CatchesAllUpstreamFailures() && !policy.MatchesErrorCodes(payload) {
		return false
	}
	if policy.CatchesAllUpstreamFailures() {
		return true
	}
	if continuousRetryHTTPSelected(policy, outcome.logStatusCode, payload) {
		return true
	}
	if policy.MatchesErrorCodes(payload) {
		return true
	}
	if policy.HasCategory(database.ContinuousRetryCategoryContextError) && isContextRetryError(payload) {
		return true
	}
	if eventType == "response.failed" && policy.HasCategory(database.ContinuousRetryCategoryResponseFailed) {
		return true
	}
	streamReadFailure := outcome.logStatusCode == logStatusUpstreamStreamBreak ||
		outcome.failureKind == "transport" || outcome.failureKind == "timeout"
	return streamReadFailure &&
		(policy.HasCategory(database.ContinuousRetryCategoryTransport) ||
			policy.HasCategory(database.ContinuousRetryCategoryStreamError))
}

// continuousRetryStreamFailureSelected includes the legacy retryable outcome
// classification and the opt-in selector. The former preserves existing
// finite WebSocket behavior; the latter allows a policy to deliberately opt
// into account-scoped 4xx/context/error-frame failures whose legacy outcome is
// not penalized.
func continuousRetryStreamFailureSelected(outcome streamOutcome, payload []byte, eventType string, policies ...database.ContinuousRetryPolicy) bool {
	return outcome.penalize || continuousRetryStreamSelected(outcome, payload, eventType, policies...)
}

func isContextRetryError(payload []byte) bool {
	value := strings.ToLower(string(payload))
	for _, marker := range []string{
		"context_length_exceeded",
		"context_window_exceeded",
		"context_window",
		"string_above_max_length",
		"input_too_long",
	} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

// isExplicitUpstreamSafetyPolicy keeps deterministic provider safety refusals
// outside continuous retry even when an operator selected a broad 4xx/5xx
// category or the exact HTTP status. Match structured machine codes only so a
// harmless mention of "content policy" in an error message cannot disable a
// genuinely recoverable retry.
func isExplicitUpstreamSafetyPolicy(payload []byte) bool {
	if isExplicitUpstreamCyberPolicy(payload) {
		return true
	}
	body := responseFailedErrorBody(payload)
	for _, path := range []string{
		"error.code",
		"error.type",
		"response.error.code",
		"response.error.type",
		"response.status_details.error.code",
		"response.status_details.error.type",
		"code",
		"type",
	} {
		marker := strings.ToLower(strings.TrimSpace(firstGJSONString(body, path)))
		switch marker {
		case "content_policy",
			"content_policy_violation",
			"content_filter",
			"policy_violation",
			"responsible_ai_policy_violation",
			"responsibleaipolicyviolation",
			"safety_policy",
			"safety_policy_violation",
			"safety_violation",
			"image_safety_violation",
			"safety_blocked",
			"blocked_by_safety",
			"moderation_blocked",
			"blocked_by_moderation":
			return true
		}
	}
	return false
}

func continuousRetryLimitsForStream(outcome streamOutcome, payload []byte, eventType string, generalLimit, rateLimit int, policies ...database.ContinuousRetryPolicy) (int, int) {
	if !continuousRetryStreamSelected(outcome, payload, eventType, policies...) {
		return generalLimit, rateLimit
	}
	if streamOutcomeUsesRateLimitBudget(outcome) {
		return generalLimit, -1
	}
	return -1, rateLimit
}
