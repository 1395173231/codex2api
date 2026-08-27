# Message Hash Account Affinity Spec

## ADDED Requirements

### Requirement: privacy-preserving message fingerprints

The system SHALL derive bounded, deterministic fingerprints from eligible user
message text without persisting the plaintext.

#### Scenario: equivalent user text

Given two supported requests contain user text that differs only by case,
surrounding whitespace, or repeated whitespace
When message fingerprints are derived
Then the resulting fingerprint SHALL be identical.

#### Scenario: weak or non-user content

Given a request contains only short text, system instructions, assistant output,
tool output, images, or empty content
When message fingerprints are derived
Then those values SHALL NOT create message-affinity fingerprints.

### Requirement: exact affinity precedence

An exact session binding or stateful continuation binding MUST take precedence
over every message-overlap hint.

#### Scenario: exact binding disagrees with message history

Given the current session key is bound to account A
And its message hashes historically favor account B
When the request is scheduled
Then account A SHALL be attempted according to the existing exact-affinity rules
And the message history SHALL NOT override that binding.

### Requirement: evidence-gated smart selection

When no usable exact binding exists, the system SHALL require sufficient
independent evidence before preferring an account whose successful requests
overlap the current request's message hashes.

#### Scenario: sufficient overlap

Given at least two distinct current message hashes are bound to account A in the
same downstream API-key scope
And account A is in the current highest eligible scheduling layer
When a fresh session is scheduled
Then the system SHALL attempt account A before ordinary HRW or scheduler fallback.

#### Scenario: insufficient overlap

Given the cached observations do not meet the configured absolute and coverage
thresholds
When a fresh session is scheduled
Then the system SHALL use the existing fresh-affinity selection path.

#### Scenario: stale or ineligible account

Given cached message hashes favor an account that is excluded, unauthorized,
filtered, cooled down, concurrency-saturated, or outside the current highest
priority and health layer
When a fresh session is scheduled
Then the system SHALL NOT select that account from the message hint
And it SHALL continue with an eligible candidate or ordinary fallback.

### Requirement: successful bounded observations

The system SHALL record message-hash-to-account observations only after a
successful upstream response and SHALL expire those observations after a bounded
TTL.

#### Scenario: successful request

Given an upstream request completes successfully on account A
When the account slot is released
Then each eligible message hash SHALL reinforce account A in the downstream
API-key scope
And the records SHALL receive the message-affinity TTL.

#### Scenario: conflicting public hash

Given the same message hash repeatedly succeeds on different accounts
When the conflict threshold is reached
Then the hash SHALL stop participating in account voting until it expires.

### Requirement: shared cache with fail-open behavior

Redis-backed deployments SHALL share message-affinity observations across
instances, while cache errors MUST NOT make an otherwise schedulable request
fail.

#### Scenario: cross-instance match

Given instance A records successful message hashes for account A in Redis
When instance B receives a sufficiently overlapping request under the same
downstream API key
Then instance B SHALL be able to prefer account A subject to current eligibility.

#### Scenario: cache unavailable

Given the message-affinity cache read or write fails
When a request is selected or completed
Then the error SHALL be treated as a cache miss or best-effort write failure
And normal account scheduling and response handling SHALL continue.
