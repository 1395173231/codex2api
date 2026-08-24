# fix-resin-account-probe-coverage

## Background

Resin is the account-bound egress boundary for requests that carry ChatGPT account credentials. Most usage and recovery probes already resolve an effective Resin forward proxy at the transport boundary, but the reset-credit inspection path and later-added account-bound auxiliary endpoints can still construct a transport from the ordinary per-account/global proxy URL.

That creates a sticky-IP and egress-identity mismatch when Resin is enabled. In the worst case, an account credential request can leave through a direct or legacy proxy route rather than its Resin lease.

## Goal

Ensure every audited account-bound probe, inspection, and auxiliary request resolves the Resin effective proxy immediately before constructing its HTTP transport. Reject malformed Resin configuration before it can be activated, and make the token-refresh decorator's hot update synchronized. Add regression tests that prove the affected paths issue an authenticated CONNECT request to the Resin forward proxy.

## Non-goals

- Do not route system-level metadata sync, proxy-pool health checks, or a pre-save model lookup without a stable account identity through Resin.
- Do not change account scheduling, request payloads, upstream URLs, or Resin lease identity format.
- Do not change the existing behavior when Resin is disabled.

## Risk and rollback

- Risk: applying Resin to an endpoint that is not actually account-bound could make an operator's system request depend on Resin availability. The change is limited to endpoints that use a concrete `auth.Account` or an existing account's stable DB identity and send its credentials.
- Risk: invalid persisted Resin settings will now prevent startup rather than silently allowing direct account traffic. This is intentional fail-closed behavior; operators can clear or correct the setting to recover.
- Risk: a regression test that only checks URL construction would miss transport use. Tests therefore inspect the HTTP CONNECT proxy-auth handshake.
- Rollback: revert the affected transport-resolution lines and their tests; no schema or persisted configuration changes are introduced.

## Acceptance

- With Resin enabled, reset-credit inspection requests use the Resin forward proxy for the account rather than the caller's fallback proxy.
- With Resin enabled, account-bound model-manifest and standalone alpha-search requests use the same Resin forward proxy.
- With Resin enabled, a saved OpenAI Responses account's model-discovery request uses its stable Resin identity.
- The proxy CONNECT handshake carries `codex2api.<DBID>` and the Resin token as Basic credentials.
- With Resin disabled, all paths retain their existing fallback-proxy behavior.
- Invalid or partial Resin settings are rejected before activation, and startup fails before serving account requests if persisted Resin settings are invalid.
