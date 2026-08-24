package auth

import "testing"

func TestTokenURLUsesAuthProxyEndpoint(t *testing.T) {
	const want = "https://authproxy.eqing.tech/oauth/token"
	if TokenURL != want {
		t.Fatalf("TokenURL = %q, want %q", TokenURL, want)
	}
}
