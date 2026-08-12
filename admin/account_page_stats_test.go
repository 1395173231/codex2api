package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func invokeAccountPageStats(t *testing.T, handler *Handler, ids []int64) map[string]accountPageStatsItem {
	t.Helper()
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/page-stats?ids="+strings.Join(parts, ","), nil)
	handler.GetAccountPageStats(ginContext)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Stats map[string]accountPageStatsItem `json:"stats"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode page-stats: %v", err)
	}
	return payload.Stats
}

func waitAccountDailyUsage(t *testing.T, db *database.DB, id int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		totals, err := db.SumAccountDailyUsage(context.Background(), []int64{id}, 7)
		if err != nil {
			t.Fatalf("SumAccountDailyUsage: %v", err)
		}
		if _, ok := totals[id]; ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("account %d snapshot did not appear", id)
}

func TestGetAccountPageStatsBackfillsMissingOfficialUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	codexID, err := db.InsertAccountWithCredentials(ctx, "codex", map[string]interface{}{
		"refresh_token": "rt-codex",
		"access_token":  "at-codex",
		"email":         "codex@example.com",
	}, "")
	if err != nil {
		t.Fatalf("insert codex: %v", err)
	}
	grokID, err := db.InsertAccountWithUpstream(ctx, "grok", "xai", "oauth", map[string]interface{}{
		"upstream_type": "grok",
		"refresh_token": "rt-grok",
		"access_token":  "at-grok",
		"email":         "grok@example.net",
	}, "")
	if err != nil {
		t.Fatalf("insert grok: %v", err)
	}

	store := auth.NewStore(db, nil, nil)
	store.SetLazyMode(true)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	tokenCache := cache.NewMemory(1)
	t.Cleanup(func() { _ = tokenCache.Close() })
	handler := NewHandler(store, db, tokenCache, nil, "")

	var mu sync.Mutex
	called := make([]int64, 0, 2)
	queried := make(chan struct{}, 1)
	handler.queryWhamDailyUsage = func(_ context.Context, account *auth.Account, _, _, _ string) (*proxy.WhamDailyUsageResponse, *http.Response, error) {
		mu.Lock()
		called = append(called, account.DBID)
		mu.Unlock()
		select {
		case queried <- struct{}{}:
		default:
		}
		return &proxy.WhamDailyUsageResponse{Data: []proxy.WhamDailyUsageDay{{
			Date:   time.Now().UTC().Format("2006-01-02"),
			Totals: proxy.WhamDailyUsageCounts{Credits: 50},
		}}}, nil, nil
	}

	first := invokeAccountPageStats(t, handler, []int64{codexID, grokID})
	codexKey := strconv.FormatInt(codexID, 10)
	if item := first[codexKey]; item.OfficialUSD7d != nil {
		t.Fatalf("first page-stats official = %v, want omitted until snapshot exists", *item.OfficialUSD7d)
	}

	select {
	case <-queried:
	case <-time.After(2 * time.Second):
		t.Fatal("missing official snapshot did not trigger upstream backfill")
	}
	waitAccountDailyUsage(t, db, codexID)

	second := invokeAccountPageStats(t, handler, []int64{codexID, grokID})
	got := second[codexKey].OfficialUSD7d
	if got == nil || *got != 2 {
		t.Fatalf("official_usd_7d = %v, want 2 (50 credits / 25)", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(called) != 1 || called[0] != codexID {
		t.Fatalf("upstream accounts = %v, want only codex %d", called, codexID)
	}
}

func TestGetAccountPageStatsSkipsOfficialBackfillWhenSnapshotExists(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "codex", map[string]interface{}{
		"refresh_token": "rt",
		"access_token":  "at",
		"email":         "codex@example.com",
	}, "")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db.UpsertAccountDailyUsage(ctx, database.AccountDailyUsageInput{
		AccountID: id,
		Day:       time.Now().UTC().Format("2006-01-02"),
		Credits:   25,
		Settled:   true,
	}); err != nil {
		t.Fatalf("upsert snapshot: %v", err)
	}

	store := auth.NewStore(db, nil, nil)
	store.SetLazyMode(true)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	tokenCache := cache.NewMemory(1)
	t.Cleanup(func() { _ = tokenCache.Close() })
	handler := NewHandler(store, db, tokenCache, nil, "")
	handler.queryWhamDailyUsage = func(context.Context, *auth.Account, string, string, string) (*proxy.WhamDailyUsageResponse, *http.Response, error) {
		t.Fatal("existing snapshot must not hit upstream")
		return nil, nil, nil
	}

	stats := invokeAccountPageStats(t, handler, []int64{id})
	got := stats[strconv.FormatInt(id, 10)].OfficialUSD7d
	if got == nil || *got != 1 {
		t.Fatalf("official_usd_7d = %v, want 1", got)
	}
}
