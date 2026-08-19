package auth

import (
	"sync/atomic"
	"testing"
	"time"
)

func newSessionSlotBufferTestStore(limit int64, accounts ...*Account) *Store {
	store := &Store{
		accounts:                accounts,
		maxConcurrency:          limit,
		sessionBindings:         make(map[string]sessionAffinity),
		sessionSlotReservations: make(map[int64]map[string][]uint64),
	}
	store.SetSessionSlotBuffer(50 * time.Millisecond)
	store.SetSessionSlotBufferEnabled(true)
	return store
}

func TestSessionSlotBufferDisabledByDefault(t *testing.T) {
	account := &Account{DBID: 1, AccessToken: "tok-1"}
	store := &Store{
		accounts:                []*Account{account},
		maxConcurrency:          1,
		sessionBindings:         make(map[string]sessionAffinity),
		sessionSlotReservations: make(map[int64]map[string][]uint64),
	}
	store.SetSessionSlotBuffer(50 * time.Millisecond)

	acquired := store.Next()
	store.ReleaseForSession(acquired, "owner")
	if got := accountOccupiedRequests(account); got != 0 {
		t.Fatalf("occupied while disabled = %d, want 0", got)
	}
}

func TestDisablingSessionSlotBufferReleasesReservations(t *testing.T) {
	account := &Account{DBID: 1, AccessToken: "tok-1"}
	store := newSessionSlotBufferTestStore(1, account)
	acquired := store.Next()
	store.ReleaseForSession(acquired, "owner")

	store.SetSessionSlotBufferEnabled(false)
	if got := accountOccupiedRequests(account); got != 0 {
		t.Fatalf("occupied after disabling = %d, want 0", got)
	}
	if next := store.Next(); next != account {
		t.Fatal("capacity was not immediately available after disabling")
	} else {
		store.Release(next)
	}
}

func TestSessionSlotBufferOwnerReclaimsBeforeFreshSession(t *testing.T) {
	primary := &Account{DBID: 1, AccessToken: "tok-1"}
	fallback := &Account{DBID: 2, AccessToken: "tok-2"}
	store := newSessionSlotBufferTestStore(1, primary, fallback)
	store.BindSessionAffinity("owner", primary, "")

	first, _ := store.NextForSession("owner", 0, nil)
	if first != primary {
		t.Fatalf("owner first account = %p, want primary %p", first, primary)
	}
	store.ReleaseForSession(first, "owner")
	if got := primary.GetActiveRequests(); got != 0 {
		t.Fatalf("active after buffering = %d, want 0", got)
	}
	if got := accountOccupiedRequests(primary); got != 1 {
		t.Fatalf("occupied after buffering = %d, want 1", got)
	}

	fresh, _ := store.NextForSession("fresh", 0, nil)
	if fresh != fallback {
		t.Fatalf("fresh session account = %p, want fallback %p", fresh, fallback)
	}
	store.Release(fresh)

	reclaimed, _ := store.NextForSession("owner", 0, nil)
	if reclaimed != primary {
		t.Fatalf("reclaimed account = %p, want primary %p", reclaimed, primary)
	}
	if got := primary.GetActiveRequests(); got != 1 {
		t.Fatalf("active after reclaim = %d, want 1", got)
	}
	if got := accountOccupiedRequests(primary); got != 1 {
		t.Fatalf("occupied after reclaim = %d, want 1", got)
	}
	store.Release(reclaimed)
}

func TestSessionSlotBufferExpiresAndReleasesCapacity(t *testing.T) {
	account := &Account{DBID: 1, AccessToken: "tok-1"}
	store := newSessionSlotBufferTestStore(1, account)
	store.BindSessionAffinity("owner", account, "")

	acquired, _ := store.NextForSession("owner", 0, nil)
	store.ReleaseForSession(acquired, "owner")
	if fresh, _ := store.NextForSession("fresh", 0, nil); fresh != nil {
		store.Release(fresh)
		t.Fatal("fresh session acquired a slot before buffer expiry")
	}

	deadline := time.Now().Add(time.Second)
	for accountOccupiedRequests(account) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := accountOccupiedRequests(account); got != 0 {
		t.Fatalf("occupied after expiry = %d, want 0", got)
	}
	fresh, _ := store.NextForSession("fresh", 0, nil)
	if fresh != account {
		t.Fatalf("fresh session after expiry = %p, want account %p", fresh, account)
	}
	store.Release(fresh)
}

func TestImmediateReleaseDoesNotCreateSessionReservation(t *testing.T) {
	account := &Account{DBID: 1, AccessToken: "tok-1"}
	store := newSessionSlotBufferTestStore(1, account)
	acquired := store.Next()
	if acquired != account {
		t.Fatal("failed to acquire test account")
	}
	store.Release(acquired)
	if got := atomic.LoadInt64(&account.ActiveRequests); got != 0 {
		t.Fatalf("active after immediate release = %d, want 0", got)
	}
	if got := accountOccupiedRequests(account); got != 0 {
		t.Fatalf("occupied after immediate release = %d, want 0", got)
	}
}
