# use-resin-forward-proxy

## Background
The project already supports Resin configuration through `resin_url` and `resin_platform_name`, but current account-bound Codex/OAuth traffic primarily uses Resin reverse proxy URL rewriting plus `X-Resin-Account`.

Reverse proxy mode terminates TLS between this service and Resin, so the project's local uTLS / Chrome TLS fingerprint is not preserved to the final upstream. This breaks request classes that depend on client TLS fingerprint fidelity.

## Goal
Switch Resin-enabled account-bound outbound requests to Resin forward proxy mode by default, preserving existing account identity stickiness while allowing standard and uTLS transports to perform TLS handshakes through an HTTP CONNECT tunnel.

## Non-goals
- Do not remove reverse proxy helper functions; keep them available for future/manual use.
- Do not add new UI settings beyond existing `resin_url` and `resin_platform_name`.
- Do not change account selection, scheduling, affinity, or rate-limit policies.

## Risk and rollback
- Risk: incorrectly constructed Proxy Auth credentials could break all Resin-enabled account traffic.
- Risk: WebSocket proxy behavior depends on Gorilla Dialer HTTP proxy support.
- Rollback: revert this change to restore reverse proxy URL rewriting.

## Acceptance
- Given `resin_url=http://127.0.0.1:2260/my-token`, `resin_platform_name=codex2api`, and account `123`, Resin forward proxy URL resolves to `http://codex2api.123:my-token@127.0.0.1:2260`.
- Resin-enabled Codex HTTP, compact, OpenAI Responses, WHAM usage/reset, OAuth refresh/exchange, session fallback, and WebSocket calls use original upstream URLs and set Resin via forward proxy credentials.
- Resin-enabled request paths no longer add `X-Resin-Account` by default.
- OAuth temporary identities still call `inherit-lease` after account creation/update.
