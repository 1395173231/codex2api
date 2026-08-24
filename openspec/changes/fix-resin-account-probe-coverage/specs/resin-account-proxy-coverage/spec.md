# Resin Account Proxy Coverage Spec

## ADDED Requirements

### Requirement: Account-bound probe and inspection traffic honors Resin

When Resin is enabled, any upstream request that is made with a concrete account's ChatGPT credentials SHALL resolve its network proxy through `EffectiveProxyURLForAccount` immediately before creating the HTTP transport.

#### Scenario: Reset-credit inspection

Given Resin is enabled for account `123`
When the system queries `/backend-api/wham/rate-limit-reset-credits`
Then the request SHALL connect through the Resin forward proxy with identity `123`
And it SHALL NOT use the fallback account or global proxy.

#### Scenario: Model manifest inspection

Given Resin is enabled for account `123`
When the system fetches the account's Codex model manifest
Then the request SHALL connect through the Resin forward proxy with identity `123`.

#### Scenario: Standalone alpha search

Given Resin is enabled for account `123`
When the system forwards an account-authenticated Codex alpha-search request
Then the request SHALL connect through the Resin forward proxy with identity `123`.

#### Scenario: Saved OpenAI Responses account model discovery

Given Resin is enabled for an existing OpenAI Responses account `123`
When the administrator requests its model list using the saved account credentials
Then the request SHALL connect through the Resin forward proxy with identity `123`.

#### Scenario: Resin is disabled

Given Resin is disabled
When an account-bound inspection or auxiliary request is made
Then the request SHALL keep using its supplied fallback proxy behavior.

### Requirement: Invalid Resin settings cannot silently enable direct account traffic

The system SHALL validate a non-empty Resin configuration before enabling it. A valid configuration has a supported HTTP(S) proxy URL, host, token path, and platform name.

#### Scenario: Invalid settings update

Given Resin settings are changed through the administrator API
When the resulting Resin URL or platform is invalid or partial
Then the update SHALL be rejected
And the previous active Resin configuration SHALL remain unchanged.

#### Scenario: Invalid persisted settings on startup

Given persisted Resin settings are invalid or partial
When the service starts
Then it SHALL stop before serving account-bound upstream requests.

### Requirement: Token refresh observes a consistent Resin decorator

The token-refresh and session-fallback paths SHALL read the Resin proxy decorator through a synchronized accessor, and runtime settings updates SHALL write it through the paired synchronized setter.

#### Scenario: Runtime Resin settings update overlaps a token refresh

Given a token refresh or session fallback is resolving its account proxy
When the administrator enables, disables, or replaces the Resin settings
Then the refresh SHALL observe either the previous complete decorator or the replacement complete decorator
And it SHALL NOT race with a partially updated function reference.
