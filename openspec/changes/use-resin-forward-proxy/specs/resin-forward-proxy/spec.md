# Resin Forward Proxy Spec

## ADDED Requirements

### Requirement: Resin forward proxy URL construction
When Resin is enabled, the system SHALL derive an HTTP proxy URL from `resin_url`, `resin_platform_name`, and a stable account identifier.

#### Scenario: simple identity
Given `resin_url` is `http://127.0.0.1:2260/my-token`
And `resin_platform_name` is `codex2api`
And account identity is `123`
Then the proxy URL SHALL be `http://codex2api.123:my-token@127.0.0.1:2260`.

#### Scenario: special characters in account identity
Given account identity contains characters such as `:`, `.`, or `@`
Then the proxy URL SHALL preserve the decoded Proxy Auth username as `<Platform>.<Account>` and password as the Resin token.

### Requirement: account-bound traffic uses forward proxy
When Resin is enabled and a request is bound to a concrete account or temporary OAuth identity, the request SHALL keep its original upstream URL and use the Resin forward proxy URL as the network proxy.

#### Scenario: Codex HTTP request
Given Resin is enabled
When a Codex `/responses` request is sent for account `123`
Then the request URL SHALL remain the original `https://chatgpt.com/backend-api/codex/responses`
And the HTTP client SHALL use the Resin forward proxy for account `123`
And `X-Resin-Account` SHALL NOT be injected.

#### Scenario: WebSocket request
Given Resin is enabled
When a Codex WebSocket connection is established for account `123`
Then the dial target SHALL remain the original `wss://...` URL
And the Gorilla WebSocket dialer SHALL use the Resin forward proxy for account `123`.

#### Scenario: OAuth temporary identity
Given Resin is enabled
When OAuth code exchange runs before a stable DB account exists
Then the token request SHALL use a unique temporary account identity derived from the OAuth session
And after successful account creation, `inherit-lease` SHALL be called from the temporary identity to the stable account identity.
