package proxy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
)

func TestCompactionProvenanceTTL(t *testing.T) {
	t.Setenv(compactionAffinityTTLEnv, "36h")
	if got := compactionProvenanceTTL(); got != 36*time.Hour {
		t.Fatalf("compactionProvenanceTTL() = %v, want 36h", got)
	}

	t.Setenv(compactionAffinityTTLEnv, "invalid")
	if got := compactionProvenanceTTL(); got != defaultCompactionProvenanceTTL {
		t.Fatalf("invalid TTL = %v, want default %v", got, defaultCompactionProvenanceTTL)
	}
}

func TestAccountCompactionDomain(t *testing.T) {
	tests := []struct {
		name    string
		account *auth.Account
		want    string
	}{
		{name: "native codex", account: &auth.Account{DBID: 1, AccessToken: "at"}, want: nativeCodexCompactionDomain},
		{
			name: "responses relay canonicalizes endpoint",
			account: &auth.Account{
				DBID: 2, UpstreamType: auth.UpstreamOpenAIResponses,
				BaseURL: "HTTPS://Relay.Example.com/v1/?tenant=ignored", APIKey: "key",
			},
			want: "responses:https://relay.example.com/v1",
		},
		{
			name: "different relay stays isolated",
			account: &auth.Account{
				DBID: 3, UpstreamType: auth.UpstreamOpenAIResponses,
				BaseURL: "https://other.example.com/v1", APIKey: "key",
			},
			want: "responses:https://other.example.com/v1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := accountCompactionDomain(tt.account); got != tt.want {
				t.Fatalf("accountCompactionDomain() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRecordCompactionProvenanceStoresDigestOnly(t *testing.T) {
	tokenCache := cache.NewMemory(1)
	handler := &Handler{cache: tokenCache}
	account := &auth.Account{
		DBID: 42, UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL: "https://relay.example/v1", APIKey: "key",
	}
	const encrypted = "opaque-encrypted-state"

	if err := handler.recordCompactionProvenance(context.Background(), account, encrypted); err != nil {
		t.Fatalf("recordCompactionProvenance() error = %v", err)
	}
	digest := compactionContentDigest(encrypted)
	raw, ok, err := tokenCache.GetRuntime(context.Background(), compactionProvenanceCacheNamespace, digest)
	if err != nil || !ok {
		t.Fatalf("GetRuntime() = ok %v, err %v", ok, err)
	}
	if strings.Contains(string(raw), encrypted) {
		t.Fatalf("cache value leaked encrypted content: %s", raw)
	}

	record, err := decodeCompactionProvenanceRecord(raw)
	if err != nil {
		t.Fatalf("decodeCompactionProvenanceRecord() error = %v", err)
	}
	if record.AccountID != 42 || record.CompatibilityDomain != "responses:https://relay.example/v1" {
		t.Fatalf("unexpected record: %+v", record)
	}
}

func TestResolveCompactionAffinity(t *testing.T) {
	tokenCache := cache.NewMemory(1)
	handler := &Handler{cache: tokenCache}
	relay := &auth.Account{
		DBID: 7, UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL: "https://relay.example/v1", APIKey: "key",
	}
	if err := handler.recordCompactionProvenance(context.Background(), relay, "known"); err != nil {
		t.Fatal(err)
	}

	t.Run("known source resolves domain and producer", func(t *testing.T) {
		resolution, err := handler.resolveCompactionAffinity(context.Background(), []byte(`{"input":[{"type":"compaction","encrypted_content":"known"}]}`))
		if err != nil {
			t.Fatalf("resolveCompactionAffinity() error = %v", err)
		}
		if !resolution.Known || resolution.CompatibilityDomain != "responses:https://relay.example/v1" || resolution.PreferredAccountID != 7 {
			t.Fatalf("unexpected resolution: %+v", resolution)
		}
	})

	t.Run("all unknown preserves legacy scheduling", func(t *testing.T) {
		resolution, err := handler.resolveCompactionAffinity(context.Background(), []byte(`{"input":[{"type":"compaction","encrypted_content":"external"}]}`))
		if err != nil {
			t.Fatalf("resolveCompactionAffinity() error = %v", err)
		}
		if resolution.Known {
			t.Fatalf("unknown source unexpectedly resolved: %+v", resolution)
		}
	})

	t.Run("known plus unknown keeps known domain", func(t *testing.T) {
		resolution, err := handler.resolveCompactionAffinity(context.Background(), []byte(`{"input":[{"type":"compaction","encrypted_content":"external"},{"type":"compaction","encrypted_content":"known"}]}`))
		if err != nil || !resolution.Known {
			t.Fatalf("resolution = %+v, err = %v", resolution, err)
		}
	})

	t.Run("conflicting known domains are rejected", func(t *testing.T) {
		other := &auth.Account{DBID: 8, AccessToken: "native"}
		if err := handler.recordCompactionProvenance(context.Background(), other, "other"); err != nil {
			t.Fatal(err)
		}
		_, err := handler.resolveCompactionAffinity(context.Background(), []byte(`{"input":[{"type":"compaction","encrypted_content":"known"},{"type":"compaction","encrypted_content":"other"}]}`))
		if err == nil || !strings.Contains(err.Error(), "conflicting") {
			t.Fatalf("conflict error = %v", err)
		}
	})
}

func TestCompactionDomainFilter(t *testing.T) {
	wantDomain := "responses:https://relay.example/v1"
	filter := compactionDomainFilter(wantDomain, func(account *auth.Account) bool { return account.DBID != 99 })
	matching := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://relay.example/v1", APIKey: "key"}
	otherRelay := &auth.Account{DBID: 2, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://other.example/v1", APIKey: "key"}
	native := &auth.Account{DBID: 3, AccessToken: "at"}

	if !filter(matching) {
		t.Fatal("matching relay was rejected")
	}
	if filter(otherRelay) || filter(native) {
		t.Fatal("cross-domain account was accepted")
	}
}
