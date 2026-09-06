package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestUpdateSettingsPersistsAutoResetCreditsAcrossPartialUpdatesOnLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousRuntime := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previousRuntime) })

	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	settings.ModelPricingOverrides = `{"gpt-5":{"input":1}}`
	settings.ModelPricingSyncURL = "https://example.com/pricing.json"
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	proxy.ApplyRuntimeSettingsFromSystem(settings)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")

	update := func(body string) {
		t.Helper()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")

		handler.UpdateSettings(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
	}

	// 阈值必须能在开关关闭时先保存，避免启用后的即时扫描使用旧默认值。
	update(`{"auto_reset_credits_before_expiry_min":90}`)
	select {
	case <-handler.autoResetCreditsWake:
	default:
		t.Fatal("threshold change did not queue an immediate scan")
	}
	beforeEnable := proxy.CurrentRuntimeSettings()
	if beforeEnable.AutoResetCreditsOnLimitEnabled {
		t.Fatal("AutoResetCreditsOnLimitEnabled = true before explicit enable")
	}
	if beforeEnable.AutoResetCreditsBeforeExpiryMin != 90 {
		t.Fatalf("runtime threshold before enable = %d, want 90", beforeEnable.AutoResetCreditsBeforeExpiryMin)
	}
	update(`{"auto_reset_credits_on_limit_enabled":true}`)
	if proxy.CurrentRuntimeSettings().AutoResetCreditsEnabled {
		t.Fatal("enabling on-limit reset also enabled expiry reset")
	}
	select {
	case <-handler.autoResetCreditsWake:
	default:
		t.Fatal("enable change did not queue an immediate scan")
	}
	update(`{"auto_reset_credits_on_limit_enabled":true,"auto_reset_credits_before_expiry_min":90}`)
	select {
	case <-handler.autoResetCreditsWake:
		t.Fatal("same-value auto-reset settings queued another scan")
	default:
	}
	update(`{"site_name":"Codex2API Test"}`)
	for _, boundary := range []int{10, 10080, 90} {
		update(fmt.Sprintf(`{"auto_reset_credits_before_expiry_min":%d}`, boundary))
		if got := proxy.CurrentRuntimeSettings().AutoResetCreditsBeforeExpiryMin; got != boundary {
			t.Fatalf("runtime boundary = %d, want %d", got, boundary)
		}
		select {
		case <-handler.autoResetCreditsWake:
		default:
			t.Fatalf("boundary %d did not queue an immediate scan", boundary)
		}
	}

	persisted, err := db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	if persisted == nil {
		t.Fatal("GetSystemSettings returned nil")
	}
	if !persisted.AutoResetCreditsOnLimitEnabled {
		t.Fatal("AutoResetCreditsOnLimitEnabled = false, want true after unrelated partial update")
	}
	if persisted.AutoResetCreditsBeforeExpiryMin != 90 {
		t.Fatalf("AutoResetCreditsBeforeExpiryMin = %d, want 90", persisted.AutoResetCreditsBeforeExpiryMin)
	}
	if persisted.ModelPricingOverrides != settings.ModelPricingOverrides {
		t.Fatalf("ModelPricingOverrides = %q, want %q", persisted.ModelPricingOverrides, settings.ModelPricingOverrides)
	}
	if persisted.ModelPricingSyncURL != settings.ModelPricingSyncURL {
		t.Fatalf("ModelPricingSyncURL = %q, want %q", persisted.ModelPricingSyncURL, settings.ModelPricingSyncURL)
	}
}

func TestUpdateSettingsDoesNotEnableAutoResetCreditsWhenPersistenceFailsOnLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousRuntime := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previousRuntime) })

	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	proxy.ApplyRuntimeSettingsFromSystem(settings)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")

	requestCtx, cancel := context.WithCancel(context.Background())
	cancel()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(`{"auto_reset_credits_on_limit_enabled":true}`))
	ctx.Request = request.WithContext(requestCtx)
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateSettings(ctx)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want %d body=%s", recorder.Code, http.StatusInternalServerError, recorder.Body.String())
	}
	if current := proxy.CurrentRuntimeSettings(); current.AutoResetCreditsOnLimitEnabled {
		t.Fatal("AutoResetCreditsOnLimitEnabled became true after persistence failure")
	}
}

func TestAutoResetCreditsSettingsUseDatabaseAuthorityOnStaleInstanceOnLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousRuntime := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previousRuntime) })

	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	settings.AutoResetCreditsOnLimitEnabled = false
	settings.AutoResetCreditsBeforeExpiryMin = 60
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")

	staleRuntime := proxy.DefaultRuntimeSettings()
	staleRuntime.AutoResetCreditsOnLimitEnabled = true
	staleRuntime.AutoResetCreditsBeforeExpiryMin = 90
	proxy.ApplyRuntimeSettings(staleRuntime)

	getRecorder := httptest.NewRecorder()
	getCtx, _ := gin.CreateTestContext(getRecorder)
	getCtx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
	handler.GetSettings(getCtx)
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", getRecorder.Code, getRecorder.Body.String())
	}
	var response settingsResponse
	if err := json.Unmarshal(getRecorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode GET settings: %v", err)
	}
	if response.AutoResetCreditsOnLimitEnabled || response.AutoResetCreditsBeforeExpiryMin != 60 {
		t.Fatalf("GET auto settings=(%v,%d), want DB authority (false,60)", response.AutoResetCreditsOnLimitEnabled, response.AutoResetCreditsBeforeExpiryMin)
	}

	updateRecorder := httptest.NewRecorder()
	updateCtx, _ := gin.CreateTestContext(updateRecorder)
	updateCtx.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(`{"site_name":"Stale Replica"}`))
	updateCtx.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateSettings(updateCtx)
	if updateRecorder.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", updateRecorder.Code, updateRecorder.Body.String())
	}

	persisted, err := db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	if persisted.AutoResetCreditsOnLimitEnabled || persisted.AutoResetCreditsBeforeExpiryMin != 60 {
		t.Fatalf("persisted auto settings=(%v,%d), want (false,60)", persisted.AutoResetCreditsOnLimitEnabled, persisted.AutoResetCreditsBeforeExpiryMin)
	}
	current := proxy.CurrentRuntimeSettings()
	if current.AutoResetCreditsOnLimitEnabled || current.AutoResetCreditsBeforeExpiryMin != 60 {
		t.Fatalf("runtime auto settings=(%v,%d), want refreshed DB authority (false,60)", current.AutoResetCreditsOnLimitEnabled, current.AutoResetCreditsBeforeExpiryMin)
	}
	select {
	case <-handler.autoResetCreditsWake:
		t.Fatal("unrelated stale-instance update queued an auto-reset scan")
	default:
	}
}
