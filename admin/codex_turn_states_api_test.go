package admin

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func turnStateAPI(t *testing.T) (*Handler, *gin.Engine, int64) {
	t.Helper()
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	id, err := db.InsertAccountWithCredentials(context.Background(), "ticket-test", map[string]any{"access_token": "fake-test-token", "account_id": "workspace-test", "plan_type": "team"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.LoadAccountByID(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	manager, err := auth.NewCodexTurnStateManager(context.Background(), db, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db, store: store, codexTurnStates: manager}
	r := gin.New()
	r.GET("/settings", h.GetCodexTurnStateSettings)
	r.PUT("/settings", h.UpdateCodexTurnStateSettings)
	r.GET("/accounts/:id/turn-states", h.GetAccountTurnStates)
	r.PUT("/accounts/:id/turn-states", h.UpdateAccountTurnState)
	r.DELETE("/accounts/:id/turn-states", h.DeleteAccountTurnState)
	r.POST("/accounts/:id/turn-states/refresh", h.RefreshAccountTurnState)
	return h, r, id
}

func turnStateAPICall(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func TestCodexTurnStateSettingsMaskHotReloadAndValidation(t *testing.T) {
	h, r, _ := turnStateAPI(t)
	w := turnStateAPICall(r, http.MethodPut, "/settings", `{"enabled":true,"harvest_proxy_url":"socks5h://private-user:private-password@localhost:1080","models":["model-a","model-b"]}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), "private-") {
		t.Fatalf("unsafe settings response: %d %s", w.Code, w.Body.String())
	}
	var cfg struct {
		auth.CodexTurnStateSettings
		Configured bool `json:"proxy_configured"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil || !cfg.Enabled || !cfg.Configured {
		t.Fatal("settings not enabled")
	}
	w = turnStateAPICall(r, http.MethodPut, "/settings", w.Body.String())
	if w.Code != 200 || !strings.Contains(h.codexTurnStates.Config().HarvestProxyURL, "private-password") {
		t.Fatal("masked resave lost proxy secret")
	}
	for _, body := range []string{`{"concurrency":17}`, `{"models":["*"]}`, `{"ttl_seconds":180,"refresh_before_seconds":180}`, `{"clear_proxy":true}`, `{"harvest_proxy_url":"file:///sensitive"}`} {
		if got := turnStateAPICall(r, http.MethodPut, "/settings", body); got.Code != 400 {
			t.Fatalf("invalid config accepted: %s", body)
		}
	}
	w = turnStateAPICall(r, http.MethodPut, "/settings", `{"enabled":false,"clear_proxy":true}`)
	if w.Code != 200 || h.codexTurnStates.Config().HarvestProxyURL != "" {
		t.Fatal("explicit proxy clear failed")
	}
}

func TestCodexTurnStateModelAPIPersistsTeamTokenWithoutExposingIt(t *testing.T) {
	h, r, id := turnStateAPI(t)
	if w := turnStateAPICall(r, http.MethodPut, "/settings", `{"enabled":true,"harvest_proxy_url":"http://localhost:9","models":["model-a","model-b"]}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	raw := make([]byte, 249)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(time.Now().Add(-30*time.Minute).Unix()))
	token := base64.URLEncoding.EncodeToString(raw)
	path := fmt.Sprintf("/accounts/%d/turn-states", id)
	body, _ := json.Marshal(map[string]string{"model": "model-a", "token": token})
	w := turnStateAPICall(r, http.MethodPut, path, string(body))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = turnStateAPICall(r, http.MethodGet, path, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), token) {
		t.Fatal("status exposed raw ticket")
	}
	var result struct {
		Items []auth.CodexTurnStateStatus `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 2 || !result.Items[0].Ready || result.Items[0].TargetLength != 332 || result.Items[0].RemainingSeconds > 1800 || result.Items[1].Ready {
		t.Fatalf("bad per-model summary: %+v", result.Items)
	}
	if w := turnStateAPICall(r, http.MethodPost, path+"/refresh", `{"model":"model-a"}`); w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	if w := turnStateAPICall(r, http.MethodDelete, path, `{"model":"model-a"}`); w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	if h.store.FindByID(id).CodexTurnStateInjection("model-a") != "" {
		t.Fatal("deleted ticket still injected")
	}
}
