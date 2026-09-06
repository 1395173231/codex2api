package admin

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

func resetLimitTestUsage(primary, secondary float64) *proxy.WhamUsage {
	u := &proxy.WhamUsage{PlanType: "pro"}
	u.RateLimit.Allowed = primary < 100 && secondary < 100
	u.RateLimit.PrimaryWindow = &proxy.WhamUsageWindow{UsedPercent: primary, LimitWindowSeconds: 18000, ResetAt: time.Now().Add(time.Hour).Unix()}
	u.RateLimit.SecondaryWindow = &proxy.WhamUsageWindow{UsedPercent: secondary, LimitWindowSeconds: 604800, ResetAt: time.Now().Add(24 * time.Hour).Unix()}
	return u
}

func TestResetCreditMainUsageState(t *testing.T) {
	for _, tc := range []struct {
		name               string
		edit               func(*proxy.WhamUsage)
		limited, recovered bool
	}{
		{name: "healthy", recovered: true},
		{name: "primary exhausted", edit: func(u *proxy.WhamUsage) { u.RateLimit.PrimaryWindow.UsedPercent = 100 }, limited: true},
		{name: "secondary exhausted", edit: func(u *proxy.WhamUsage) { u.RateLimit.SecondaryWindow.UsedPercent = 100 }, limited: true},
		{name: "authoritative main flag", edit: func(u *proxy.WhamUsage) { u.RateLimit.LimitReached = true }, limited: true},
		{name: "generic denial", edit: func(u *proxy.WhamUsage) { u.RateLimit.Allowed = false }},
		{name: "missing weekly window cannot rearm", edit: func(u *proxy.WhamUsage) { u.RateLimit.SecondaryWindow = nil }},
		{name: "stale full window", edit: func(u *proxy.WhamUsage) {
			u.RateLimit.PrimaryWindow.UsedPercent = 100
			u.RateLimit.PrimaryWindow.ResetAt = time.Now().Add(-time.Second).Unix()
		}},
		{name: "malformed percentage", edit: func(u *proxy.WhamUsage) { u.RateLimit.SecondaryWindow.UsedPercent = math.NaN() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := resetLimitTestUsage(20, 30)
			if tc.edit != nil {
				tc.edit(u)
			}
			limited, recovered := resetCreditMainUsageState(u, time.Now())
			if limited != tc.limited || recovered != tc.recovered {
				t.Fatalf("got (%v,%v), want (%v,%v)", limited, recovered, tc.limited, tc.recovered)
			}
		})
	}
	var sparkOnly proxy.WhamUsage
	if err := json.Unmarshal([]byte(`{"rate_limit":{"allowed":true,"primary_window":{"used_percent":5,"limit_window_seconds":18000},"secondary_window":{"used_percent":10,"limit_window_seconds":604800}},"additional_rate_limits":[{"limit_name":"gpt-5.3-codex-spark","rate_limit":{"limit_reached":true,"primary_window":{"used_percent":100}}}]}`), &sparkOnly); err != nil {
		t.Fatal(err)
	}
	if limited, recovered := resetCreditMainUsageState(&sparkOnly, time.Now()); limited || !recovered {
		t.Fatalf("Spark-only limit = (%v,%v)", limited, recovered)
	}
	if limited, recovered := resetCreditMainUsageState(&proxy.WhamUsage{}, time.Now()); limited || recovered {
		t.Fatal("empty payload must not trigger or rearm")
	}
}

func enableResetLimitTest(t *testing.T) {
	t.Helper()
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	settings := proxy.DefaultRuntimeSettings()
	settings.AutoResetCreditsOnLimitEnabled = true
	proxy.ApplyRuntimeSettings(settings)
}

func resetLimitTestHandler(t *testing.T, tc cache.TokenCache, account *auth.Account, consumes *atomic.Int32) *Handler {
	t.Helper()
	store := auth.NewStore(nil, tc, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.AddAccount(account)
	h := &Handler{
		store: store, cache: tc,
		queryResetUsage: func(context.Context, *auth.Account, string) (*proxy.WhamUsage, *http.Response, error) {
			return resetLimitTestUsage(100, 20), nil, nil
		},
		queryResetCredits: func(context.Context, *auth.Account, string) (*proxy.WhamResetCreditsList, *http.Response, error) {
			return &proxy.WhamResetCreditsList{AvailableCount: 2, Credits: []proxy.WhamResetCreditItem{{ID: "credit-1", Status: "available", ResetType: "codex_rate_limits", ConsumableUntil: time.Now().Add(7 * 24 * time.Hour).Format(time.RFC3339)}}}, nil, nil
		},
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			consumes.Add(1)
			return &proxy.WhamResetResult{WindowsReset: 2}, nil, nil
		},
		recordAccountEvent: func(int64, string, string) {},
		probeUsage:         func(context.Context, *auth.Account) error { return nil },
	}
	t.Cleanup(h.WaitAutoResetCredits)
	return h
}

func TestAutoResetCreditsOnLimitChecksFreshMainQuota(t *testing.T) {
	enableResetLimitTest(t)
	for _, tc := range []struct {
		name  string
		usage func(int) *proxy.WhamUsage
		want  int32
	}{
		{"main exhausted nonexpiring card", func(int) *proxy.WhamUsage { return resetLimitTestUsage(100, 20) }, 1},
		{"weekly exhausted", func(int) *proxy.WhamUsage { return resetLimitTestUsage(20, 100) }, 1},
		{"healthy main despite cached full", func(int) *proxy.WhamUsage { return resetLimitTestUsage(20, 30) }, 0},
		{"recovers while querying cards", func(n int) *proxy.WhamUsage {
			if n == 1 {
				return resetLimitTestUsage(100, 20)
			}
			return resetLimitTestUsage(20, 30)
		}, 0},
		{"plan downgraded", func(int) *proxy.WhamUsage { u := resetLimitTestUsage(100, 20); u.PlanType = "free"; return u }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var consumes atomic.Int32
			account := &auth.Account{DBID: 1, AccountID: "workspace", AccessToken: "token", PlanType: "pro"}
			account.SetUsageSnapshot5h(100, time.Now().Add(time.Hour))
			h := resetLimitTestHandler(t, nil, account, &consumes)
			queries := 0
			h.queryResetUsage = func(context.Context, *auth.Account, string) (*proxy.WhamUsage, *http.Response, error) {
				queries++
				return tc.usage(queries), nil, nil
			}
			h.runAutoResetCreditsScan(context.Background(), time.Time{})
			if got := consumes.Load(); got != tc.want {
				t.Fatalf("consumes=%d, want %d", got, tc.want)
			}
		})
	}
}

func TestAutoResetCreditsOnLimitConcurrentSharedWorkspace(t *testing.T) {
	enableResetLimitTest(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	var consumes atomic.Int32
	handlers := []*Handler{
		resetLimitTestHandler(t, tc, &auth.Account{DBID: 1, AccountID: "same-workspace", AccessToken: "token", PlanType: "plus"}, &consumes),
		resetLimitTestHandler(t, tc, &auth.Account{DBID: 2, AccountID: "same-workspace", AccessToken: "token", PlanType: "pro"}, &consumes),
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(h *Handler) {
			defer wg.Done()
			<-start
			h.runAutoResetCreditsScan(context.Background(), time.Time{})
		}(handlers[i%2])
	}
	close(start)
	wg.Wait()
	if got := consumes.Load(); got != 1 {
		t.Fatalf("concurrent consumes=%d, want 1", got)
	}
	// 自动后手动也必须受同一个工作区冷却保护。
	outcome, failure := handlers[1].consumeResetCreditLocked(context.Background(), handlers[1].store.Accounts()[0], "manual-after-auto", "manual")
	if failure != nil || !outcome.AlreadyHandled || consumes.Load() != 1 {
		t.Fatalf("manual outcome=%+v failure=%+v", outcome, failure)
	}
}

func TestAutoResetCreditsOnLimitUnknownOutcomeWaitsForRecovery(t *testing.T) {
	enableResetLimitTest(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	var consumes atomic.Int32
	account := &auth.Account{DBID: 1, AccountID: "workspace", AccessToken: "token", PlanType: "pro"}
	h := resetLimitTestHandler(t, tc, account, &consumes)
	h.consumeResetCredit = func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
		consumes.Add(1)
		return nil, nil, context.DeadlineExceeded
	}
	if stats := h.runAutoResetCreditsScan(context.Background(), time.Time{}); stats.Failed != 1 {
		t.Fatalf("stats=%+v", stats)
	}
	other := resetLimitTestHandler(t, tc, account, &consumes)
	other.runAutoResetCreditsScan(context.Background(), time.Time{})
	if consumes.Load() != 1 {
		t.Fatal("unknown result did not retain shared cooldown")
	}
	// 模拟一小时冷却结束。新实例仍需尊重未恢复的同一耗尽事件。
	if err := tc.DeleteRuntime(context.Background(), resetCreditCooldownNamespace, resetCreditLockKey(account)); err != nil {
		t.Fatal(err)
	}
	other.runAutoResetCreditsScan(context.Background(), time.Time{})
	if consumes.Load() != 1 {
		t.Fatal("same exhausted episode spent another credit after cooldown")
	}
	other.queryResetUsage = func(context.Context, *auth.Account, string) (*proxy.WhamUsage, *http.Response, error) {
		u := resetLimitTestUsage(10, 20)
		u.RateLimit.SecondaryWindow = nil
		return u, nil, nil
	}
	other.runAutoResetCreditsScan(context.Background(), time.Time{})
	if pending, err := other.resetCreditLimitAttemptPending(context.Background(), account); err != nil || !pending {
		t.Fatalf("partial recovery removed protection: pending=%v err=%v", pending, err)
	}
	other.queryResetUsage = func(context.Context, *auth.Account, string) (*proxy.WhamUsage, *http.Response, error) {
		return resetLimitTestUsage(10, 20), nil, nil
	}
	other.runAutoResetCreditsScan(context.Background(), time.Time{})
	other.queryResetUsage = func(context.Context, *auth.Account, string) (*proxy.WhamUsage, *http.Response, error) {
		return resetLimitTestUsage(100, 20), nil, nil
	}
	other.runAutoResetCreditsScan(context.Background(), time.Time{})
	if consumes.Load() != 2 {
		t.Fatal("fresh exhausted episode did not rearm after confirmed recovery")
	}
}

type resetCreditWriteFailureCache struct {
	cache.TokenCache
	namespace string
}

func (c resetCreditWriteFailureCache) SetRuntime(ctx context.Context, namespace, key string, value json.RawMessage, ttl time.Duration) error {
	if namespace == c.namespace {
		return errors.New("cache write failed")
	}
	return c.TokenCache.SetRuntime(ctx, namespace, key, value, ttl)
}

func TestAutoResetCreditsOnLimitCacheFailureNeverConsumes(t *testing.T) {
	enableResetLimitTest(t)
	for _, namespace := range []string{resetCreditCooldownNamespace, resetCreditLimitAttemptNamespace} {
		t.Run(namespace, func(t *testing.T) {
			tc := cache.NewMemory(4)
			t.Cleanup(func() { _ = tc.Close() })
			var consumes atomic.Int32
			h := resetLimitTestHandler(t, resetCreditWriteFailureCache{tc, namespace}, &auth.Account{DBID: 1, AccountID: "workspace", AccessToken: "token", PlanType: "pro"}, &consumes)
			stats := h.runAutoResetCreditsScan(context.Background(), time.Time{})
			if stats.Failed != 1 || consumes.Load() != 0 {
				t.Fatalf("stats=%+v consumes=%d", stats, consumes.Load())
			}
		})
	}
}

func TestAutoResetCreditsOnLimitReloadsSwitchBeforeConsume(t *testing.T) {
	enableResetLimitTest(t)
	var consumes atomic.Int32
	h := resetLimitTestHandler(t, nil, &auth.Account{DBID: 1, AccountID: "workspace", AccessToken: "token", PlanType: "pro"}, &consumes)
	query := h.queryResetCredits
	h.queryResetCredits = func(ctx context.Context, account *auth.Account, proxyURL string) (*proxy.WhamResetCreditsList, *http.Response, error) {
		settings := proxy.CurrentRuntimeSettings()
		settings.AutoResetCreditsOnLimitEnabled = false
		proxy.ApplyRuntimeSettings(settings)
		return query(ctx, account, proxyURL)
	}
	h.runAutoResetCreditsScan(context.Background(), time.Time{})
	if consumes.Load() != 0 {
		t.Fatal("disabled switch did not stop pending consume")
	}
}

func TestResetCreditRecoveryDoesNotClearNewerAttempt(t *testing.T) {
	var consumes atomic.Int32
	account := &auth.Account{DBID: 1, AccountID: "workspace", AccessToken: "token", PlanType: "pro"}
	h := resetLimitTestHandler(t, nil, account, &consumes)
	oldObservation := time.Now().Add(-time.Minute)
	if err := h.markResetCreditLimitAttempt(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	if err := h.clearResetCreditLimitAttempt(context.Background(), account, oldObservation); err != nil {
		t.Fatal(err)
	}
	if pending, err := h.resetCreditLimitAttemptPending(context.Background(), account); err != nil || !pending {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
}
