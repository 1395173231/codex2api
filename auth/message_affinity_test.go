package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

func newMessageAffinityTestStore(t *testing.T, tokenCache cache.TokenCache, accounts ...*Account) *Store {
	t.Helper()
	store := NewStore(nil, tokenCache, &database.SystemSettings{
		MaxConcurrency:  4,
		TestConcurrency: 1,
		TestModel:       "gpt-5.4",
	})
	for _, account := range accounts {
		store.AddAccount(account)
	}
	t.Cleanup(store.Stop)
	return store
}

func TestMessageAffinitySelectsAcrossStoresAndExactBindingWins(t *testing.T) {
	shared := cache.NewMemory(1)
	writerAccounts := []*Account{
		{DBID: 1, AccessToken: "writer-1", PlanType: "plus"},
		{DBID: 2, AccessToken: "writer-2", PlanType: "plus"},
	}
	writer := newMessageAffinityTestStore(t, shared, writerAccounts...)
	hashes := []uint64{101, 202}
	writer.RecordMessageAffinity(7, hashes, writerAccounts[1])

	readerAccounts := []*Account{
		{DBID: 1, AccessToken: "reader-1", PlanType: "plus"},
		{DBID: 2, AccessToken: "reader-2", PlanType: "plus"},
	}
	reader := newMessageAffinityTestStore(t, shared, readerAccounts...)
	selected, _ := reader.NextForSessionWithMessageHashesWithDispatch("fresh-session", 7, hashes, nil, nil, DispatchPolicyStandard)
	if selected != readerAccounts[1] {
		t.Fatalf("selected account = %v, want DBID 2 from shared message affinity", selected)
	}
	reader.Release(selected)

	reader.BindSessionAffinity("exact-session", readerAccounts[0], "")
	selected, _ = reader.NextForSessionWithMessageHashesWithDispatch("exact-session", 7, hashes, nil, nil, DispatchPolicyStandard)
	if selected != readerAccounts[0] {
		t.Fatalf("exact binding selected account = %v, want DBID 1", selected)
	}
	reader.Release(selected)
}

func TestMessageAffinityCapacitySpilloverPreservesExactBinding(t *testing.T) {
	shared := cache.NewMemory(1)
	defer shared.Close()
	bound := &Account{DBID: 1, AccessToken: "bound", PlanType: "plus"}
	ordinary := &Account{DBID: 2, AccessToken: "ordinary", PlanType: "plus"}
	hinted := &Account{DBID: 3, AccessToken: "hinted", PlanType: "plus"}
	store := &Store{
		accounts:       []*Account{bound, ordinary, hinted},
		maxConcurrency: 1,
		tokenCache:     shared,
	}
	store.bindSessionAffinity("capacity-with-message-hint", bound, "")
	hashes := []uint64{111, 222}
	store.RecordMessageAffinity(7, hashes, hinted)

	held := store.TakePreferredAccountWithDispatch(bound.DBID, 7, nil, nil, DispatchPolicyStandard)
	if held != bound {
		t.Fatalf("held account = %p, want bound account %p", held, bound)
	}
	defer store.Release(held)

	selected, proxyURL, guard := store.NextForSessionWithMessageHashesWithDispatchGuard(
		"capacity-with-message-hint", 7, hashes, nil, nil, DispatchPolicyStandard,
	)
	if selected != hinted {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("capacity spillover account = %v, want message-affinity DBID %d", selected, hinted.DBID)
	}
	if !guard.PreservesExisting() {
		store.Release(selected)
		t.Fatal("message-affinity spillover did not preserve the exact binding")
	}

	store.BindSessionAffinityWithGuard("capacity-with-message-hint", selected, proxyURL, guard)
	requireSessionBinding(t, store, "capacity-with-message-hint", bound.DBID)
	store.ReleaseForSessionWithGuard(selected, "capacity-with-message-hint", guard)
	if got := accountOccupiedRequests(hinted); got != 0 {
		t.Fatalf("hinted fallback occupied slots after guarded release = %d, want 0", got)
	}
}

func TestMessageAffinityRequiresEvidenceAndRespectsTopPriorityLayer(t *testing.T) {
	shared := cache.NewMemory(1)
	high := &Account{DBID: 1, AccessToken: "high", PlanType: "plus", SchedulerPriority: 10}
	low := &Account{DBID: 2, AccessToken: "low", PlanType: "plus"}
	store := newMessageAffinityTestStore(t, shared, high, low)

	store.RecordMessageAffinity(7, []uint64{303}, low)
	if selected := store.nextAccountForMessageAffinityWithDispatch("single", 7, []uint64{303}, nil, nil, DispatchPolicyStandard); selected != nil {
		store.Release(selected)
		t.Fatalf("single weak hash selected DBID %d, want no suggestion", selected.DBID)
	}

	store.RecordMessageAffinity(7, []uint64{303, 404}, low)
	if selected := store.nextAccountForMessageAffinityWithDispatch("priority", 7, []uint64{303, 404}, nil, nil, DispatchPolicyStandard); selected != nil {
		store.Release(selected)
		t.Fatalf("lower-priority account selected DBID %d from message hint", selected.DBID)
	}

	selected, _ := store.NextForSessionWithMessageHashesWithDispatch("priority", 7, []uint64{303, 404}, nil, nil, DispatchPolicyStandard)
	if selected != high {
		t.Fatalf("fallback selected account = %v, want highest-priority DBID 1", selected)
	}
	store.Release(selected)
}

func TestMessageAffinitySingleHashRequiresRepeatedSuccessAndHonorsFilter(t *testing.T) {
	shared := cache.NewMemory(1)
	first := &Account{DBID: 1, AccessToken: "first", PlanType: "plus"}
	second := &Account{DBID: 2, AccessToken: "second", PlanType: "plus"}
	store := newMessageAffinityTestStore(t, shared, first, second)
	for i := 0; i < messageAffinitySingleMinCount; i++ {
		store.RecordMessageAffinity(7, []uint64{909}, second)
	}
	selected := store.nextAccountForMessageAffinityWithDispatch("single-strong", 7, []uint64{909}, nil, nil, DispatchPolicyStandard)
	if selected != second {
		t.Fatalf("single strong hash selected account = %v, want DBID 2", selected)
	}
	store.Release(selected)

	selected = store.nextAccountForMessageAffinityWithDispatch("filtered", 7, []uint64{909}, nil, func(account *Account) bool {
		return account.DBID == first.DBID
	}, DispatchPolicyStandard)
	if selected != nil {
		store.Release(selected)
		t.Fatalf("filtered message hint selected DBID %d, want no suggestion", selected.DBID)
	}
}

type failingMessageAffinityCache struct {
	cache.TokenCache
}

func (f failingMessageAffinityCache) GetMessageAffinities(context.Context, string, []uint64) (map[uint64]cache.MessageAffinityBinding, error) {
	return nil, errors.New("message cache unavailable")
}

func (f failingMessageAffinityCache) RecordMessageAffinities(context.Context, string, []uint64, int64, time.Duration) error {
	return errors.New("message cache unavailable")
}

func TestMessageAffinityCacheFailureFallsBack(t *testing.T) {
	base := cache.NewMemory(1)
	account := &Account{DBID: 1, AccessToken: "only", PlanType: "plus"}
	store := newMessageAffinityTestStore(t, failingMessageAffinityCache{TokenCache: base}, account)
	selected, _ := store.NextForSessionWithMessageHashesWithDispatch("cache-error", 7, []uint64{505, 606}, nil, nil, DispatchPolicyStandard)
	if selected != account {
		t.Fatalf("selected account = %v, want ordinary fallback account", selected)
	}
	store.Release(selected)
	store.RecordMessageAffinity(7, []uint64{505, 606}, account)
}

func TestMessageAffinitySkipsAnonymousScope(t *testing.T) {
	shared := cache.NewMemory(1)
	account := &Account{DBID: 1, AccessToken: "only", PlanType: "plus"}
	store := newMessageAffinityTestStore(t, shared, account)
	store.RecordMessageAffinity(0, []uint64{707, 808}, account)
	backend := shared.(cache.MessageAffinityCache)
	bindings, err := backend.GetMessageAffinities(context.Background(), "api-key:0", []uint64{707, 808})
	if err != nil {
		t.Fatalf("GetMessageAffinities: %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("anonymous bindings = %+v, want none", bindings)
	}
}
