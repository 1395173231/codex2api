# Tasks

- [x] Add normalized user-message hash extraction for supported request shapes.
- [x] Add optional memory and Redis message-affinity cache capabilities with TTL and conflict handling.
- [x] Apply message-overlap voting only to fresh session selection within the eligible top candidate layer.
- [x] Record message-affinity observations only after successful upstream responses.
- [x] Add focused tests for hashing, cross-instance sharing, precedence, filtering, conflicts, and cache failure fallback.
- [x] Run focused Go tests, the full Go suite, race checks for touched packages, and strict OpenSpec validation.
