package database

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexTurnStateEncryptedAtRest(t *testing.T) {
	oldKey := credCipherKey()
	setCredEncryptionKeyForTest("turn-state-test-key")
	t.Cleanup(func() { credKey = oldKey })
	db, err := New("sqlite", filepath.Join(t.TempDir(), "encrypted-tickets.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "test", map[string]any{"access_token": "fake"}, "")
	if err != nil {
		t.Fatal(err)
	}
	config := `{"harvest_proxy_url":"http://private-user:private-pass@localhost:9"}`
	if err := db.SaveCodexTurnStateConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReplaceCodexTurnState(ctx, CodexTurnStateRecord{AccountID: id, Model: "model", Token: "private-ticket"}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`SELECT config FROM codex_turn_state_settings`, `SELECT payload FROM codex_turn_states`} {
		var raw string
		if err := db.conn.QueryRowContext(ctx, query).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(raw, "private-") || !strings.HasPrefix(raw, credEncPrefix) {
			t.Fatal("secret was stored unencrypted")
		}
	}
	loaded, err := db.LoadCodexTurnStateConfig(ctx)
	if err != nil || loaded != config {
		t.Fatal("config decryption failed")
	}
	tickets, err := db.ListCodexTurnStates(ctx)
	if err != nil || len(tickets) != 1 || tickets[0].Token != "private-ticket" {
		t.Fatal("ticket decryption failed")
	}
}
