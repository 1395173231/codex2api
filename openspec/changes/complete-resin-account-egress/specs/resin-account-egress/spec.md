# Resin Account Egress Completion Spec

## ADDED Requirements

### Requirement: OpenAI OAuth token endpoint override

The system SHALL send OpenAI OAuth token refresh and authorization-code exchange
requests to `https://authproxy.eqing.tech/oauth/token`.

#### Scenario: authorization page remains upstream

Given an administrator starts OpenAI OAuth authorization
When the browser authorization URL is generated
Then its endpoint SHALL remain `https://auth.openai.com/oauth/authorize`.

### Requirement: saved accounts use Resin forward proxy

When Resin is enabled, the system MUST route every outbound request carrying a
saved account's credentials through a Resin HTTP forward proxy URL whose Proxy Auth username is
`<Platform>.<DBID>` and whose password is the Resin token.

#### Scenario: later-added Grok paths

Given Resin is enabled for saved Grok account `123`
When the system sends a Responses request, fetches models, fetches billing, or
refreshes the account token
Then the request SHALL use the original Grok/xAI HTTPS endpoint
And it SHALL establish the connection through Resin identity `123`.

#### Scenario: Agent Identity task registration

Given Resin is enabled for saved Agent Identity account `123`
When the system registers or refreshes its upstream task ID
Then the registration request SHALL establish the connection through Resin
identity `123`.

### Requirement: pre-save account flows use isolated temporary identities

When an account-bound request occurs before a DBID exists, the system SHALL use a
non-empty temporary Resin identity that is unique to that authorization session
or stable credential. Raw API keys, refresh tokens, SSO tokens, and access tokens
SHALL NOT appear in the identity.

#### Scenario: device authorization

Given Resin is enabled
When a Grok device authorization session starts and is polled
Then both requests SHALL use the same session-specific temporary identity
And a different authorization session SHALL use a different identity.

#### Scenario: credential-based model probe

Given Resin is enabled
When an unsaved Grok or OpenAI Responses credential fetches its model list
Then repeated probes with the same credential SHALL use the same hashed temporary
identity
And probes with different credentials SHALL use different identities.

### Requirement: temporary lease inheritance

When a pre-save flow creates a saved account, the system SHALL request Resin to
inherit the temporary lease into the new DBID identity.

#### Scenario: successful account creation

Given a pre-save request used temporary identity `temp-A`
When account `123` is created successfully
Then the system SHALL call Resin `inherit-lease` with parent `temp-A` and new
account `123`.
