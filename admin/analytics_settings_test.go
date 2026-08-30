package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestCodexAnalyticsWebSettingIsPersistedHotAndAuthoritative(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousRuntime := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previousRuntime) })

	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	settings.CodexAnalyticsEnabled = false
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	proxy.ApplyRuntimeSettingsFromSystem(settings)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")

	put := func(body string) settingsResponse {
		t.Helper()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		handler.UpdateSettings(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("PUT status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var response settingsResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode PUT: %v", err)
		}
		return response
	}

	if response := put(`{"codex_analytics_enabled":true}`); !response.CodexAnalyticsEnabled {
		t.Fatal("PUT response did not enable analytics")
	}
	if !proxy.CurrentRuntimeSettings().CodexAnalyticsEnabled {
		t.Fatal("analytics setting did not hot-enable runtime")
	}
	persisted, err := db.GetSystemSettings(context.Background())
	if err != nil || persisted == nil || !persisted.CodexAnalyticsEnabled {
		t.Fatalf("persisted analytics = %#v, err=%v", persisted, err)
	}

	// Simulate a stale local runtime snapshot. GET must still return the database
	// value because the web-persisted setting is authoritative across replicas.
	proxy.UpdateRuntimeSettings(func(current proxy.RuntimeSettings) proxy.RuntimeSettings {
		current.CodexAnalyticsEnabled = false
		return current
	})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
	handler.GetSettings(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response settingsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if !response.CodexAnalyticsEnabled {
		t.Fatal("GET used stale runtime instead of persisted web setting")
	}

	if response := put(`{"codex_analytics_enabled":false}`); response.CodexAnalyticsEnabled {
		t.Fatal("PUT response did not disable analytics")
	}
	if proxy.CurrentRuntimeSettings().CodexAnalyticsEnabled {
		t.Fatal("analytics setting did not hot-disable runtime")
	}
}
