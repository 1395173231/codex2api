package database

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func newCodexTurnStateTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := New("sqlite", filepath.Join(t.TempDir(), "turn-states.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

func insertCodexTurnStateTestAccount(t *testing.T, db *DB, name string) int64 {
	t.Helper()
	id, err := db.InsertAccount(context.Background(), name, "refresh-"+name, "")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func codexTurnStateTestRecord(id int64, model, token string) CodexTurnStateRecord {
	now := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	return CodexTurnStateRecord{
		AccountID: id, Model: model, Identity: "identity", Token: token,
		IssuedAt: now, CapturedAt: now.Add(time.Second), ExpiresAt: now.Add(time.Hour),
		LastAttemptAt: now, NextAttemptAt: now.Add(55 * time.Minute), Attempts: 2,
	}
}

func requireCodexTurnStateRecord(t *testing.T, db *DB, id int64, model string) CodexTurnStateRecord {
	t.Helper()
	records, err := db.ListCodexTurnStates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range records {
		if rec.AccountID == id && rec.Model == model {
			return rec
		}
	}
	t.Fatalf("missing account=%d model=%s in %d records", id, model, len(records))
	return CodexTurnStateRecord{}
}

func TestCodexTurnStateSQLiteMigrationAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "restart.db")
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	id := insertCodexTurnStateTestAccount(t, db, "restart")
	// Recreate the feature tables on an existing account database, as on upgrade.
	for _, table := range []string{"codex_turn_states", "codex_turn_state_settings"} {
		if _, err := db.conn.ExecContext(ctx, "DROP TABLE "+table); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := db.LoadCodexTurnStateConfig(ctx)
	if err != nil || cfg != "{}" {
		db.Close()
		t.Fatalf("default config=%q err=%v", cfg, err)
	}
	cfg = `{"enabled":true,"models":["gpt-5.5"],"harvest_proxy_url":"http://user:password@proxy.invalid:8080"}`
	if err := db.SaveCodexTurnStateConfig(ctx, cfg); err != nil {
		db.Close()
		t.Fatal(err)
	}
	want := codexTurnStateTestRecord(id, "gpt-5.5", "persisted-token")
	saved, err := db.ReplaceCodexTurnState(ctx, want)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if saved.Revision != 1 {
		db.Close()
		t.Fatalf("first revision=%d", saved.Revision)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	gotCfg, err := db.LoadCodexTurnStateConfig(ctx)
	if err != nil || gotCfg != cfg {
		t.Fatalf("reopened config=%q err=%v", gotCfg, err)
	}
	got := requireCodexTurnStateRecord(t, db, id, want.Model)
	if got.Token != want.Token || got.Identity != want.Identity || got.Revision != saved.Revision || got.Attempts != want.Attempts || !got.IssuedAt.Equal(want.IssuedAt) || !got.ExpiresAt.Equal(want.ExpiresAt) || !got.NextAttemptAt.Equal(want.NextAttemptAt) {
		t.Fatalf("reopened ticket=%+v, want %+v", got, saved)
	}
}

func TestCodexTurnStateMigratesLegacyTableWithoutTokenHash(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	id := insertCodexTurnStateTestAccount(t, db, "legacy")
	if _, err := db.conn.ExecContext(ctx, `DROP TABLE codex_turn_states`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `CREATE TABLE codex_turn_states (
		account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
		model TEXT NOT NULL, payload TEXT NOT NULL DEFAULT '{}', revision BIGINT NOT NULL DEFAULT 0,
		lease_id TEXT NOT NULL DEFAULT '', lease_until BIGINT NOT NULL DEFAULT 0,
		PRIMARY KEY(account_id, model))`); err != nil {
		t.Fatal(err)
	}
	first := codexTurnStateTestRecord(id, "gpt-5.5", "legacy-shared-token")
	second := codexTurnStateTestRecord(id, "gpt-5.4", "legacy-shared-token")
	for _, rec := range []CodexTurnStateRecord{first, second} {
		payload, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO codex_turn_states(account_id,model,payload) VALUES($1,$2,$3)`, rec.AccountID, rec.Model, string(payload)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = New("sqlite", path)
	if err != nil {
		t.Fatalf("reopen legacy database: %v", err)
	}
	defer db.Close()
	columns, err := db.sqliteTableColumns(ctx, "codex_turn_states")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := columns["token_hash"]; !ok {
		t.Fatal("legacy table was not upgraded with token_hash")
	}
	kept := requireCodexTurnStateRecord(t, db, id, "gpt-5.4")
	removed := requireCodexTurnStateRecord(t, db, id, "gpt-5.5")
	if kept.Token != "legacy-shared-token" || removed.Token != "" || !removed.RefreshRequested {
		t.Fatalf("legacy duplicate migration kept=%+v removed=%+v", kept, removed)
	}
	if _, err := db.ReplaceCodexTurnState(ctx, codexTurnStateTestRecord(id, "gpt-5.5", kept.Token)); err == nil {
		t.Fatal("unique ticket index was not created after legacy migration")
	}
}

func TestCodexTurnStateAccountModelIsolation(t *testing.T) {
	db := newCodexTurnStateTestDB(t)
	ctx := context.Background()
	first := insertCodexTurnStateTestAccount(t, db, "first")
	second := insertCodexTurnStateTestAccount(t, db, "second")
	inputs := []CodexTurnStateRecord{
		codexTurnStateTestRecord(first, "gpt-5.5", "first-55"),
		codexTurnStateTestRecord(first, "gpt-5.4", "first-54"),
		codexTurnStateTestRecord(second, "gpt-5.5", "second-55"),
	}
	for _, input := range inputs {
		if _, err := db.ReplaceCodexTurnState(ctx, input); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range inputs {
		if got := requireCodexTurnStateRecord(t, db, want.AccountID, want.Model); got.Token != want.Token {
			t.Fatalf("ticket=%+v, want %+v", got, want)
		}
	}
	now := time.Now()
	claimed := make([]CodexTurnStateRecord, 0, len(inputs))
	for _, input := range inputs {
		rec, ok, err := db.ClaimCodexTurnState(ctx, input.AccountID, input.Model, now, time.Minute)
		if err != nil || !ok {
			t.Fatalf("claim account=%d model=%s ok=%v err=%v", input.AccountID, input.Model, ok, err)
		}
		claimed = append(claimed, rec)
	}
	if claimed[0].Lease == claimed[1].Lease || claimed[0].Lease == claimed[2].Lease {
		t.Fatal("independent model/account leases reused")
	}
	if _, err := db.ReplaceCodexTurnState(ctx, CodexTurnStateRecord{AccountID: first, Model: "gpt-5.5"}); err != nil {
		t.Fatal(err)
	}
	if got := requireCodexTurnStateRecord(t, db, first, "gpt-5.5"); got.Token != "" {
		t.Fatalf("tombstone retained ticket: %+v", got)
	}
	for _, want := range inputs[1:] {
		got := requireCodexTurnStateRecord(t, db, want.AccountID, want.Model)
		if got.Token != want.Token || got.Lease == "" {
			t.Fatalf("other account/model affected: %+v", got)
		}
	}
}

func TestCodexTurnStateLeaseExpiryAndOldRelease(t *testing.T) {
	db := newCodexTurnStateTestDB(t)
	ctx := context.Background()
	id := insertCodexTurnStateTestAccount(t, db, "lease")
	now := time.Now().Truncate(time.Second)
	first, ok, err := db.ClaimCodexTurnState(ctx, id, "gpt-5.5", now, time.Minute)
	if err != nil || !ok || first.Lease == "" {
		t.Fatalf("first claim=%+v ok=%v err=%v", first, ok, err)
	}
	if _, ok, err := db.ClaimCodexTurnState(ctx, id, "gpt-5.5", now.Add(59*time.Second), time.Minute); err != nil || ok {
		t.Fatalf("duplicate claim ok=%v err=%v", ok, err)
	}
	second, ok, err := db.ClaimCodexTurnState(ctx, id, "gpt-5.5", now.Add(time.Minute), time.Minute)
	if err != nil || !ok || second.Lease == first.Lease {
		t.Fatalf("expired recovery=%+v ok=%v err=%v", second, ok, err)
	}
	if err := db.ReleaseCodexTurnState(ctx, first); err != nil {
		t.Fatal(err)
	}
	if got := requireCodexTurnStateRecord(t, db, id, "gpt-5.5"); got.Lease != second.Lease || got.LeaseUntil != second.LeaseUntil {
		t.Fatalf("old release changed new lease: %+v", got)
	}
	first.Token = "stale-worker"
	if changed, err := db.CommitCodexTurnState(ctx, first); err != nil || changed {
		t.Fatalf("stale commit changed=%v err=%v", changed, err)
	}
	second.Token = "current-worker"
	if changed, err := db.CommitCodexTurnState(ctx, second); err != nil || !changed {
		t.Fatalf("current commit changed=%v err=%v", changed, err)
	}
	got := requireCodexTurnStateRecord(t, db, id, "gpt-5.5")
	if got.Token != second.Token || got.Lease != "" || got.LeaseUntil != 0 || got.Revision != second.Revision+1 {
		t.Fatalf("committed state=%+v", got)
	}
}

func TestCodexTurnStateReplaceFencesOldCommit(t *testing.T) {
	db := newCodexTurnStateTestDB(t)
	ctx := context.Background()
	id := insertCodexTurnStateTestAccount(t, db, "replace")
	initial, err := db.ReplaceCodexTurnState(ctx, codexTurnStateTestRecord(id, "gpt-5.5", "original"))
	if err != nil {
		t.Fatal(err)
	}
	old, ok, err := db.ClaimCodexTurnState(ctx, id, "gpt-5.5", time.Now(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	replacement := codexTurnStateTestRecord(id, "gpt-5.5", "new-manual-token")
	replacement.Lease, replacement.LeaseUntil = old.Lease, old.LeaseUntil
	saved, err := db.ReplaceCodexTurnState(ctx, replacement)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Revision != initial.Revision+1 || saved.Lease != "" || saved.LeaseUntil != 0 {
		t.Fatalf("replacement returned stale revision/lease: %+v", saved)
	}
	old.Token = "late-probe-token"
	if changed, err := db.CommitCodexTurnState(ctx, old); err != nil || changed {
		t.Fatalf("old commit changed=%v err=%v", changed, err)
	}
	got := requireCodexTurnStateRecord(t, db, id, "gpt-5.5")
	if got.Token != saved.Token || got.Revision != saved.Revision {
		t.Fatalf("new ticket overwritten: %+v", got)
	}
	saved.LastError = "revision check"
	if changed, err := db.CommitCodexTurnState(ctx, saved); err != nil || !changed {
		t.Fatalf("returned replacement revision unusable: changed=%v err=%v", changed, err)
	}
}

func TestCodexTurnStateRefreshPreservesNewestTicket(t *testing.T) {
	db := newCodexTurnStateTestDB(t)
	ctx := context.Background()
	id := insertCodexTurnStateTestAccount(t, db, "refresh")
	old := codexTurnStateTestRecord(id, "gpt-5.5", "old")
	if _, err := db.ReplaceCodexTurnState(ctx, old); err != nil {
		t.Fatal(err)
	}
	newest := codexTurnStateTestRecord(id, "gpt-5.5", "newest")
	saved, err := db.ReplaceCodexTurnState(ctx, newest)
	if err != nil {
		t.Fatal(err)
	}
	worker, ok, err := db.ClaimCodexTurnState(ctx, id, "gpt-5.5", time.Now(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	refreshed, err := db.RequestCodexTurnStateRefresh(ctx, id, "gpt-5.5")
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Token != newest.Token || refreshed.Identity != newest.Identity || !refreshed.IssuedAt.Equal(newest.IssuedAt) || !refreshed.ExpiresAt.Equal(newest.ExpiresAt) || !refreshed.RefreshRequested || !refreshed.NextAttemptAt.IsZero() || refreshed.Revision != saved.Revision+1 || refreshed.Lease != "" || refreshed.LeaseUntil != 0 {
		t.Fatalf("refresh damaged newest ticket or failed to reset due: %+v", refreshed)
	}
	worker.Token = "late-worker"
	if changed, err := db.CommitCodexTurnState(ctx, worker); err != nil || changed {
		t.Fatalf("superseded worker changed=%v err=%v", changed, err)
	}
	got := requireCodexTurnStateRecord(t, db, id, "gpt-5.5")
	if got.Token != newest.Token || !got.RefreshRequested || !got.NextAttemptAt.IsZero() {
		t.Fatalf("refresh not persisted: %+v", got)
	}
	missing, err := db.RequestCodexTurnStateRefresh(ctx, id, "gpt-5.4")
	if err != nil || !missing.RefreshRequested || missing.Token != "" || missing.Model != "gpt-5.4" {
		t.Fatalf("first refresh=%+v err=%v", missing, err)
	}
}
