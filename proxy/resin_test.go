package proxy

import (
	"net/url"
	"strings"
	"testing"

	"github.com/codex2api/auth"
)

// Legacy URL builder coverage: production traffic uses forward CONNECT now.
func TestBuildReverseProxyURL(t *testing.T) {
	// 保存并恢复原始配置
	old := resinCfg.Load()
	defer func() { resinCfg.Store(old) }()

	SetResinConfig(&ResinConfig{
		BaseURL:      "http://127.0.0.1:2260/my-token",
		PlatformName: "codex2api",
	})

	tests := []struct {
		name      string
		targetURL string
		want      string
	}{
		{
			name:      "HTTPS codex responses",
			targetURL: "https://chatgpt.com/backend-api/codex/responses",
			want:      "http://127.0.0.1:2260/my-token/codex2api/https/chatgpt.com/backend-api/codex/responses",
		},
		{
			name:      "HTTPS codex responses compact",
			targetURL: "https://chatgpt.com/backend-api/codex/responses/compact",
			want:      "http://127.0.0.1:2260/my-token/codex2api/https/chatgpt.com/backend-api/codex/responses/compact",
		},
		{
			name:      "HTTPS auth token URL",
			targetURL: "https://authproxy.eqing.tech/oauth/token",
			want:      "http://127.0.0.1:2260/my-token/codex2api/https/authproxy.eqing.tech/oauth/token",
		},
		{
			name:      "URL with query params",
			targetURL: "https://api.example.com/healthz?foo=bar",
			want:      "http://127.0.0.1:2260/my-token/codex2api/https/api.example.com/healthz?foo=bar",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildReverseProxyURL(tt.targetURL)
			if got != tt.want {
				t.Fatalf("BuildReverseProxyURL(%q)\n  got:  %q\n  want: %q", tt.targetURL, got, tt.want)
			}
		})
	}
}

func TestBuildForwardProxyURL(t *testing.T) {
	old := resinCfg.Load()
	defer func() { resinCfg.Store(old) }()

	SetResinConfig(&ResinConfig{
		BaseURL:      "http://127.0.0.1:2260/my-token",
		PlatformName: "codex2api",
	})

	got := BuildForwardProxyURL("123")
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("BuildForwardProxyURL returned invalid URL %q: %v", got, err)
	}
	if parsed.Scheme != "http" {
		t.Fatalf("scheme = %q, want http", parsed.Scheme)
	}
	if parsed.Host != "127.0.0.1:2260" {
		t.Fatalf("host = %q, want 127.0.0.1:2260", parsed.Host)
	}
	if parsed.User == nil {
		t.Fatal("proxy URL missing userinfo")
	}
	if username := parsed.User.Username(); username != "codex2api.123" {
		t.Fatalf("username = %q, want codex2api.123", username)
	}
	if password, _ := parsed.User.Password(); password != "my-token" {
		t.Fatalf("password = %q, want my-token", password)
	}
	if parsed.Path != "" {
		t.Fatalf("path = %q, want empty path", parsed.Path)
	}
}

func TestBuildForwardProxyURLPreservesSpecialAccountIdentity(t *testing.T) {
	old := resinCfg.Load()
	defer func() { resinCfg.Store(old) }()

	SetResinConfig(&ResinConfig{
		BaseURL:      "http://proxy.local:2260/token-value",
		PlatformName: "Default",
	})

	got := BuildForwardProxyURL("user.name:with@special")
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("BuildForwardProxyURL returned invalid URL %q: %v", got, err)
	}
	if parsed.Host != "proxy.local:2260" {
		t.Fatalf("host = %q, want proxy.local:2260", parsed.Host)
	}
	if username := parsed.User.Username(); username != "Default.user.name:with@special" {
		t.Fatalf("username = %q, want special account identity preserved", username)
	}
	if password, _ := parsed.User.Password(); password != "token-value" {
		t.Fatalf("password = %q, want token-value", password)
	}
}

func TestBuildWebSocketURL(t *testing.T) {
	old := resinCfg.Load()
	defer func() { resinCfg.Store(old) }()

	SetResinConfig(&ResinConfig{
		BaseURL:      "http://127.0.0.1:2260/my-token",
		PlatformName: "codex2api",
	})

	tests := []struct {
		name      string
		targetURL string
		want      string
	}{
		{
			name:      "WSS codex responses",
			targetURL: "wss://chatgpt.com/backend-api/codex/responses",
			want:      "ws://127.0.0.1:2260/my-token/codex2api/https/chatgpt.com/backend-api/codex/responses",
		},
		{
			name:      "WS target",
			targetURL: "ws://local.dev/ws",
			want:      "ws://127.0.0.1:2260/my-token/codex2api/http/local.dev/ws",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildWebSocketURL(tt.targetURL)
			if got != tt.want {
				t.Fatalf("BuildWebSocketURL(%q)\n  got:  %q\n  want: %q", tt.targetURL, got, tt.want)
			}
		})
	}
}

func TestIsResinEnabled(t *testing.T) {
	old := resinCfg.Load()
	defer func() { resinCfg.Store(old) }()

	// 禁用状态
	SetResinConfig(nil)
	if IsResinEnabled() {
		t.Fatal("expected Resin disabled when config is nil")
	}

	// 空 URL
	SetResinConfig(&ResinConfig{BaseURL: "", PlatformName: "test"})
	if IsResinEnabled() {
		t.Fatal("expected Resin disabled when BaseURL is empty")
	}

	// 启用状态
	SetResinConfig(&ResinConfig{BaseURL: "http://localhost:2260/tk", PlatformName: "test"})
	if !IsResinEnabled() {
		t.Fatal("expected Resin enabled")
	}
}

func TestValidateResinConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *ResinConfig
		wantErr bool
	}{
		{name: "nil disables Resin", cfg: nil},
		{name: "both fields blank disables Resin", cfg: &ResinConfig{}},
		{name: "valid HTTP forward proxy", cfg: &ResinConfig{BaseURL: "http://127.0.0.1:2260/my-token", PlatformName: "codex2api"}},
		{name: "partial config", cfg: &ResinConfig{BaseURL: "http://127.0.0.1:2260/my-token"}, wantErr: true},
		{name: "missing token", cfg: &ResinConfig{BaseURL: "http://127.0.0.1:2260", PlatformName: "codex2api"}, wantErr: true},
		{name: "unsupported proxy scheme", cfg: &ResinConfig{BaseURL: "socks5://127.0.0.1:2260/my-token", PlatformName: "codex2api"}, wantErr: true},
		{name: "query is not a token", cfg: &ResinConfig{BaseURL: "http://127.0.0.1:2260/?token=my-token", PlatformName: "codex2api"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateResinConfig(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateResinConfig(%+v) error = %v, wantErr %t", tt.cfg, err, tt.wantErr)
			}
		})
	}
}

func TestSetResinConfigInvalidDisablesResin(t *testing.T) {
	old := GetResinConfig()
	t.Cleanup(func() { SetResinConfig(old) })

	SetResinConfig(&ResinConfig{BaseURL: "http://127.0.0.1:2260", PlatformName: "codex2api"})
	if IsResinEnabled() {
		t.Fatal("invalid Resin configuration must not remain enabled")
	}
}

func TestBuildReverseProxyURL_Disabled(t *testing.T) {
	old := resinCfg.Load()
	defer func() { resinCfg.Store(old) }()

	SetResinConfig(nil)

	target := "https://chatgpt.com/backend-api/codex/responses"
	got := BuildReverseProxyURL(target)
	if got != target {
		t.Fatalf("expected passthrough when disabled, got %q", got)
	}
}

func TestTemporaryResinIdentityIsStableAndNonSecret(t *testing.T) {
	const secret = "sk-test-secret-value"
	first := TemporaryResinIdentity("openai-models", secret)
	second := TemporaryResinIdentity("openai-models", secret)
	if first == "" || first != second {
		t.Fatalf("identity is not stable: first=%q second=%q", first, second)
	}
	if !strings.HasPrefix(first, "temp-") {
		t.Fatalf("identity = %q, want temp- prefix", first)
	}
	if strings.Contains(first, secret) {
		t.Fatalf("identity contains raw secret: %q", first)
	}
	if other := TemporaryResinIdentity("other-flow", secret); other == first {
		t.Fatalf("different namespaces must not share identity %q", first)
	}
	if got := TemporaryResinIdentity("openai-models", ""); got != "" {
		t.Fatalf("empty stable value identity = %q, want empty", got)
	}
}

func TestResinAccountIDRejectsUnsavedAccounts(t *testing.T) {
	for name, account := range map[string]*auth.Account{
		"nil":      nil,
		"zero":     {DBID: 0},
		"negative": {DBID: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ResinAccountID(account); got != "" {
				t.Fatalf("ResinAccountID = %q, want empty", got)
			}
		})
	}
	if got := ResinAccountID(&auth.Account{DBID: 42}); got != "42" {
		t.Fatalf("ResinAccountID(42) = %q, want 42", got)
	}
}

func TestEffectiveProxyURLForAccountFallsBackWithoutDBID(t *testing.T) {
	old := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(old) })
	SetResinConfig(&ResinConfig{BaseURL: "http://127.0.0.1:2260/token", PlatformName: "codex2api"})

	const fallback = " http://legacy-proxy.example:8080 "
	if got := EffectiveProxyURLForAccount(&auth.Account{}, fallback); got != strings.TrimSpace(fallback) {
		t.Fatalf("unsaved account proxy = %q, want trimmed fallback", got)
	}
	if got := EffectiveProxyURLForAccount(&auth.Account{DBID: 42}, fallback); !strings.Contains(got, "codex2api.42") {
		t.Fatalf("saved account proxy = %q, want Resin identity", got)
	}
}
