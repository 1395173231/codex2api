package database

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestSubscriptionUpgradeOperationRejectsDuplicateIdempotencyKey(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	first := SubscriptionUpgradeOperation{
		ID:                 "operation-1",
		AccountID:          20,
		IdempotencyKeyHash: "sha256:first-key",
		SourcePlan:         "prolite",
		TargetPlan:         "chatgptpro",
		Currency:           "PHP",
		AmountDueMinor:     345196,
		Status:             "submitting",
	}
	if err := db.CreateSubscriptionUpgradeOperation(ctx, first); err != nil {
		t.Fatalf("Create first: %v", err)
	}
	duplicate := first
	duplicate.ID = "operation-2"
	if err := db.CreateSubscriptionUpgradeOperation(ctx, duplicate); !errors.Is(err, ErrSubscriptionUpgradeIdempotencyConflict) {
		t.Fatalf("Create duplicate error = %v, want idempotency conflict", err)
	}
	got, err := db.GetSubscriptionUpgradeOperationByIdempotencyHash(ctx, 20, first.IdempotencyKeyHash)
	if err != nil {
		t.Fatalf("Get by idempotency hash: %v", err)
	}
	if got.ID != first.ID || got.AmountDueMinor != 345196 {
		t.Fatalf("operation = %#v, want original operation", got)
	}
}
