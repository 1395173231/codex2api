# Tasks

- [x] Add Resin forward proxy URL builder and tests.
- [x] Switch Codex HTTP/compact/OpenAI Responses/WHAM/reset-credit/invite paths to use Resin forward proxy URL as effective proxy.
- [x] Switch OAuth token refresh/session fallback/code exchange from reverse URL rewriting to forward proxy.
- [x] Switch WebSocket upstream from Resin reverse WS URL to original WSS URL plus forward proxy.
- [x] Update tests that asserted reverse-proxy behavior.
- [x] Run targeted Go tests for `proxy`, `auth`, and `admin`.
