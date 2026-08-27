package cache

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestMessageAffinityBindingJSONPreservesInt64AccountID(t *testing.T) {
	want := int64(9007199254740993)
	encoded, err := json.Marshal(MessageAffinityBinding{AccountID: want, Count: 1})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"account_id":"9007199254740993"`) {
		t.Fatalf("encoded binding = %s, want quoted lossless account ID", encoded)
	}
	var decoded MessageAffinityBinding
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.AccountID != want {
		t.Fatalf("decoded account ID = %d, want %d", decoded.AccountID, want)
	}
}

func TestMemoryMessageAffinityReinforcesDeduplicatesAndScopes(t *testing.T) {
	backend := NewMemory(1).(MessageAffinityCache)
	ctx := context.Background()
	if err := backend.RecordMessageAffinities(ctx, "api-key:7", []uint64{11, 11}, 2, time.Hour); err != nil {
		t.Fatalf("RecordMessageAffinities: %v", err)
	}
	bindings, err := backend.GetMessageAffinities(ctx, "api-key:7", []uint64{11})
	if err != nil {
		t.Fatalf("GetMessageAffinities: %v", err)
	}
	if got := bindings[11]; got.AccountID != 2 || got.Count != 1 || got.Stop {
		t.Fatalf("binding = %+v, want account 2 count 1", got)
	}
	if other, err := backend.GetMessageAffinities(ctx, "api-key:8", []uint64{11}); err != nil || len(other) != 0 {
		t.Fatalf("other scope bindings = %+v err=%v, want miss", other, err)
	}
	if err := backend.RecordMessageAffinities(ctx, "api-key:7", []uint64{11}, 2, time.Hour); err != nil {
		t.Fatalf("reinforce: %v", err)
	}
	bindings, _ = backend.GetMessageAffinities(ctx, "api-key:7", []uint64{11})
	if got := bindings[11].Count; got != 2 {
		t.Fatalf("reinforced count = %d, want 2", got)
	}
}

func TestMemoryMessageAffinityStopsAfterRepeatedConflicts(t *testing.T) {
	backend := NewMemory(1).(MessageAffinityCache)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := backend.RecordMessageAffinities(ctx, "api-key:7", []uint64{22}, 1, time.Hour); err != nil {
			t.Fatalf("seed record %d: %v", i, err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := backend.RecordMessageAffinities(ctx, "api-key:7", []uint64{22}, 2, time.Hour); err != nil {
			t.Fatalf("conflict record %d: %v", i, err)
		}
	}
	bindings, err := backend.GetMessageAffinities(ctx, "api-key:7", []uint64{22})
	if err != nil {
		t.Fatalf("GetMessageAffinities: %v", err)
	}
	if got := bindings[22]; !got.Stop || got.Changes != 3 {
		t.Fatalf("binding = %+v, want stopped after 3 conflicts", got)
	}
}
