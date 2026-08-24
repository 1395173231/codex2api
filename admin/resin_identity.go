package admin

import (
	"strconv"
	"strings"

	"github.com/codex2api/proxy"
)

// temporaryResinProxy derives a stable, non-secret temporary identity from the
// first available seed and returns the effective Resin forward proxy URL. The
// raw seed is never embedded in Proxy-Auth credentials.
func temporaryResinProxy(namespace, fallbackProxyURL string, seeds ...string) (identity, proxyURL string) {
	stableValue := ""
	for _, seed := range seeds {
		if seed = strings.TrimSpace(seed); seed != "" {
			stableValue = seed
			break
		}
	}
	identity = proxy.TemporaryResinIdentity(namespace, stableValue)
	return identity, proxy.EffectiveProxyURLForIdentity(identity, fallbackProxyURL)
}

func effectiveTemporaryResinProxy(identity, fallbackProxyURL string) string {
	return proxy.EffectiveProxyURLForIdentity(strings.TrimSpace(identity), fallbackProxyURL)
}

// inheritTemporaryResinLease moves a pre-save lease to the stable DBID. Resin's
// control-plane call is intentionally asynchronous, matching the OpenAI OAuth
// account flow; account creation does not fail because of a transient control
// plane error.
func inheritTemporaryResinLease(identity string, accountID int64) {
	if !proxy.IsResinEnabled() || strings.TrimSpace(identity) == "" || accountID <= 0 {
		return
	}
	go proxy.InheritLease(identity, strconv.FormatInt(accountID, 10))
}

func grokCredentialResinIdentity(credentials map[string]interface{}) string {
	identity, _ := temporaryResinProxy(
		"grok-credential",
		"",
		credentialStringValue(credentials, "account_id"),
		credentialStringValue(credentials, "api_key"),
		credentialStringValue(credentials, "refresh_token"),
		credentialStringValue(credentials, "access_token"),
	)
	return identity
}
