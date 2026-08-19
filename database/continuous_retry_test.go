package database

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestContinuousRetryPolicyDefaultsAndNormalization(t *testing.T) {
	defaultPolicy := DefaultContinuousRetryPolicy()
	if defaultPolicy.Enabled {
		t.Fatal("continuous retry must be disabled by default")
	}
	if len(defaultPolicy.Categories) == 0 {
		t.Fatal("default policy should preserve the common transient categories")
	}
	enabledDefault := defaultPolicy
	enabledDefault.Enabled = true
	if enabledDefault.HasCategory(ContinuousRetryCategoryResponseFailed) {
		t.Fatal("default policy must not select every response.failed event")
	}

	got := NormalizeContinuousRetryPolicy(ContinuousRetryPolicy{
		Enabled:     true,
		Categories:  []string{"5xx", "HTTP_429", "5xx", "unknown", "context_length"},
		StatusCodes: []int{504, 404, 504, 99, 600},
		ErrorCodes:  []string{" Context_Length_Exceeded ", "rate_limited", "rate_limited", "bad code!"},
	})
	if strings.Join(got.Categories, ",") != "context_error,http_429,http_5xx" {
		t.Fatalf("normalized categories = %v", got.Categories)
	}
	if strings.Join(intsToStrings(got.StatusCodes), ",") != "404,504" {
		t.Fatalf("normalized status codes = %v", got.StatusCodes)
	}
	if strings.Join(got.ErrorCodes, ",") != "context_length_exceeded,rate_limited" {
		t.Fatalf("normalized error codes = %v", got.ErrorCodes)
	}
}

func TestContinuousRetryPolicyMatchesStatusAndErrorCode(t *testing.T) {
	policy := ContinuousRetryPolicy{
		Enabled:     true,
		Categories:  []string{ContinuousRetryCategoryHTTP429, ContinuousRetryCategoryHTTP5xx},
		StatusCodes: []int{403, 404, 501},
		ErrorCodes:  []string{"context_length_exceeded"},
	}

	for _, status := range []int{403, 404, 429, 500, 501, 503, 504} {
		if !policy.MatchesHTTP(status, nil) {
			t.Errorf("status %d did not match selected policy", status)
		}
	}
	if policy.MatchesHTTP(400, []byte(`{"error":{"code":"invalid_request"}}`)) {
		t.Error("unselected 400 unexpectedly matched")
	}
	if !policy.MatchesHTTP(400, []byte(`{"error":{"code":"context_length_exceeded"}}`)) {
		t.Error("selected error code did not match HTTP body")
	}
	rateCodeOnly := ContinuousRetryPolicy{Enabled: true, ErrorCodes: []string{"rate_limited"}}
	if rateCodeOnly.MatchesErrorCodes([]byte(`{"error":{"code":"rate_limited_model"}}`)) {
		t.Error("an exact error-code selector matched a longer code")
	}

	plain := ContinuousRetryPolicy{Enabled: true, Categories: []string{ContinuousRetryCategoryTransport}}
	if !plain.MatchesTransport("context deadline exceeded") {
		t.Error("transport category did not match a transport error")
	}
	if plain.MatchesHTTP(404, nil) {
		t.Error("transport category unexpectedly matched an HTTP status")
	}
	fourXX := ContinuousRetryPolicy{Enabled: true, Categories: []string{ContinuousRetryCategoryHTTP4xx}}
	if !fourXX.MatchesHTTP(429, nil) {
		t.Error("http_4xx category should include 429")
	}
}

func TestContinuousRetryPolicyEncodeParseRoundTrip(t *testing.T) {
	want := ContinuousRetryPolicy{
		Enabled:     true,
		Categories:  []string{ContinuousRetryCategoryHTTP4xx},
		StatusCodes: []int{403, 404},
		ErrorCodes:  []string{"forbidden"},
	}
	raw := EncodeContinuousRetryPolicy(want)
	got := ParseContinuousRetryPolicy(raw)
	if got.Enabled != want.Enabled || strings.Join(got.Categories, ",") != strings.Join(want.Categories, ",") {
		t.Fatalf("round-trip policy = %#v, raw=%s", got, raw)
	}
	if strings.Join(intsToStrings(got.StatusCodes), ",") != "403,404" || strings.Join(got.ErrorCodes, ",") != "forbidden" {
		t.Fatalf("round-trip selectors = %#v", got)
	}
}

func TestSQLiteContinuousRetryPolicyPersistsIndependently(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "continuous-retry.sqlite"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	want := ContinuousRetryPolicy{
		Enabled:     true,
		Categories:  []string{ContinuousRetryCategoryHTTP4xx},
		StatusCodes: []int{403, 404},
		ErrorCodes:  []string{"forbidden"},
	}
	if err := db.UpdateContinuousRetryPolicy(ctx, want); err != nil {
		t.Fatalf("UpdateContinuousRetryPolicy: %v", err)
	}
	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	if got := ParseContinuousRetryPolicy(settings.ContinuousRetryPolicy); !got.Enabled || len(got.StatusCodes) != 2 || got.StatusCodes[0] != 403 || len(got.Categories) != 1 || got.Categories[0] != ContinuousRetryCategoryHTTP4xx {
		t.Fatalf("persisted policy = %#v", got)
	}

	// A legacy full settings write must not clear the narrow policy column.
	settings.SiteName = "preserve policy"
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings after full write: %v", err)
	}
	if got := ParseContinuousRetryPolicy(settings.ContinuousRetryPolicy); !got.Enabled || len(got.StatusCodes) != 2 {
		t.Fatalf("full settings write cleared policy = %#v", got)
	}
}

func intsToStrings(values []int) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = strconv.Itoa(value)
	}
	return result
}
