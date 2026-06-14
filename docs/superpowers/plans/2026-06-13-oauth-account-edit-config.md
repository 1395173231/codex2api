# OAuth Account Edit Config Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let admins re-authorize an existing OAuth account from `/admin/accounts` by generating a new OAuth link, pasting the callback URL, and updating the current account instead of creating a duplicate account.

**Architecture:** Keep the add-account OAuth flow unchanged and add a dedicated edit-only endpoint at `POST /api/admin/accounts/:id/oauth/exchange-code`. The frontend owns the manual callback URL parsing and edit-modal state, while the backend validates the target account type, exchanges the OAuth code through the existing PKCE helper, atomically updates the existing account credentials/proxy, and reloads the runtime store entry.

**Tech Stack:** Go, Gin, SQLite/PostgreSQL-compatible database layer, React 19, TypeScript, Vite, react-i18next, existing Tailwind/UI components.

---

## File Structure

- Modify: `admin/oauth_test.go`
  - Add focused tests for the new edit-only OAuth exchange endpoint.
  - Covers invalid id, missing payload fields, missing session, state mismatch, non-OAuth rejection, and successful in-place update.
- Modify: `database/postgres.go`
  - Add `UpdateOAuthAccountCredentials(ctx, id, credentials, proxyURL)` to merge credentials and update `proxy_url` in one transaction without changing account id or creating a row.
- Modify: `admin/oauth.go`
  - Add `UpdateOAuthAccountCode(c *gin.Context)`.
  - Reuse existing `doOAuthCodeExchange`, `normalizeTokenCredentialSeed`, and `tokenCredentialMap`.
  - Never return refresh/access/id tokens in responses or logs.
- Modify: `admin/handler.go`
  - Register `POST /api/admin/accounts/:id/oauth/exchange-code`.
- Modify: `frontend/src/types.ts`
  - Add `UpdateOAuthAccountRequest` for the edit-only OAuth update request.
- Modify: `frontend/src/api.ts`
  - Add `api.updateOAuthAccount(id, data)`.
- Modify: `frontend/src/pages/Accounts.tsx`
  - Add OAuth account detection, edit OAuth state, handlers, footer behavior, and account-settings tab UI for OAuth accounts.
  - Keep add-account OAuth state separate.
- Modify: `frontend/src/locales/zh.json`
  - Add Chinese UI strings for OAuth re-authorization.
- Modify: `frontend/src/locales/en.json`
  - Add English UI strings for OAuth re-authorization.

---

## Task 1: Add backend failing tests for OAuth account re-authorization

**Files:**
- Modify: `admin/oauth_test.go`

- [ ] **Step 1: Add helpers and tests to `admin/oauth_test.go`**

Append the following test code after `TestOAuthCallbackTriggersUsageProbe` in `admin/oauth_test.go`:

```go
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
```

- [ ] **Step 2: Add missing imports for the new tests**

Update the import block in `admin/oauth_test.go` to include `fmt` and `github.com/codex2api/database`:

```go
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
```

- [ ] **Step 3: Run tests to verify they fail for the intended reason**

Run:

```powershell
go test ./admin -run UpdateOAuthAccountCode -count=1
```

Expected: FAIL because `handler.UpdateOAuthAccountCode` is not defined and `database.DB.UpdateOAuthAccountCredentials` has not been added yet.

- [ ] **Step 4: Commit the failing tests**

Run:

```powershell
git add admin/oauth_test.go
git commit -m "test: cover OAuth account reauthorization"
```

Expected: commit succeeds with only test changes.

---

## Task 2: Add database method for in-place OAuth credential updates

**Files:**
- Modify: `database/postgres.go`

- [ ] **Step 1: Add `UpdateOAuthAccountCredentials` after `UpdateOpenAIResponsesAccount`**

Insert this function in `database/postgres.go` after `UpdateOpenAIResponsesAccount`:

```go
func (db *DB) UpdateOAuthAccountCredentials(ctx context.Context, id int64, credentials map[string]interface{}, proxyURL string) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	selectQuery := `SELECT credentials FROM accounts WHERE id = $1 AND status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'`
	if !db.isSQLite() {
		selectQuery += ` FOR UPDATE`
	}

	var currentRaw interface{}
	if err := tx.QueryRowContext(ctx, selectQuery, id).Scan(&currentRaw); err != nil {
		return err
	}

	merged := mergeCredentialMaps(decodeCredentials(currentRaw), credentials)
	credJSON, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("序列化 credentials 失败: %w", err)
	}

	updateQuery := `UPDATE accounts SET credentials = $1, proxy_url = $2, platform = 'openai', type = 'oauth', updated_at = CURRENT_TIMESTAMP WHERE id = $3`
	if !db.isSQLite() {
		updateQuery = `UPDATE accounts SET credentials = $1::jsonb, proxy_url = $2, platform = 'openai', type = 'oauth', updated_at = CURRENT_TIMESTAMP WHERE id = $3`
	}
	res, err := tx.ExecContext(ctx, updateQuery, credJSON, proxyURL, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}
```

- [ ] **Step 2: Run backend tests to verify the database method compiles once the handler exists**

Run:

```powershell
go test ./admin -run UpdateOAuthAccountCode -count=1
```

Expected: still FAIL because `UpdateOAuthAccountCode` is not defined. There should be no database-method syntax errors.

- [ ] **Step 3: Commit the database method**

Run:

```powershell
git add database/postgres.go
git commit -m "feat: add OAuth account credential updater"
```

Expected: commit succeeds with the database change.

---

## Task 3: Implement the backend OAuth edit endpoint

**Files:**
- Modify: `admin/oauth.go`
- Modify: `admin/handler.go`

- [ ] **Step 1: Add imports to `admin/oauth.go`**

Update the import block in `admin/oauth.go` so it includes these additional packages:

```go
import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)
```

- [ ] **Step 2: Add `UpdateOAuthAccountCode` to `admin/oauth.go`**

Insert this handler after `ExchangeOAuthCode` and before the `rawOAuthTokenResp` type:

```go
// UpdateOAuthAccountCode 用授权码更新已有 OAuth 账号的授权参数。
// POST /api/admin/accounts/:id/oauth/exchange-code
func (h *Handler) UpdateOAuthAccountCode(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	var req struct {
		SessionID string `json:"session_id"`
		Code      string `json:"code"`
		State     string `json:"state"`
		ProxyURL  string `json:"proxy_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.Code = strings.TrimSpace(req.Code)
	req.State = strings.TrimSpace(req.State)
	req.ProxyURL = strings.TrimSpace(req.ProxyURL)
	if req.SessionID == "" || req.Code == "" || req.State == "" {
		writeError(c, http.StatusBadRequest, "session_id、code 和 state 均为必填")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	row, err := h.db.GetAccountByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "账号不存在")
			return
		}
		writeInternalError(c, err)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(row.Type), "oauth") {
		writeError(c, http.StatusBadRequest, "当前账号不是 OAuth 授权类型，不能重新授权")
		return
	}

	sess, ok := globalOAuthStore.get(req.SessionID)
	if !ok {
		writeError(c, http.StatusBadRequest, "OAuth 会话不存在或已过期（有效期 30 分钟）")
		return
	}
	if req.State != sess.State {
		writeError(c, http.StatusBadRequest, "state 不匹配，请重新发起授权")
		return
	}

	proxyURL := sess.ProxyURL
	if req.ProxyURL != "" {
		proxyURL = req.ProxyURL
	}
	if proxyURL == "" {
		proxyURL = strings.TrimSpace(row.ProxyURL)
	}
	if proxyURL == "" && h.store != nil {
		proxyURL = h.store.GetProxyURL()
	}

	resinAccountID := fmt.Sprintf("%d", id)
	tokenResp, accountInfo, err := doOAuthCodeExchange(c.Request.Context(), req.Code, sess.CodeVerifier, sess.RedirectURI, proxyURL, resinAccountID)
	if err != nil {
		writeError(c, http.StatusBadGateway, "授权码兑换失败: "+err.Error())
		return
	}
	globalOAuthStore.delete(req.SessionID)

	if tokenResp.RefreshToken == "" {
		writeError(c, http.StatusBadGateway, "授权服务器未返回 refresh_token，请确认已开启 offline_access scope")
		return
	}

	seed := normalizeTokenCredentialSeed(tokenCredentialSeed{
		refreshToken: tokenResp.RefreshToken,
		accessToken:  tokenResp.AccessToken,
		idToken:      tokenResp.IDToken,
		expiresIn:    tokenResp.ExpiresIn,
	})
	credentials := tokenCredentialMap(seed)
	if err := h.db.UpdateOAuthAccountCredentials(ctx, id, credentials, proxyURL); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "账号不存在")
			return
		}
		writeError(c, http.StatusInternalServerError, "Token 写入数据库失败: "+err.Error())
		return
	}

	if h.store != nil {
		h.store.RemoveAccount(id)
		if err := h.store.LoadAccountByID(ctx, id); err != nil {
			writeError(c, http.StatusInternalServerError, "重新加载运行时账号失败: "+err.Error())
			return
		}
	}
	h.db.InsertAccountEventAsync(id, "updated", "oauth_reauth")

	if account := h.store.FindByID(id); account != nil && account.GetAccessToken() != "" {
		h.triggerImportedAccountUsageProbe(id, "oauth_reauth")
	} else if h.store != nil && !h.store.GetLazyMode() {
		go h.refreshImportedAccountAndProbe(id, "oauth_reauth_refresh")
	}

	email := ""
	planType := ""
	if accountInfo != nil {
		email = accountInfo.Email
		planType = accountInfo.PlanType
	}
	if email == "" {
		email = seed.email
	}
	if planType == "" {
		planType = seed.planType
	}

	c.JSON(http.StatusOK, gin.H{
		"message":   "OAuth 账号授权参数更新成功",
		"id":        id,
		"email":     email,
		"plan_type": planType,
	})
}
```

- [ ] **Step 3: Register the new route in `admin/handler.go`**

Add this route near the existing account edit routes in `RegisterRoutes`:

```go
api.POST("/accounts/:id/oauth/exchange-code", h.UpdateOAuthAccountCode)
```

The surrounding route block should include:

```go
api.POST("/accounts/openai-responses", h.AddOpenAIResponsesAccount)
api.POST("/accounts/openai-responses/models", h.FetchOpenAIResponsesModels)
api.PATCH("/accounts/:id/openai-responses", h.UpdateOpenAIResponsesAccount)
api.POST("/accounts/:id/oauth/exchange-code", h.UpdateOAuthAccountCode)
api.POST("/accounts/import", h.ImportAccounts)
```

- [ ] **Step 4: Run targeted backend tests**

Run:

```powershell
go test ./admin -run "UpdateOAuthAccountCode|ExchangeOAuthCode|OAuthCallback" -count=1
```

Expected: PASS. The success test proves the account count stays unchanged, credentials/proxy update, runtime store reloads, and usage probe runs.

- [ ] **Step 5: Run the full admin test package**

Run:

```powershell
go test ./admin -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit backend endpoint implementation**

Run:

```powershell
git add admin/oauth.go admin/handler.go
git commit -m "feat: update OAuth credentials on existing accounts"
```

Expected: commit succeeds with endpoint and route changes.

---

## Task 4: Add frontend API request type and client method

**Files:**
- Modify: `frontend/src/types.ts`
- Modify: `frontend/src/api.ts`

- [ ] **Step 1: Add `UpdateOAuthAccountRequest` to `frontend/src/types.ts`**

Insert this interface after `UpdateOpenAIResponsesAccountRequest`:

```ts
export interface UpdateOAuthAccountRequest {
  session_id: string
  code: string
  state: string
  proxy_url?: string
}
```

- [ ] **Step 2: Import the new type in `frontend/src/api.ts`**

Add `UpdateOAuthAccountRequest` to the type import list:

```ts
  UpdateOAuthAccountRequest,
  UpdateOpenAIResponsesAccountRequest,
```

- [ ] **Step 3: Add the edit-only OAuth API method**

In `frontend/src/api.ts`, add `updateOAuthAccount` in the OAuth section immediately after `exchangeOAuthCode`:

```ts
  exchangeOAuthCode: (data: { session_id: string; code: string; state: string; name?: string; proxy_url?: string }) =>
    request<OAuthExchangeResponse>('/oauth/exchange-code', { method: 'POST', body: JSON.stringify(data) }),
  updateOAuthAccount: (id: number, data: UpdateOAuthAccountRequest) =>
    request<OAuthExchangeResponse>(`/accounts/${id}/oauth/exchange-code`, { method: 'POST', body: JSON.stringify(data) }),
```

- [ ] **Step 4: Run frontend typecheck for API/type changes**

Run:

```powershell
npm --prefix frontend run typecheck
```

Expected: PASS.

- [ ] **Step 5: Commit frontend API/type changes**

Run:

```powershell
git add frontend/src/types.ts frontend/src/api.ts
git commit -m "feat: add OAuth account update API client"
```

Expected: commit succeeds with only API/type changes.

---

## Task 5: Add OAuth edit state and handlers in `Accounts.tsx`

**Files:**
- Modify: `frontend/src/pages/Accounts.tsx`

- [ ] **Step 1: Add OAuth helper functions near existing account helper functions**

Insert these helpers after `formatAccountListEmail`:

```ts
function isOAuthAccount(account: AccountRow | null): boolean {
  return account?.account_type === "oauth";
}

function parseOAuthCallbackParams(rawValue: string): { code: string; state: string } {
  const raw = rawValue.trim();
  try {
    const url = new URL(raw);
    return {
      code: url.searchParams.get("code") ?? "",
      state: url.searchParams.get("state") ?? "",
    };
  } catch {
    const qs = raw.includes("?") ? raw.split("?")[1] : raw;
    const params = new URLSearchParams(qs);
    return {
      code: params.get("code") ?? "",
      state: params.get("state") ?? "",
    };
  }
}
```

- [ ] **Step 2: Add independent edit OAuth state**

After the existing add-OAuth state declarations:

```ts
  const [oauthProxyUrl, setOauthProxyUrl] = useState("");
  const [oauthCallbackUrl, setOauthCallbackUrl] = useState("");
  const [oauthName, setOauthName] = useState("");
  const [oauthGenerating, setOauthGenerating] = useState(false);
  const [oauthCompleting, setOauthCompleting] = useState(false);
```

add:

```ts
  const [editOAuthStep, setEditOAuthStep] = useState<"generate" | "exchange">(
    "generate",
  );
  const [editOAuthSession, setEditOAuthSession] = useState<{
    session_id: string;
    auth_url: string;
  } | null>(null);
  const [editOAuthProxyUrl, setEditOAuthProxyUrl] = useState("");
  const [editOAuthCallbackUrl, setEditOAuthCallbackUrl] = useState("");
  const [editOAuthGenerating, setEditOAuthGenerating] = useState(false);
  const [editOAuthUpdating, setEditOAuthUpdating] = useState(false);
```

- [ ] **Step 3: Update add-OAuth completion to use the shared parser**

Replace the manual parsing block at the start of `handleOAuthComplete` with:

```ts
    const { code, state } = parseOAuthCallbackParams(oauthCallbackUrl);
```

The first lines of `handleOAuthComplete` should become:

```ts
  const handleOAuthComplete = async () => {
    if (!oauthSession) return;
    const { code, state } = parseOAuthCallbackParams(oauthCallbackUrl);
    if (!code || !state) {
      showToast(t("accounts.oauthParseError"), "error");
      return;
    }
```

- [ ] **Step 4: Add edit-OAuth handlers after `handleOAuthComplete`**

Insert this block after `handleOAuthComplete`:

```ts
  const startEditOAuthSession = async () => {
    const result = await api.generateOAuthURL({ proxy_url: editOAuthProxyUrl });
    setEditOAuthSession(result);
    setEditOAuthCallbackUrl("");
    setEditOAuthStep("exchange");
    return result;
  };

  const handleEditOAuthGenerate = async () => {
    setEditOAuthGenerating(true);
    try {
      await startEditOAuthSession();
    } catch (error) {
      showToast(
        t("accounts.oauthFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setEditOAuthGenerating(false);
    }
  };

  const handleEditOAuthRestart = async () => {
    setEditOAuthGenerating(true);
    setEditOAuthSession(null);
    setEditOAuthCallbackUrl("");
    try {
      await startEditOAuthSession();
    } catch (error) {
      setEditOAuthStep("generate");
      showToast(
        t("accounts.oauthFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setEditOAuthGenerating(false);
    }
  };

  const handleEditOAuthCopyLink = async () => {
    if (!editOAuthSession?.auth_url) return;
    try {
      await copyTextToClipboard(editOAuthSession.auth_url);
      showToast(t("common.copied"));
    } catch {
      showToast(t("common.copyFailed"), "error");
    }
  };

  const handleSaveOAuthAccountSettings = async () => {
    if (!editingAccount || !isOAuthAccount(editingAccount)) return;
    if (!editOAuthSession) {
      showToast(t("accounts.oauthGenerateFirst"), "error");
      return;
    }
    const { code, state } = parseOAuthCallbackParams(editOAuthCallbackUrl);
    if (!code || !state) {
      showToast(t("accounts.oauthParseError"), "error");
      return;
    }

    setEditSubmitting(true);
    setEditOAuthUpdating(true);
    try {
      const result = await api.updateOAuthAccount(editingAccount.id, {
        session_id: editOAuthSession.session_id,
        code,
        state,
        proxy_url: editOAuthProxyUrl.trim() || undefined,
      });
      showToast(
        result.email
          ? t("accounts.oauthUpdateSuccess", { email: result.email })
          : t("accounts.oauthUpdateSuccessNoEmail"),
      );
      await reload();
      closeSchedulerEditor(true);
    } catch (error) {
      showToast(
        t("accounts.oauthFailed", { error: getErrorMessage(error) }),
        "error",
      );
    } finally {
      setEditOAuthUpdating(false);
      setEditSubmitting(false);
    }
  };
```

- [ ] **Step 5: Initialize and clear edit OAuth state in editor open/close**

In `openSchedulerEditor`, after `setEditOpenAIModelDraft("");`, add:

```ts
    setEditOAuthStep("generate");
    setEditOAuthSession(null);
    setEditOAuthProxyUrl(account.proxy_url ?? "");
    setEditOAuthCallbackUrl("");
    setEditOAuthGenerating(false);
    setEditOAuthUpdating(false);
```

In `closeSchedulerEditor`, after `setEditOpenAIModelDraft("");`, add:

```ts
    setEditOAuthStep("generate");
    setEditOAuthSession(null);
    setEditOAuthProxyUrl("");
    setEditOAuthCallbackUrl("");
    setEditOAuthGenerating(false);
    setEditOAuthUpdating(false);
```

- [ ] **Step 6: Add account-settings capability flags and route save calls**

After `openAIAccountInputInvalid`, add:

```ts
  const isEditingOAuthAccount = isOAuthAccount(editingAccount);
  const canEditAccountSettings = Boolean(
    editingAccount?.openai_responses_api || isEditingOAuthAccount,
  );
```

Update `handleSaveAccountEditor` to route OAuth account settings to the new handler:

```ts
  const handleSaveAccountEditor = async () => {
    if (editingAccount?.openai_responses_api && editTab === "account") {
      await handleSaveOpenAIAccountSettings();
      return;
    }
    if (isOAuthAccount(editingAccount) && editTab === "account") {
      await handleSaveOAuthAccountSettings();
      return;
    }
    await handleSaveScheduler();
  };
```

- [ ] **Step 7: Run frontend typecheck for state/handler changes**

Run:

```powershell
npm --prefix frontend run typecheck
```

Expected: PASS.

- [ ] **Step 8: Commit state and handler changes**

Run:

```powershell
git add frontend/src/pages/Accounts.tsx
git commit -m "feat: add OAuth edit authorization handlers"
```

Expected: commit succeeds with `Accounts.tsx` logic changes.

---

## Task 6: Add OAuth account-settings UI and translations

**Files:**
- Modify: `frontend/src/pages/Accounts.tsx`
- Modify: `frontend/src/locales/zh.json`
- Modify: `frontend/src/locales/en.json`

- [ ] **Step 1: Add translation keys in `frontend/src/locales/zh.json`**

Add these keys after `oauthRestart`:

```json
    "oauthRestart": "重新发起授权",
    "oauthEditIntroTitle": "OAuth 重新授权",
    "oauthEditIntroDesc": "为当前 OAuth 账号生成新的授权链接，完成浏览器授权后粘贴回调 URL，系统会更新当前账号的授权参数，不会新增账号。",
    "oauthCurrentAccount": "当前账号",
    "oauthAuthLinkLabel": "授权链接",
    "oauthUpdateAuth": "更新授权参数",
    "oauthUpdateSuccess": "OAuth 授权参数已更新：{{email}}",
    "oauthUpdateSuccessNoEmail": "OAuth 授权参数已更新",
    "oauthGenerateFirst": "请先生成授权链接",
```

Ensure the previous `oauthRestart` line receives a trailing comma and the final inserted key uses a comma because more keys follow in the file.

- [ ] **Step 2: Add translation keys in `frontend/src/locales/en.json`**

Add these keys after `oauthRestart`:

```json
    "oauthRestart": "Restart Authorization",
    "oauthEditIntroTitle": "OAuth Re-authorization",
    "oauthEditIntroDesc": "Generate a new authorization link for the current OAuth account. After browser authorization, paste the callback URL to update this account's authorization parameters without creating a new account.",
    "oauthCurrentAccount": "Current Account",
    "oauthAuthLinkLabel": "Authorization Link",
    "oauthUpdateAuth": "Update Authorization",
    "oauthUpdateSuccess": "OAuth authorization updated: {{email}}",
    "oauthUpdateSuccessNoEmail": "OAuth authorization updated successfully",
    "oauthGenerateFirst": "Generate an authorization link first",
```

Ensure the JSON remains valid.

- [ ] **Step 3: Update footer button behavior in `Accounts.tsx`**

Replace the footer button `disabled` and label logic inside the edit modal with:

```tsx
                  disabled={
                    editSubmitting ||
                    editOAuthGenerating ||
                    (editTab === "scheduler" &&
                      (scoreInputInvalid ||
                        concurrencyInputInvalid ||
                        editAutoPause5hThresholdInvalid ||
                        editAutoPause7dThresholdInvalid)) ||
                    openAIAccountInputInvalid ||
                    (editTab === "account" &&
                      isEditingOAuthAccount &&
                      !editOAuthSession)
                  }
                >
                  {editTab === "account" && isEditingOAuthAccount
                    ? editOAuthUpdating
                      ? t("accounts.oauthCompleting")
                      : t("accounts.oauthUpdateAuth")
                    : editSubmitting
                      ? t("common.saving")
                      : t("common.save")}
```

- [ ] **Step 4: Show tabs for both OpenAI Responses and OAuth accounts**

Replace:

```tsx
                {editingAccount.openai_responses_api && (
```

with:

```tsx
                {canEditAccountSettings && (
```

- [ ] **Step 5: Add OAuth account settings UI branch**

Replace the start of the account-tab conditional:

```tsx
                {editTab === "account" &&
                editingAccount.openai_responses_api ? (
```

with:

```tsx
                {editTab === "account" && editingAccount.openai_responses_api ? (
```

Then insert this `else if` branch between the OpenAI Responses account-settings block and the scheduler fallback. The final structure must be:

```tsx
                {editTab === "account" && editingAccount.openai_responses_api ? (
                  <div className="space-y-4">
                    ...existing OpenAI Responses settings...
                  </div>
                ) : editTab === "account" && isEditingOAuthAccount ? (
                  <div className="space-y-4">
                    <div className="rounded-xl border border-primary/20 bg-primary/5 px-4 py-3">
                      <div className="text-sm font-semibold text-foreground">
                        {t("accounts.oauthEditIntroTitle")}
                      </div>
                      <p className="mt-1 text-sm text-muted-foreground">
                        {t("accounts.oauthEditIntroDesc")}
                      </p>
                    </div>

                    <div className="rounded-xl border border-border bg-muted/30 px-4 py-3 text-sm">
                      <div className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
                        {t("accounts.oauthCurrentAccount")}
                      </div>
                      <div className="mt-1 font-semibold text-foreground">
                        {formatAccountName(editingAccount)}
                      </div>
                      {editingAccount.email ? (
                        <div className="mt-1 text-muted-foreground">
                          {editingAccount.email}
                        </div>
                      ) : null}
                    </div>

                    {renderProxyInput({
                      value: editOAuthProxyUrl,
                      onChange: setEditOAuthProxyUrl,
                      testKey: "edit-oauth",
                      label: t("accounts.oauthProxyUrl"),
                      placeholder: t("accounts.oauthProxyUrlPlaceholder"),
                      disabled: editOAuthGenerating || editOAuthUpdating,
                    })}

                    <div className="rounded-xl border border-border p-4">
                      <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
                        <div>
                          <div className="text-sm font-semibold text-foreground">
                            {t("accounts.oauthStep1Title")}
                          </div>
                          <p className="mt-1 text-sm text-muted-foreground">
                            {t("accounts.oauthStep1Desc")}
                          </p>
                        </div>
                        <Button
                          type="button"
                          variant={editOAuthStep === "exchange" ? "outline" : "default"}
                          className="shrink-0"
                          disabled={editOAuthGenerating || editOAuthUpdating}
                          onClick={() =>
                            void (editOAuthStep === "exchange"
                              ? handleEditOAuthRestart()
                              : handleEditOAuthGenerate())
                          }
                        >
                          <RefreshCw
                            className={`size-3.5 ${editOAuthGenerating ? "animate-spin" : ""}`}
                          />
                          {editOAuthGenerating
                            ? t("accounts.oauthGenerating")
                            : editOAuthStep === "exchange"
                              ? t("accounts.oauthRestart")
                              : t("accounts.oauthGenerateBtn")}
                        </Button>
                      </div>

                      {editOAuthSession ? (
                        <div className="mt-4 space-y-3">
                          <label className="block text-sm font-semibold text-muted-foreground">
                            {t("accounts.oauthAuthLinkLabel")}
                          </label>
                          <div className="flex flex-col gap-2 sm:flex-row">
                            <Input
                              className="min-w-0 flex-1 font-mono text-xs"
                              value={editOAuthSession.auth_url}
                              readOnly
                            />
                            <Button
                              type="button"
                              variant="outline"
                              className="shrink-0"
                              onClick={() => void handleEditOAuthCopyLink()}
                            >
                              <Copy className="size-3.5" />
                              {t("common.copy")}
                            </Button>
                            <Button
                              type="button"
                              variant="outline"
                              className="shrink-0"
                              onClick={() => window.open(editOAuthSession.auth_url, "_blank")}
                            >
                              <ExternalLink className="size-3.5" />
                              {t("accounts.oauthOpenLink")}
                            </Button>
                          </div>
                        </div>
                      ) : null}
                    </div>

                    <div className="rounded-xl border border-border p-4">
                      <div className="text-sm font-semibold text-foreground">
                        {t("accounts.oauthStep2Title")}
                      </div>
                      <p className="mt-1 text-sm text-muted-foreground">
                        {t("accounts.oauthStep2Desc")}
                      </p>
                      <div className="mt-3">
                        <label className="block mb-2 text-sm font-semibold text-muted-foreground">
                          {t("accounts.oauthCallbackUrlLabel")}
                        </label>
                        <Input
                          value={editOAuthCallbackUrl}
                          disabled={editOAuthUpdating}
                          placeholder={t("accounts.oauthCallbackUrlPlaceholder")}
                          onChange={(event: ChangeEvent<HTMLInputElement>) =>
                            setEditOAuthCallbackUrl(event.target.value)
                          }
                        />
                        <p className="mt-1.5 text-xs text-muted-foreground">
                          {t("accounts.oauthCallbackUrlHint")}
                        </p>
                      </div>
                    </div>
                  </div>
                ) : (
                  <>
                    ...existing scheduler settings...
                  </>
                )}
```

Keep the existing OpenAI Responses block and scheduler block contents unchanged; only add the OAuth middle branch.

- [ ] **Step 6: Run frontend typecheck**

Run:

```powershell
npm --prefix frontend run typecheck
```

Expected: PASS.

- [ ] **Step 7: Build the frontend**

Run:

```powershell
npm --prefix frontend run build
```

Expected: PASS. This catches JSX and translation JSON syntax issues that typecheck may not surface.

- [ ] **Step 8: Commit UI and i18n changes**

Run:

```powershell
git add frontend/src/pages/Accounts.tsx frontend/src/locales/zh.json frontend/src/locales/en.json
git commit -m "feat: add OAuth reauthorization UI"
```

Expected: commit succeeds with UI and locale changes.

---

## Task 7: Final integration verification

**Files:**
- Verify changed backend and frontend files.
- No code changes expected unless a command below fails.

- [ ] **Step 1: Run backend admin tests**

Run:

```powershell
go test ./admin -count=1
```

Expected: PASS.

- [ ] **Step 2: Run full backend tests**

Run:

```powershell
go test ./... -count=1
```

Expected: PASS. If unrelated pre-existing tests fail, record the exact package/test/output and continue with the targeted passing results only after confirming the failure is unrelated to OAuth edit changes.

- [ ] **Step 3: Run frontend typecheck**

Run:

```powershell
npm --prefix frontend run typecheck
```

Expected: PASS.

- [ ] **Step 4: Run frontend build**

Run:

```powershell
npm --prefix frontend run build
```

Expected: PASS.

- [ ] **Step 5: Inspect diff for sensitive token leakage and route correctness**

Run:

```powershell
git diff --check
git diff --stat
git diff -- admin/oauth.go admin/handler.go database/postgres.go frontend/src/pages/Accounts.tsx frontend/src/api.ts frontend/src/types.ts frontend/src/locales/zh.json frontend/src/locales/en.json
```

Expected:
- `git diff --check` prints no output.
- Diff includes `POST /accounts/:id/oauth/exchange-code`.
- Diff does not include response/log output of `refresh_token`, `access_token`, or `id_token` values.
- Existing `/oauth/exchange-code` and `/auth/callback` behavior remains add-account only.

- [ ] **Step 6: Final commit for any verification fixes**

If Task 7 required small fixes, run:

```powershell
git add admin/oauth.go admin/handler.go database/postgres.go frontend/src/pages/Accounts.tsx frontend/src/api.ts frontend/src/types.ts frontend/src/locales/zh.json frontend/src/locales/en.json
git commit -m "fix: polish OAuth account reauthorization"
```

Expected: commit is created only if Task 7 produced code changes. If Task 7 produced no code changes, skip this commit and report that no verification fixes were needed.

---

## Self-Review

### Spec coverage

- Edit OAuth accounts show an account settings tab: Task 5 and Task 6.
- Generate OAuth authorization link in edit modal: Task 5 and Task 6.
- Copy/open generated authorization link: Task 6.
- Manual callback URL paste and frontend parse of `code` / `state`: Task 5 and Task 6.
- Dedicated backend update endpoint: Task 1, Task 2, and Task 3.
- Existing add OAuth flow remains unchanged: Task 4 adds a new API method, Task 5 keeps add/edit state separate, Task 7 checks unchanged add endpoints.
- Existing `/auth/callback` remains add-account only: Task 3 adds a separate handler, Task 7 verifies route separation.
- No duplicate account on update: Task 1 success test asserts account count stays unchanged.
- Runtime store synchronization: Task 3 reloads the account, Task 1 success test asserts runtime tokens/proxy update.
- Sensitive token handling: Task 3 response only includes message/id/email/plan type, Task 7 checks no token values are logged or returned.

### Placeholder scan

The plan contains concrete file paths, code snippets, commands, and expected outcomes for every step. It does not leave unspecified implementation sections.

### Type consistency

- Backend handler name is consistently `UpdateOAuthAccountCode`.
- Backend DB method name is consistently `UpdateOAuthAccountCredentials`.
- Frontend request type is consistently `UpdateOAuthAccountRequest`.
- Frontend API method is consistently `updateOAuthAccount`.
- OAuth edit state uses the `editOAuth*` prefix and never reuses add-account `oauth*` state.
- Account type detection consistently uses `account.account_type === "oauth"`.
