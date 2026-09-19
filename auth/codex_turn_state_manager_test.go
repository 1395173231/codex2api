package auth

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func fakeFernetTicket(issued time.Time, cipherBytes int, marker byte) string {
	raw := make([]byte, 57+cipherBytes)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(issued.Unix()))
	raw[9] = marker
	return base64.URLEncoding.EncodeToString(raw)
}

func turnStateManagerFixture(t *testing.T, probe CodexTurnStateProbe) (*CodexTurnStateManager, *Account) {
	t.Helper()
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "tickets.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	id, err := db.InsertAccountWithCredentials(ctx, "tickets", map[string]any{"access_token": "test-account-token", "account_id": "workspace-1", "plan_type": "plus"}, "")
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	if err := store.LoadAccountByID(ctx, id); err != nil {
		t.Fatal(err)
	}
	m, err := NewCodexTurnStateManager(ctx, db, store, probe)
	if err != nil {
		t.Fatal(err)
	}
	cfg := m.Config()
	cfg.Enabled = true
	cfg.Models = []string{"model-a", "model-b"}
	cfg.HarvestProxyURL = "http://private-user:private-pass@127.0.0.1:9"
	if err := m.SaveConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	return m, store.FindByID(id)
}

func TestCodexTurnStateFernetTimestampAndConfiguredLengths(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, tc := range []struct {
		size, length int
	}{{160, 292}, {192, 332}} {
		token := fakeFernetTicket(now.Add(-30*time.Minute), tc.size, 1)
		if len(token) != tc.length {
			t.Fatal("invalid fixture length")
		}
		issued, expires, err := validateCodexTicket(token, []int{292, 332}, time.Hour, now)
		if err != nil || !issued.Equal(now.Add(-30*time.Minute)) || !expires.Equal(now.Add(30*time.Minute)) {
			t.Fatalf("length %d: wrong validity %v", tc.length, err)
		}
	}
	for _, token := range []string{"gAAAAAbogus", fakeFernetTicket(now.Add(-2*time.Hour), 160, 1), fakeFernetTicket(now.Add(2*time.Minute), 160, 1), fakeFernetTicket(now, 176, 1)} {
		if _, _, err := validateCodexTicket(token, []int{292, 332}, time.Hour, now); err == nil {
			t.Fatal("invalid, expired, future or unconfigured-length ticket accepted")
		}
	}
}

func TestCodexTurnStateSettingsMigratesLegacyLengths(t *testing.T) {
	cfg, err := DecodeCodexTurnStateSettings(`{"enabled":false,"target_length":292,"team_target_length":332}`)
	if err != nil || !slices.Equal(cfg.TargetLengths, []int{292, 332}) {
		t.Fatalf("legacy lengths = %v err=%v", cfg.TargetLengths, err)
	}
	cfg, err = DecodeCodexTurnStateSettings(`{"enabled":false,"target_lengths":[332,292,332]}`)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = NormalizeCodexTurnStateSettings(cfg)
	if err != nil || !slices.Equal(cfg.TargetLengths, []int{332, 292}) {
		t.Fatalf("normalized lengths = %v err=%v", cfg.TargetLengths, err)
	}
}

func TestCodexTurnStateSchedulingGateRequiresValidModelTicket(t *testing.T) {
	m, a := turnStateManagerFixture(t, nil)
	missing := &Account{DBID: a.ID() + 1, AccessToken: "missing-ticket", AccountID: "workspace-2", PlanType: "plus", codexTurnStateManager: m}
	filter := m.store.WithModelCooldownFilter("model-a", nil)
	if !filter(a) || !filter(missing) {
		t.Fatal("scheduling gate affected accounts while disabled")
	}

	cfg := m.Config()
	cfg.SchedulingEnabled = true
	if err := m.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if filter(a) || filter(missing) {
		t.Fatal("account without a valid model ticket passed scheduling")
	}
	if !m.store.WithModelCooldownFilter("unconfigured-model", nil)(missing) {
		t.Fatal("unconfigured model was gated")
	}
	relay := &Account{DBID: missing.ID() + 1, UpstreamType: UpstreamOpenAIResponses, APIKey: "relay-key", BaseURL: "https://relay.invalid", codexTurnStateManager: m}
	if !filter(relay) {
		t.Fatal("relay account was incorrectly gated by harvested tickets")
	}

	ticket := fakeFernetTicket(time.Now(), 160, 3)
	if err := m.Replace(context.Background(), a, "model-a", ticket); err != nil {
		t.Fatal(err)
	}
	if !filter(a) || filter(missing) {
		t.Fatal("scheduler did not isolate the valid-ticket account")
	}
	// A configured normal response length keeps the current ticket valid.
	m.Observe(a, "model-a", ticket, fakeFernetTicket(time.Now(), 192, 4))
	if !filter(a) {
		t.Fatal("configured response length revoked the ticket")
	}
	// Any returned state outside target_lengths revokes immediately, before the
	// asynchronous database update completes.
	m.Observe(a, "model-a", ticket, fakeFernetTicket(time.Now(), 176, 5))
	if filter(a) || m.Injection(a, "model-a") != "" {
		t.Fatal("abnormal returned state remained schedulable or injectable")
	}
	m.invalidateObservation(context.Background(), turnStateObservation{a, "model-a", ticket, fakeFernetTicket(time.Now(), 176, 5)})
	status := m.Statuses(a.ID(), a)[0]
	if status.Ready || status.TokenLength != 0 || !strings.Contains(status.LastError, "length 312") {
		t.Fatalf("invalidated status = %+v", status)
	}
}

func TestCodexTurnStateHarvestPausesForRateLimitsAndMissingCredits(t *testing.T) {
	m, a := turnStateManagerFixture(t, nil)
	now := time.Now()
	ticket := fakeFernetTicket(now.Add(-time.Minute), 160, 1)
	if err := m.Replace(context.Background(), a, "model-a", ticket); err != nil {
		t.Fatal(err)
	}

	a.mu.Lock()
	a.Status = StatusCooldown
	a.CooldownReason = "rate_limited"
	a.CooldownUtil = now.Add(time.Minute)
	a.mu.Unlock()
	status := m.Statuses(a.ID(), a)[0]
	if status.Status != "paused" || status.PauseReason != codexTurnStatePauseRateLimited || !status.Ready || m.Injection(a, "model-a") != ticket {
		t.Fatalf("active cooldown status = %+v", status)
	}
	if err := m.RequestRefresh(context.Background(), a, "model-a"); err == nil {
		t.Fatal("manual refresh bypassed active cooldown")
	}

	a.mu.Lock()
	a.Status = StatusReady
	a.CooldownReason = ""
	a.CooldownUtil = time.Time{}
	a.PlanType = "free"
	a.UsagePercent7dValid = true
	a.UsagePercent7d = 100
	a.Reset7dAt = now.Add(time.Hour)
	a.CreditEnabled = false
	a.CreditSkipUsageWindow = false
	a.CreditsValid = false
	a.mu.Unlock()
	status = m.Statuses(a.ID(), a)[0]
	if status.Status != "paused" || status.PauseReason != codexTurnStatePauseCreditsUnavailable || !status.Ready {
		t.Fatalf("exhausted usage status = %+v", status)
	}

	// Explicitly authorized, available credits make a fresh probe eligible.
	a.mu.Lock()
	a.CreditEnabled = true
	a.CreditSkipUsageWindow = true
	a.CreditsValid = true
	a.CreditsHasCredits = true
	a.CreditsBalanceKnown = true
	a.CreditsBalance = "5"
	a.mu.Unlock()
	status = m.Statuses(a.ID(), a)[0]
	reason := codexTicketHarvestPauseReason(a, "model-a", now)
	if status.Status == "paused" || reason != "" {
		t.Fatalf("available authorized credits did not resume collection: status=%+v reason=%q", status, reason)
	}

	a.SetModelCooldownUntil("model-a", "rate_limited", now.Add(time.Minute))
	status = m.Statuses(a.ID(), a)[0]
	if status.Status != "paused" || status.PauseReason != codexTurnStatePauseRateLimited {
		t.Fatalf("model cooldown status = %+v", status)
	}
}

func TestCodexTurnStateSchedulerDoesNotProbeUntilRateLimitRecovers(t *testing.T) {
	var calls atomic.Int32
	ticket := fakeFernetTicket(time.Now(), 160, 7)
	m, a := turnStateManagerFixture(t, func(context.Context, *Account, string, string) (string, error) {
		calls.Add(1)
		return ticket, nil
	})
	cfg := m.Config()
	cfg.Models = []string{"model-a"}
	if err := m.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	a.SetCooldownUntil(time.Now().Add(time.Minute), "rate_limited")

	var wg sync.WaitGroup
	m.schedule(context.Background(), &wg)
	wg.Wait()
	if calls.Load() != 0 {
		t.Fatal("rate-limited account sent a collection request")
	}

	a.mu.Lock()
	a.Status = StatusReady
	a.CooldownReason = ""
	a.CooldownUtil = time.Time{}
	a.mu.Unlock()
	m.schedule(context.Background(), &wg)
	wg.Wait()
	if calls.Load() != 1 || m.Injection(a, "model-a") != ticket {
		t.Fatal("collection did not resume after rate-limit recovery")
	}
}

func TestCodexTurnStateIsolationRestartAndExpiry(t *testing.T) {
	m, a := turnStateManagerFixture(t, nil)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	first := fakeFernetTicket(now.Add(-time.Minute), 160, 1)
	second := fakeFernetTicket(now.Add(-2*time.Minute), 160, 2)
	if err := m.Replace(ctx, a, "model-a", first); err != nil {
		t.Fatal(err)
	}
	if err := m.Replace(ctx, a, "model-b", second); err != nil {
		t.Fatal(err)
	}
	if err := m.Replace(ctx, a, "model-b", first); err == nil {
		t.Fatal("same ticket accepted for two models")
	}
	if a.CodexTurnStateInjection("model-b", "model-a") != first || a.CodexTurnStateInjection("model-a", "model-b") != second || a.CodexTurnStateInjection("model-a", "unknown") != "" {
		t.Fatal("tickets crossed model boundaries")
	}
	other := &Account{DBID: a.ID() + 1, AccessToken: "other", PlanType: "plus"}
	if m.Injection(other, "model-a") != "" {
		t.Fatal("ticket crossed account boundary")
	}
	restarted, err := NewCodexTurnStateManager(ctx, m.db, m.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Injection(a, "model-a") != first || restarted.Injection(a, "model-b") != second {
		t.Fatal("restart lost model tickets")
	}
	cfg := restarted.Config()
	cfg.TTLSeconds = 180
	cfg.RefreshBeforeSeconds = 60
	if err := restarted.SaveConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.AccountID = "replacement-workspace"
	a.mu.Unlock()
	if restarted.Injection(a, "model-a") != "" {
		t.Fatal("ticket survived account identity replacement")
	}
}

func TestCodexTurnStateFailedProbePreservesValidTicket(t *testing.T) {
	m, a := turnStateManagerFixture(t, func(context.Context, *Account, string, string) (string, error) {
		return "", errors.New("proxy secret must not leak")
	})
	ctx := context.Background()
	ticket := fakeFernetTicket(time.Now().Add(-time.Minute), 160, 1)
	if err := m.Replace(ctx, a, "model-a", ticket); err != nil {
		t.Fatal(err)
	}
	if err := m.RequestRefresh(ctx, a, "model-a"); err != nil {
		t.Fatal(err)
	}
	if err := m.probeOnce(ctx, a, "model-a", m.Config(), m.generation); err != nil {
		t.Fatal(err)
	}
	if m.Injection(a, "model-a") != ticket {
		t.Fatal("failed harvest erased valid ticket")
	}
	status := m.Statuses(a.ID(), a)[0]
	if status.Attempts != 1 || status.LastError != "harvest request failed" || status.NextAttemptAt == "" {
		t.Fatalf("bad failure status: %+v", status)
	}
}

func TestCodexTurnStateInFlightProbeCannotOverwriteManualTicket(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	now := time.Now()
	old := fakeFernetTicket(now, 160, 1)
	fresh := fakeFernetTicket(now, 160, 2)
	m, a := turnStateManagerFixture(t, func(context.Context, *Account, string, string) (string, error) {
		close(started)
		<-finish
		return old, nil
	})
	done := make(chan error, 1)
	go func() { done <- m.probeOnce(context.Background(), a, "model-a", m.Config(), m.generation) }()
	<-started
	if err := m.Replace(context.Background(), a, "model-a", fresh); err != nil {
		t.Fatal(err)
	}
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if m.Injection(a, "model-a") != fresh {
		t.Fatal("old harvest overwrote manual ticket")
	}
}

func TestCodexTurnStateRefreshReadsLatestDatabaseTicket(t *testing.T) {
	m, a := turnStateManagerFixture(t, nil)
	ticket := fakeFernetTicket(time.Now(), 160, 9)
	other, err := NewCodexTurnStateManager(context.Background(), m.db, m.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Replace(context.Background(), a, "model-a", ticket); err != nil {
		t.Fatal(err)
	}
	if err := m.RequestRefresh(context.Background(), a, "model-a"); err != nil {
		t.Fatal(err)
	}
	if m.Injection(a, "model-a") != ticket {
		t.Fatal("manual refresh overwrote another instance's newer ticket")
	}
}

func TestCodexTurnStateHotConfigAndCancellation(t *testing.T) {
	started := make(chan struct{}, 2)
	var active atomic.Int32
	var peak atomic.Int32
	m, _ := turnStateManagerFixture(t, func(ctx context.Context, _ *Account, _, _ string) (string, error) {
		n := active.Add(1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		defer active.Add(-1)
		started <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	})
	cfg := m.Config()
	cfg.Concurrency = 1
	if err := m.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("harvester did not start")
	}
	cfg.Enabled = false
	if err := m.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel probes")
	}
	if active.Load() != 0 || peak.Load() > 1 {
		t.Fatal("worker limit or cancellation failed")
	}
	rows, err := m.db.ListCodexTurnStates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range rows {
		if rec.Lease != "" {
			t.Fatal("cancellation leaked a lease")
		}
	}
}

func TestCodexTurnStateHotTTLReschedulesAnd312OnlyInvalidatesUsedTicket(t *testing.T) {
	m, a := turnStateManagerFixture(t, nil)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	ticket := fakeFernetTicket(now.Add(-10*time.Minute), 160, 1)
	if err := m.Replace(ctx, a, "model-a", ticket); err != nil {
		t.Fatal(err)
	}
	rec := m.records[turnStateKey{a.ID(), "model-a"}]
	rec.NextAttemptAt = now.Add(40 * time.Minute)
	cfg := m.Config()
	cfg.TTLSeconds = 180
	if !turnStateDue(rec, a, cfg, now) {
		t.Fatal("new TTL kept old successful refresh deadline")
	}
	fresh := fakeFernetTicket(now, 160, 2)
	if err := m.Replace(ctx, a, "model-a", fresh); err != nil {
		t.Fatal(err)
	}
	m.invalidateObservation(ctx, turnStateObservation{a, "model-a", ticket, fakeFernetTicket(now, 176, 4)})
	if m.Injection(a, "model-a") != fresh {
		t.Fatal("stale 312 observation revoked replacement")
	}
	// Revocation must work while another harvest owns the model lease.
	_, claimed, err := m.db.ClaimCodexTurnState(ctx, a.ID(), "model-a", now, time.Minute)
	if err != nil || !claimed {
		t.Fatal("failed to claim refresh lease")
	}
	m.Observe(a, "model-a", fresh, fakeFernetTicket(now, 176, 4))
	if m.Injection(a, "model-a") != "" {
		t.Fatal("queued revocation still allowed injection")
	}
	m.invalidateObservation(ctx, turnStateObservation{a, "model-a", fresh, fakeFernetTicket(now, 176, 4)})
	if m.Injection(a, "model-a") != "" {
		t.Fatal("312 observation left used ticket active")
	}
}

func TestCodexTurnStateSimultaneousModels(t *testing.T) {
	m, a := turnStateManagerFixture(t, func(_ context.Context, _ *Account, model, _ string) (string, error) {
		marker := byte(1)
		if model == "model-b" {
			marker = 2
		}
		return fakeFernetTicket(time.Now(), 160, marker), nil
	})
	var wg sync.WaitGroup
	for _, model := range m.Config().Models {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.probeOnce(context.Background(), a, model, m.Config(), m.generation); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if m.Injection(a, "model-a") == "" || m.Injection(a, "model-a") == m.Injection(a, "model-b") {
		t.Fatal("parallel model tickets were not independent")
	}
}
