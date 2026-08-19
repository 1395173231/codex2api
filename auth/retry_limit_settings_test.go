package auth

import (
	"testing"

	"github.com/codex2api/database"
)

func TestStoreRetryLimitDefaults(t *testing.T) {
	store := NewStore(nil, nil, nil)
	defer store.Stop()

	if got := store.GetMaxRetries(); got != 2 {
		t.Fatalf("default GetMaxRetries() = %d, want 2", got)
	}
	if got := store.GetMaxRateLimitRetries(); got != 1 {
		t.Fatalf("default GetMaxRateLimitRetries() = %d, want 1", got)
	}
	if got := store.CodexWSSilentMaxRetries(); got != 2 {
		t.Fatalf("default CodexWSSilentMaxRetries() = %d, want 2", got)
	}
}

func TestStoreRetryLimitSettingsLoadAndNormalize(t *testing.T) {
	cases := []struct {
		name     string
		max      int
		rate     int
		ws       int
		wantMax  int
		wantRate int
		wantWS   int
	}{
		{name: "negative values stay finite", max: -1, rate: -1, ws: -1, wantMax: 0, wantRate: 0, wantWS: 0},
		{name: "disabled", max: 0, rate: 0, ws: 0, wantMax: 0, wantRate: 0, wantWS: 0},
		{name: "finite", max: 4, rate: 2, ws: 3, wantMax: 4, wantRate: 2, wantWS: 3},
		{name: "below sentinel", max: -2, rate: -2, ws: -2, wantMax: 0, wantRate: 0, wantWS: 0},
		{name: "above finite bound", max: 99, rate: 99, ws: 99, wantMax: 10, wantRate: 10, wantWS: 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore(nil, nil, &database.SystemSettings{
				MaxConcurrency:          1,
				TestConcurrency:         1,
				MaxRetries:              tc.max,
				MaxRateLimitRetries:     tc.rate,
				CodexWSSilentMaxRetries: tc.ws,
			})
			defer store.Stop()

			if got := store.GetMaxRetries(); got != tc.wantMax {
				t.Fatalf("GetMaxRetries() = %d, want %d", got, tc.wantMax)
			}
			if got := store.GetMaxRateLimitRetries(); got != tc.wantRate {
				t.Fatalf("GetMaxRateLimitRetries() = %d, want %d", got, tc.wantRate)
			}
			if got := store.CodexWSSilentMaxRetries(); got != tc.wantWS {
				t.Fatalf("CodexWSSilentMaxRetries() = %d, want %d", got, tc.wantWS)
			}
		})
	}
}

func TestStoreRetryLimitSettersNormalizeWithoutChangingOtherBudgets(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:          1,
		TestConcurrency:         1,
		MaxRetries:              2,
		MaxRateLimitRetries:     1,
		CodexWSSilentMaxRetries: 2,
	})
	defer store.Stop()

	store.SetMaxRetries(-1)
	store.SetMaxRateLimitRetries(0)
	store.SetCodexWSSilentMaxRetries(99)
	if got := store.GetMaxRetries(); got != 0 {
		t.Fatalf("SetMaxRetries(-1) = %d, want 0", got)
	}
	if got := store.GetMaxRateLimitRetries(); got != 0 {
		t.Fatalf("SetMaxRateLimitRetries(0) = %d, want 0", got)
	}
	if got := store.CodexWSSilentMaxRetries(); got != 10 {
		t.Fatalf("SetCodexWSSilentMaxRetries(99) = %d, want 10", got)
	}

	store.SetMaxRetries(-2)
	store.SetMaxRateLimitRetries(-2)
	store.SetCodexWSSilentMaxRetries(-2)
	if got := store.GetMaxRetries(); got != 0 {
		t.Fatalf("SetMaxRetries(-2) = %d, want 0", got)
	}
	if got := store.GetMaxRateLimitRetries(); got != 0 {
		t.Fatalf("SetMaxRateLimitRetries(-2) = %d, want 0", got)
	}
	if got := store.CodexWSSilentMaxRetries(); got != 0 {
		t.Fatalf("SetCodexWSSilentMaxRetries(-2) = %d, want 0", got)
	}
}
