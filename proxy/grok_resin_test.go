package proxy

import (
	"context"
	"sync"
	"testing"

	"github.com/codex2api/auth"
)

func TestExecuteGrokRequestUsesResinForwardProxy(t *testing.T) {
	oldCfg := GetResinConfig()
	t.Cleanup(func() {
		// sync.Map contains a noCopy marker; reset it instead of copying/restoring
		// the value (which would fail go vet and is unnecessary between tests).
		clientPool = sync.Map{}
		SetResinConfig(oldCfg)
	})
	clientPool = sync.Map{}

	proxyServer := newCaptureConnectProxy(t)
	SetResinConfig(&ResinConfig{
		BaseURL:      proxyServer.urlWithToken("my-token"),
		PlatformName: "codex2api",
	})
	account := &auth.Account{
		DBID:         44,
		UpstreamType: auth.UpstreamGrok,
		BaseURL:      "https://grok.example/v1",
		AccessToken:  "grok-access-token",
	}

	resp, err := ExecuteGrokRequest(context.Background(), account, []byte(`{"model":"grok-4","input":"hello"}`), "http://legacy-proxy.example:8080", nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected capture proxy to reject the Grok request")
	}
	assertResinProxyConnect(t, proxyServer, "grok.example:443", "44", "my-token")
}

func TestFetchGrokModelIDsUsesResinForwardProxy(t *testing.T) {
	oldCfg := GetResinConfig()
	t.Cleanup(func() {
		clientPool = sync.Map{}
		SetResinConfig(oldCfg)
	})
	clientPool = sync.Map{}

	proxyServer := newCaptureConnectProxy(t)
	SetResinConfig(&ResinConfig{
		BaseURL:      proxyServer.urlWithToken("my-token"),
		PlatformName: "codex2api",
	})
	account := &auth.Account{
		DBID:         42,
		UpstreamType: auth.UpstreamGrok,
		BaseURL:      "https://grok.example/v1",
		AccessToken:  "grok-access-token",
	}

	if _, err := FetchGrokModelIDs(context.Background(), account, "http://legacy-proxy.example:8080"); err == nil {
		t.Fatal("expected capture proxy to reject the model request")
	}
	assertResinProxyConnect(t, proxyServer, "grok.example:443", "42", "my-token")
}

func TestFetchGrokBillingUsesResinForwardProxy(t *testing.T) {
	oldCfg := GetResinConfig()
	t.Cleanup(func() {
		clientPool = sync.Map{}
		SetResinConfig(oldCfg)
	})
	clientPool = sync.Map{}

	proxyServer := newCaptureConnectProxy(t)
	SetResinConfig(&ResinConfig{
		BaseURL:      proxyServer.urlWithToken("my-token"),
		PlatformName: "codex2api",
	})
	account := &auth.Account{
		DBID:         43,
		UpstreamType: auth.UpstreamGrok,
		BaseURL:      "https://grok.example/v1",
		AccessToken:  "grok-access-token",
	}

	if _, err := FetchGrokBilling(context.Background(), account, "http://legacy-proxy.example:8080"); err == nil {
		t.Fatal("expected capture proxy to reject billing requests")
	}
	assertResinProxyConnect(t, proxyServer, "grok.example:443", "43", "my-token")
	assertResinProxyConnect(t, proxyServer, "grok.example:443", "43", "my-token")
}
