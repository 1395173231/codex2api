package proxy

import (
	"net/url"
	"testing"
)

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
