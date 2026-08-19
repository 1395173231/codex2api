package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestNormalizeRetryLimit(t *testing.T) {
	cases := []struct {
		name string
		got  int
		want int
	}{
		{name: "negative", got: -2, want: 0},
		{name: "former sentinel stays finite", got: -1, want: 0},
		{name: "disabled", got: 0, want: 0},
		{name: "finite lower bound", got: 1, want: 1},
		{name: "finite upper bound", got: 10, want: 10},
		{name: "above finite upper bound", got: 11, want: 10},
		{name: "large positive", got: 1000000, want: 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeRetryLimit(tc.got); got != tc.want {
				t.Fatalf("NormalizeRetryLimit(%d) = %d, want %d", tc.got, got, tc.want)
			}
		})
	}
}

func TestSQLiteRetryLimitsRoundTripAndNormalize(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "retry-limits.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	settings := &SystemSettings{
		MaxConcurrency:          1,
		TestConcurrency:         1,
		TestModel:               "gpt-5.4",
		MaxRetries:              -1,
		MaxRateLimitRetries:     7,
		CodexWSSilentMaxRetries: -1,
	}
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateSystemSettings(normalized): %v", err)
	}
	got, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings(normalized): %v", err)
	}
	if got.MaxRetries != 0 || got.MaxRateLimitRetries != 7 || got.CodexWSSilentMaxRetries != 0 {
		t.Fatalf("retry limits round trip = (%d, %d, %d), want (0, 7, 0)", got.MaxRetries, got.MaxRateLimitRetries, got.CodexWSSilentMaxRetries)
	}

	got.MaxRetries = -99
	got.MaxRateLimitRetries = 99
	got.CodexWSSilentMaxRetries = -99
	if err := db.UpdateSystemSettings(ctx, got); err != nil {
		t.Fatalf("UpdateSystemSettings(normalize): %v", err)
	}
	normalized, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings(normalize): %v", err)
	}
	if normalized.MaxRetries != 0 || normalized.MaxRateLimitRetries != 10 || normalized.CodexWSSilentMaxRetries != 0 {
		t.Fatalf("normalized retry limits = (%d, %d, %d), want (0, 10, 0)", normalized.MaxRetries, normalized.MaxRateLimitRetries, normalized.CodexWSSilentMaxRetries)
	}
}
