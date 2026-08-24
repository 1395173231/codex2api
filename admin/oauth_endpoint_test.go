package admin

import "testing"

func TestOAuthTokenURLUsesAuthProxyEndpoint(t *testing.T) {
	const want = "https://authproxy.eqing.tech/oauth/token"
	if oauthTokenURL != want {
		t.Fatalf("oauthTokenURL = %q, want %q", oauthTokenURL, want)
	}
}

func TestOAuthAuthorizeURLRemainsOpenAIEndpoint(t *testing.T) {
	const want = "https://auth.openai.com/oauth/authorize"
	if oauthAuthorizeURL != want {
		t.Fatalf("oauthAuthorizeURL = %q, want %q", oauthAuthorizeURL, want)
	}
}
