package admin

import (
	"net/url"
	"strings"
	"testing"

	"github.com/codex2api/proxy"
)

func TestTemporaryResinProxyUsesStableFirstSeedWithoutLeakingIt(t *testing.T) {
	old := proxy.GetResinConfig()
	t.Cleanup(func() { proxy.SetResinConfig(old) })

	proxy.SetResinConfig(&proxy.ResinConfig{
		BaseURL:      "http://127.0.0.1:2260/my-token",
		PlatformName: "codex2api",
	})
	const refreshToken = "refresh-token-secret"
	identity, proxyURL := temporaryResinProxy("grok-credential", "http://legacy-proxy.example:8080", "", refreshToken, "access-token")
	if identity == "" || !strings.HasPrefix(identity, "temp-") {
		t.Fatalf("identity = %q, want non-empty temp identity", identity)
	}
	if strings.Contains(proxyURL, refreshToken) || strings.Contains(proxyURL, "access-token") {
		t.Fatalf("proxy URL leaks credential: %q", proxyURL)
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatalf("proxy URL is invalid: %v", err)
	}
	if parsed.User == nil || parsed.User.Username() != "codex2api."+identity {
		t.Fatalf("proxy username = %q, want codex2api.%s", parsed.User, identity)
	}
	if got, _ := parsed.User.Password(); got != "my-token" {
		t.Fatalf("proxy password = %q, want my-token", got)
	}

	repeated, _ := temporaryResinProxy("grok-credential", "", "", refreshToken)
	if repeated != identity {
		t.Fatalf("same first seed changed identity: %q vs %q", repeated, identity)
	}
}

func TestTemporaryResinProxyPreservesFallbackWhenResinDisabled(t *testing.T) {
	old := proxy.GetResinConfig()
	t.Cleanup(func() { proxy.SetResinConfig(old) })
	proxy.SetResinConfig(nil)

	const fallback = " http://legacy-proxy.example:8080 "
	identity, got := temporaryResinProxy("grok-device", fallback, "session-1")
	if identity == "" {
		t.Fatal("expected temporary identity even when Resin is disabled")
	}
	if got != strings.TrimSpace(fallback) {
		t.Fatalf("fallback proxy = %q, want %q", got, strings.TrimSpace(fallback))
	}
	if got := effectiveTemporaryResinProxy(identity, fallback); got != strings.TrimSpace(fallback) {
		t.Fatalf("effective fallback = %q, want %q", got, strings.TrimSpace(fallback))
	}
}
