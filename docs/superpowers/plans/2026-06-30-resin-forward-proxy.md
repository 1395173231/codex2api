# Resin Forward Proxy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Switch Resin-enabled account-bound outbound traffic from reverse proxy URL rewriting to forward proxy credentials while preserving sticky identity.

**Architecture:** Add one Resin helper that converts `resin_url + platform + account` into an HTTP forward proxy URL. Replace reverse-proxy branches at account-bound call sites with effective proxy URL selection, leaving original upstream URLs unchanged.

**Tech Stack:** Go `net/http`, Gorilla WebSocket, existing uTLS transports, existing Gin/admin settings.

---

### Task 1: Resin forward proxy helper

**Files:**
- Modify: `proxy/resin.go`
- Modify: `proxy/resin_test.go`

- [ ] Add tests for `BuildForwardProxyURL` simple and special-character identities.
- [ ] Run `go test ./proxy -run TestBuildForwardProxyURL -count=1` and confirm failure before implementation.
- [ ] Implement `BuildForwardProxyURL`, `BuildForwardProxyURLFromConfig`, and `EffectiveProxyURLForAccount`.
- [ ] Re-run the targeted tests.

### Task 2: Proxy package account traffic

**Files:**
- Modify: `proxy/executor.go`
- Modify: `proxy/usage_wham.go`
- Modify: `proxy/codex_invite.go`

- [ ] Add or update tests to assert Resin chooses forward proxy as effective proxy.
- [ ] Replace Codex reverse-proxy URL rewriting with `EffectiveProxyURLForAccount`.
- [ ] Ensure OpenAI Responses, WHAM usage/reset, and invite paths use `EffectiveProxyURLForAccount`.
- [ ] Re-run `go test ./proxy -count=1`.

### Task 3: OAuth/auth account traffic

**Files:**
- Modify: `auth/token.go`
- Modify: `auth/token_parse_test.go`
- Modify: `admin/oauth.go`
- Modify: `admin/oauth_test.go`
- Modify: `main.go`
- Modify: `admin/handler.go`

- [ ] Replace auth reverse decorator with Resin forward proxy decorator.
- [ ] Update OAuth code exchange to retain original token URL and use Resin proxy URL for temporary/stable identity.
- [ ] Keep `InheritLease` behavior unchanged.
- [ ] Re-run `go test ./auth ./admin -count=1`.

### Task 4: WebSocket forwarding

**Files:**
- Modify: `proxy/wsrelay/executor.go`
- Modify: `proxy/wsrelay/manager.go`
- Modify: `proxy/wsrelay/manager_test.go`

- [ ] Add test that Resin-enabled `effectiveProxyURL` returns Resin forward proxy URL.
- [ ] Stop rewriting WS URL to Resin reverse URL.
- [ ] Always configure Gorilla Dialer proxy when effective proxy URL is non-empty.
- [ ] Re-run `go test ./proxy/wsrelay -count=1`.

### Task 5: Final verification

**Files:**
- Modify: `openspec/changes/use-resin-forward-proxy/tasks.md`

- [ ] Run `gofmt` on touched Go files.
- [ ] Run `go test ./auth ./admin ./proxy ./proxy/wsrelay -count=1`.
- [ ] Search for remaining default Resin reverse-proxy request injections in account paths.
- [ ] Mark tasks complete.
