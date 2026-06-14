package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestExchangeOAuthCodeSeedsAccessTokenFromExchangeResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	handler := &Handler{db: db, store: store}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"access_token": "access-from-exchange",
			"refresh_token": "refresh-from-exchange",
			"id_token": "id-from-exchange",
			"expires_in": 3600
		}`))
	}))
	defer server.Close()

	oldResinCfg := proxy.GetResinConfig()
	oldDecorator := auth.ResinRequestDecorator
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "codex2api"})
	t.Cleanup(func() {
		proxy.SetResinConfig(oldResinCfg)
		auth.ResinRequestDecorator = oldDecorator
	})

	sessionID := "oauth-test-session"
	globalOAuthStore.set(sessionID, &oauthSession{
		State:        "state-test",
		CodeVerifier: "verifier-test",
		RedirectURI:  oauthDefaultRedirectURI,
		CreatedAt:    time.Now(),
	})
	t.Cleanup(func() {
		globalOAuthStore.delete(sessionID)
	})

	body := `{"session_id":"oauth-test-session","code":"code-test","state":"state-test"}`
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/oauth/exchange-code", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.ExchangeOAuthCode(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var payload struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.ID == 0 {
		t.Fatal("response id is empty")
	}

	account := store.FindByID(payload.ID)
	if account == nil {
		t.Fatalf("runtime account %d not found", payload.ID)
	}
	account.Mu().RLock()
	accessToken := account.AccessToken
	refreshToken := account.RefreshToken
	account.Mu().RUnlock()
	if accessToken != "access-from-exchange" || refreshToken != "refresh-from-exchange" {
		t.Fatalf("runtime tokens = access:%q refresh:%q, want exchange tokens", accessToken, refreshToken)
	}

	row, err := db.GetAccountByID(context.Background(), payload.ID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("access_token"); got != "access-from-exchange" {
		t.Fatalf("stored access_token = %q, want exchange access token", got)
	}
	if got := row.GetCredential("id_token"); got != "id-from-exchange" {
		t.Fatalf("stored id_token = %q, want exchange id token", got)
	}
}

func newOAuthExchangeTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"access_token": "access-from-exchange",
			"refresh_token": "refresh-from-exchange",
			"id_token": "id-from-exchange",
			"expires_in": 3600
		}`))
	}))
	t.Cleanup(server.Close)

	oldResinCfg := proxy.GetResinConfig()
	oldDecorator := auth.ResinRequestDecorator
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "codex2api"})
	t.Cleanup(func() {
		proxy.SetResinConfig(oldResinCfg)
		auth.ResinRequestDecorator = oldDecorator
	})
	return server
}

func TestExchangeOAuthCodeTriggersUsageProbe(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	probed := make(chan int64, 1)
	handler := &Handler{db: db, store: store}
	handler.probeUsage = func(_ context.Context, account *auth.Account) error {
		probed <- account.DBID
		return nil
	}

	newOAuthExchangeTestServer(t)

	sessionID := "oauth-probe-session"
	globalOAuthStore.set(sessionID, &oauthSession{
		State:        "state-probe",
		CodeVerifier: "verifier-probe",
		RedirectURI:  oauthDefaultRedirectURI,
		CreatedAt:    time.Now(),
	})
	t.Cleanup(func() {
		globalOAuthStore.delete(sessionID)
	})

	body := `{"session_id":"oauth-probe-session","code":"code-probe","state":"state-probe"}`
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/oauth/exchange-code", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.ExchangeOAuthCode(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var payload struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	select {
	case dbID := <-probed:
		if dbID != payload.ID {
			t.Fatalf("usage probe ran for account %d, want %d", dbID, payload.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("usage probe was not triggered after OAuth account add")
	}
}

func TestOAuthCallbackTriggersUsageProbe(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	probed := make(chan int64, 1)
	handler := &Handler{db: db, store: store}
	handler.probeUsage = func(_ context.Context, account *auth.Account) error {
		probed <- account.DBID
		return nil
	}

	newOAuthExchangeTestServer(t)

	sessionID := "oauth-callback-probe-session"
	sess := &oauthSession{
		State:        "state-callback-probe",
		CodeVerifier: "verifier-callback-probe",
		RedirectURI:  oauthDefaultRedirectURI,
		CreatedAt:    time.Now(),
	}
	globalOAuthStore.set(sessionID, sess)
	t.Cleanup(func() {
		globalOAuthStore.delete(sessionID)
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/oauth/callback?code=code-callback-probe&state=state-callback-probe", nil)

	handler.OAuthCallback(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if sess.ExchangeResult == nil || !sess.ExchangeResult.Success {
		t.Fatalf("exchange result = %+v, want success", sess.ExchangeResult)
	}

	select {
	case dbID := <-probed:
		if dbID != sess.ExchangeResult.ID {
			t.Fatalf("usage probe ran for account %d, want %d", dbID, sess.ExchangeResult.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("usage probe was not triggered after OAuth callback account add")
	}
}

func insertOAuthEditTestAccount(t *testing.T, db *database.DB, name string, refreshToken string, proxyURL string) int64 {
	t.Helper()
	id, err := db.InsertAccount(context.Background(), name, refreshToken, proxyURL)
	if err != nil {
		t.Fatalf("InsertAccount: %v", err)
	}
	return id
}

func newOAuthEditRequest(sessionID, code, state, proxyURL string) *http.Request {
	body := fmt.Sprintf(`{"session_id":%q,"code":%q,"state":%q,"proxy_url":%q}`, sessionID, code, state, proxyURL)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/1/oauth/exchange-code", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestUpdateOAuthAccountCodeRejectsInvalidID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	handler := &Handler{db: db, store: store}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "bad"}}
	ctx.Request = newOAuthEditRequest("session", "code", "state", "")

	handler.UpdateOAuthAccountCode(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

func TestUpdateOAuthAccountCodeRejectsMissingFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	handler := &Handler{db: db, store: store}
	id := insertOAuthEditTestAccount(t, db, "oauth-existing", "old-refresh", "")

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", id)}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/1/oauth/exchange-code", strings.NewReader(`{"session_id":"","code":"code","state":"state"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateOAuthAccountCode(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

func TestUpdateOAuthAccountCodeRejectsMissingAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	handler := &Handler{db: db, store: store}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "999999"}}
	ctx.Request = newOAuthEditRequest("session", "code", "state", "")

	handler.UpdateOAuthAccountCode(ctx)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
}

func TestUpdateOAuthAccountCodeRejectsNonOAuthAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	handler := &Handler{db: db, store: store}

	id, err := db.InsertOpenAIResponsesAccount(context.Background(), "responses", map[string]interface{}{
		"upstream_type": auth.UpstreamOpenAIResponses,
		"base_url":      "https://api.openai.com",
		"api_key":       "sk-test",
		"models":        []string{"gpt-4.1"},
		"plan_type":     "api",
		"email":         "https://api.openai.com",
	}, "")
	if err != nil {
		t.Fatalf("InsertOpenAIResponsesAccount: %v", err)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", id)}}
	ctx.Request = newOAuthEditRequest("session", "code", "state", "")

	handler.UpdateOAuthAccountCode(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

func TestUpdateOAuthAccountCodeRejectsMissingSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	handler := &Handler{db: db, store: store}
	id := insertOAuthEditTestAccount(t, db, "oauth-existing", "old-refresh", "")

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", id)}}
	ctx.Request = newOAuthEditRequest("missing-session", "code", "state", "")

	handler.UpdateOAuthAccountCode(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

func TestUpdateOAuthAccountCodeRejectsStateMismatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	handler := &Handler{db: db, store: store}
	id := insertOAuthEditTestAccount(t, db, "oauth-existing", "old-refresh", "")

	sessionID := "oauth-edit-state-mismatch"
	globalOAuthStore.set(sessionID, &oauthSession{
		State:        "expected-state",
		CodeVerifier: "verifier-test",
		RedirectURI:  oauthDefaultRedirectURI,
		CreatedAt:    time.Now(),
	})
	t.Cleanup(func() { globalOAuthStore.delete(sessionID) })

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", id)}}
	ctx.Request = newOAuthEditRequest(sessionID, "code", "wrong-state", "")

	handler.UpdateOAuthAccountCode(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

func TestUpdateOAuthAccountCodeDoesNotExposeTokenErrorBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	handler := &Handler{db: db, store: store}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","refresh_token":"secret-refresh","access_token":"secret-access","id_token":"secret-id"}`))
	}))
	t.Cleanup(server.Close)

	oldResinCfg := proxy.GetResinConfig()
	oldDecorator := auth.ResinRequestDecorator
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "codex2api"})
	t.Cleanup(func() {
		proxy.SetResinConfig(oldResinCfg)
		auth.ResinRequestDecorator = oldDecorator
	})

	id := insertOAuthEditTestAccount(t, db, "oauth-existing", "old-refresh", "")
	sessionID := "oauth-edit-token-error"
	globalOAuthStore.set(sessionID, &oauthSession{
		State:        "state-token-error",
		CodeVerifier: "verifier-token-error",
		RedirectURI:  oauthDefaultRedirectURI,
		CreatedAt:    time.Now(),
	})
	t.Cleanup(func() { globalOAuthStore.delete(sessionID) })

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", id)}}
	ctx.Request = newOAuthEditRequest(sessionID, "code-token-error", "state-token-error", "")

	handler.UpdateOAuthAccountCode(ctx)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadGateway, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, leaked := range []string{"secret-refresh", "secret-access", "secret-id", "invalid_grant"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("response leaked token exchange error body value %q: %s", leaked, body)
		}
	}
	if !strings.Contains(body, "token 兑换失败 (HTTP 400)") {
		t.Fatalf("response = %s, want sanitized HTTP status error", body)
	}
}

func TestUpdateOAuthAccountCodeUpdatesExistingAccountInPlace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	probed := make(chan int64, 1)
	handler := &Handler{db: db, store: store}
	handler.probeUsage = func(_ context.Context, account *auth.Account) error {
		probed <- account.DBID
		return nil
	}

	newOAuthExchangeTestServer(t)

	id := insertOAuthEditTestAccount(t, db, "oauth-existing", "old-refresh", "http://old-proxy.example")
	if err := store.LoadAccountByID(context.Background(), id); err != nil {
		t.Fatalf("LoadAccountByID: %v", err)
	}
	beforeCount, err := db.CountAll(context.Background())
	if err != nil {
		t.Fatalf("CountAll before: %v", err)
	}

	sessionID := "oauth-edit-success-session"
	globalOAuthStore.set(sessionID, &oauthSession{
		State:        "state-success",
		CodeVerifier: "verifier-success",
		RedirectURI:  oauthDefaultRedirectURI,
		CreatedAt:    time.Now(),
	})
	t.Cleanup(func() { globalOAuthStore.delete(sessionID) })

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", id)}}
	ctx.Request = newOAuthEditRequest(sessionID, "code-success", "state-success", "http://new-proxy.example")

	handler.UpdateOAuthAccountCode(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	afterCount, err := db.CountAll(context.Background())
	if err != nil {
		t.Fatalf("CountAll after: %v", err)
	}
	if afterCount != beforeCount {
		t.Fatalf("account count = %d, want unchanged %d", afterCount, beforeCount)
	}

	row, err := db.GetAccountByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("refresh_token"); got != "refresh-from-exchange" {
		t.Fatalf("stored refresh_token = %q, want exchange refresh token", got)
	}
	if got := row.GetCredential("access_token"); got != "access-from-exchange" {
		t.Fatalf("stored access_token = %q, want exchange access token", got)
	}
	if got := row.GetCredential("id_token"); got != "id-from-exchange" {
		t.Fatalf("stored id_token = %q, want exchange id token", got)
	}
	if row.ProxyURL != "http://new-proxy.example" {
		t.Fatalf("proxy_url = %q, want new proxy", row.ProxyURL)
	}

	account := store.FindByID(id)
	if account == nil {
		t.Fatalf("runtime account %d not found", id)
	}
	account.Mu().RLock()
	accessToken := account.AccessToken
	refreshToken := account.RefreshToken
	proxyURL := account.ProxyURL
	account.Mu().RUnlock()
	if accessToken != "access-from-exchange" || refreshToken != "refresh-from-exchange" {
		t.Fatalf("runtime tokens = access:%q refresh:%q, want exchange tokens", accessToken, refreshToken)
	}
	if proxyURL != "http://new-proxy.example" {
		t.Fatalf("runtime proxy = %q, want new proxy", proxyURL)
	}

	select {
	case dbID := <-probed:
		if dbID != id {
			t.Fatalf("usage probe ran for account %d, want %d", dbID, id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("usage probe was not triggered after OAuth account update")
	}
}
