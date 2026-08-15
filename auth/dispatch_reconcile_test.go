package auth

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/codex2api/database"
)

func TestReconcileDispatchStateLoadsAccountAddedAfterStartup(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "dispatch-reconcile.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := NewStore(db, nil, &database.SystemSettings{
		MaxConcurrency:       1,
		FastSchedulerEnabled: true,
	})
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Store.Init: %v", err)
	}
	if got := store.Next(); got != nil {
		store.Release(got)
		t.Fatalf("Next() before insert = account %d, want nil", got.ID())
	}

	accountID, err := db.InsertOpenAIResponsesAccount(ctx, "new-endpoint", map[string]interface{}{
		"upstream_type": UpstreamOpenAIResponses,
		"base_url":      "https://healthy.example",
		"api_key":       "sk-new",
		"models":        []string{"gpt-5.6"},
	}, "")
	if err != nil {
		t.Fatalf("InsertOpenAIResponsesAccount: %v", err)
	}

	changed, err := store.ReconcileDispatchState(ctx)
	if err != nil {
		t.Fatalf("ReconcileDispatchState: %v", err)
	}
	if !changed {
		t.Fatal("ReconcileDispatchState reported no change for a newly added account")
	}
	got := store.Next()
	if got == nil {
		t.Fatal("Next() returned nil after dispatch reconciliation")
	}
	defer store.Release(got)
	if got.ID() != accountID {
		t.Fatalf("Next() = account %d, want newly added account %d", got.ID(), accountID)
	}
}
