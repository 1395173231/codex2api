package proxy

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func TestCodexTurnStateHarvestUsesDedicatedFreshProxyConnections(t *testing.T) {
	raw := make([]byte, 217)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(time.Now().Unix()))
	token := base64.URLEncoding.EncodeToString(raw)
	addresses := make(map[string]bool)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		addresses[r.RemoteAddr] = true
		if r.URL.Host != "upstream.invalid" || r.Header.Get("Authorization") != "Bearer local-test-token" || r.Header.Get("Chatgpt-Account-Id") != "local-workspace" {
			t.Error("harvest used incorrect endpoint/account")
		}
		if !r.Close || r.Header.Get(codexTurnStateHeader) != "" || r.Header.Get("X-Resin-Account") != "" {
			t.Error("probe reused a ticket or did not close its connection")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["model"] != "test-model" || body["stream"] != true {
			t.Error("invalid harvest request")
		}
		w.Header().Set(codexTurnStateHeader, token)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	a := &auth.Account{DBID: 10, AccessToken: "local-test-token", AccountID: "local-workspace", CustomHeaders: map[string]string{codexTurnStateHeader: "stale"}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		got, err := harvestCodexTurnStateAt(ctx, a, "test-model", server.URL, "http://upstream.invalid/responses")
		if err != nil || got != token {
			t.Fatalf("harvest: %v", err)
		}
	}
	if calls != 2 || len(addresses) != 2 {
		t.Fatal("harvest reused a connection")
	}
	transport, err := newCodexHarvestTransport(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !transport.DisableKeepAlives || transport.ForceAttemptHTTP2 || transport.TLSNextProto == nil {
		t.Fatal("harvest enabled pooled HTTP/2 transport")
	}
}

func TestCodexTurnStateManagedHTTPAndWebsocketInjection(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "inject.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err := db.InsertAccountWithCredentials(ctx, "inject", map[string]any{"access_token": "fake-at", "plan_type": "plus"}, "")
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, nil)
	defer store.Stop()
	if err := store.LoadAccountByID(ctx, id); err != nil {
		t.Fatal(err)
	}
	m, err := auth.NewCodexTurnStateManager(ctx, db, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := m.Config()
	cfg.Enabled = true
	cfg.Models = []string{"model-a", "model-b"}
	cfg.HarvestProxyURL = "http://localhost:9"
	if err := m.SaveConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	a := store.FindByID(id)
	raw := make([]byte, 217)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(time.Now().Unix()))
	ticketA := base64.URLEncoding.EncodeToString(raw)
	raw[9] = 2
	ticketB := base64.URLEncoding.EncodeToString(raw)
	for model, token := range map[string]string{"model-a": ticketA, "model-b": ticketB} {
		if err := m.Replace(ctx, a, model, token); err != nil {
			t.Fatal(err)
		}
	}
	for _, ws := range []bool{false, true} {
		attempt, body, headers := prepareCodexTurnStateInjection(WithCodexClientModel(ctx, "model-a"), a, []byte(`{"model":"model-b"}`), nil, ws)
		if headers.Get(codexTurnStateHeader) != ticketB || CodexTurnStateInjectionFromContext(attempt) != ticketB {
			t.Fatal("client model ticket leaked into outbound model")
		}
		if ws && gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String() != ticketB {
			t.Fatal("websocket frame lost its model ticket")
		}
		if err := m.Replace(ctx, a, "model-b", ""); err != nil {
			t.Fatal(err)
		}
		attempt, _, headers = prepareCodexTurnStateInjection(attempt, a, body, headers, ws)
		if headers.Get(codexTurnStateHeader) != "" || CodexTurnStateInjectionFromContext(attempt) != "" {
			t.Fatal("reused context retained deleted ticket")
		}
		if err := m.Replace(ctx, a, "model-b", ticketB); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCodexTurnStateHarvestMetadataAndFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"metadata", 200, `data: {"type":"response.metadata","response":{"current_turn_state":"test-state"}}`, "test-state"},
		{"rate limit", 429, `{"error":"private upstream detail"}`, ""},
		{"no state", 200, `{"output":[]}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body + "\n"))
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			got, err := harvestCodexTurnStateAt(ctx, &auth.Account{DBID: 1, AccessToken: "fake"}, "model", server.URL, "http://upstream.invalid/responses")
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("got=%q err=%v", got, err)
			}
			if err != nil && strings.Contains(err.Error(), "private") {
				t.Fatal("upstream detail leaked")
			}
		})
	}
	for _, url := range []string{"", "https://user:secret@proxy.invalid/path", "file:///private"} {
		if _, err := newCodexHarvestTransport(url); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid proxy accepted or leaked credentials")
		}
	}
}
