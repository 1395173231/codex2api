# complete-resin-account-egress

## Background

The existing Resin integration already sends Codex, OAuth, WebSocket, and the
original maintenance paths through an authenticated HTTP forward proxy. Later
account types and auxiliary flows were added after that integration, including
Grok traffic and Codex Agent Identity task registration. Several of those paths
still construct clients from the ordinary account/global proxy URL and can
bypass Resin when Resin is enabled.

Pre-save account probes and authorization flows also need an identity before a
database account ID exists. Reusing one fixed placeholder would collapse
unrelated accounts onto the same Resin lease; sending a raw credential as the
proxy username would expose secrets unnecessarily.

## Goal

Make Resin the account egress boundary for every audited account-bound request,
including later-added Grok and Agent Identity paths. Use Resin forward proxy
credentials so the application still performs the upstream TLS handshake and
preserves its uTLS fingerprint. Use deterministic, non-secret temporary
identities before account creation and inherit their leases into the stable
database account ID after creation.

Keep the OpenAI OAuth token exchange endpoint pinned to
`https://authproxy.eqing.tech/oauth/token` while leaving the authorization page
on `https://auth.openai.com/oauth/authorize`.

## Non-goals

- Do not merge the full `backup/retry-before-upstream-20260824` branch; it
  contains unrelated feature and refactor work.
- Do not remove the legacy Resin reverse-proxy helper functions.
- Do not change account scheduling, payload translation, model mapping, or
  rate-limit behavior.
- Do not route system-level metadata synchronization or proxy health checks
  through an account lease.

## Risk and rollback

- Risk: central proxy resolution can accidentally double-transform a proxy URL.
  Mitigation: Resin forward proxy selection is idempotent for a saved account,
  and temporary accounts with no DB ID preserve an explicitly supplied proxy.
- Risk: temporary identities can drift between probe and create operations.
  Mitigation: derive them from the same stable credential fingerprint or stored
  authorization session ID.
- Rollback: revert this change; the earlier Codex/OAuth Resin integration remains
  independently intact.

## Acceptance

- OpenAI OAuth token refresh and code exchange use
  `https://authproxy.eqing.tech/oauth/token`.
- Saved Grok Responses, model-list, billing, token-refresh, and Agent Identity
  task-registration requests use Resin forward proxy credentials based on DBID.
- Pre-save Grok/OpenAI Responses probes and Grok authorization/import flows use
  deterministic non-secret temporary Resin identities.
- Account creation inherits a temporary lease into the new DBID when applicable.
- Original upstream HTTPS URLs remain unchanged; no production request is
  rewritten to a Resin reverse-proxy path.
