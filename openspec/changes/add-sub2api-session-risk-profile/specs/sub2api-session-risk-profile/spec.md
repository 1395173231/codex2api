## ADDED Requirements

### Requirement: Sub2API header aliases
The gateway MUST accept `X-Sub2API-*` identity and policy-meta headers only when the resolved API-key binding has a `platform_code` equal to `sub2api` or prefixed by `sub2api-`/`sub2api_`.

#### Scenario: alias headers verify
- **WHEN** Sub2API sends a valid V1 signature and signed policy meta using `X-Sub2API-*` headers
- **THEN** the gateway MUST verify the request with the same API-key-bound secret and expose the verified user/platform/session metadata to prompt auditing

#### Scenario: conflicting namespaces
- **WHEN** canonical `X-NewAPI-*` and `X-Sub2API-*` headers for the same suffix are both present with different values
- **THEN** verification MUST fail without creating a verified person profile or policy decision

### Requirement: zero-score session observation
The gateway MUST create a `session_observed` risk event for a verified Sub2API request only when signed policy meta contains a valid `session_fingerprint`. The event MUST include privacy-preserving session/IP hashes and MAY include API-key metadata, but MUST have request risk score zero.

#### Scenario: clean session appears in profiles
- **WHEN** a verified Sub2API request has a valid session fingerprint
- **THEN** the risk profile list MUST contain the corresponding `newapi_user`, `session`, `api_key`, and `client_ip` subjects without increasing risk score

#### Scenario: observation deduplication
- **WHEN** the same API-key/user/session sends more requests within the configured session window
- **THEN** the gateway MUST avoid creating another observation until the window expires

#### Scenario: unverified request
- **WHEN** identity or policy meta verification fails
- **THEN** the gateway MUST NOT create a `newapi_user` observation and MUST preserve the existing authentication failure behavior
