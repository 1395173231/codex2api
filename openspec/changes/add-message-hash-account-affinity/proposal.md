# add-message-hash-account-affinity

## Background

Session affinity currently uses an explicit downstream session identifier or a
single content-derived seed. This works when clients preserve those identifiers
or resend an unchanged conversation prefix, but a shared downstream API key can
still lose account affinity when request envelopes change, system/tool content
drifts, or only an overlapping message window is sent. The resulting account
rotation reduces upstream prompt-cache reuse.

The existing Redis-backed session binding remains the authoritative exact match.
What is missing is a weaker, overlap-based hint that can recognize a conversation
from several stable user messages without storing their plaintext.

## Goal

Derive bounded, normalized hashes from user message text and persist successful
message-hash-to-account observations in the configured runtime cache. When a
request has no usable exact session binding, use overlap voting to prefer a
currently eligible account before falling back to the existing fresh-affinity
HRW or scheduler path.

Scope observations by downstream API key, batch Redis reads, update Redis records
atomically, and preserve the memory cache implementation for single-process
deployments and tests.

## Non-goals

- Do not replace explicit session affinity, continuation pinning, or the existing
  content-derived session key.
- Do not store raw message text or expose message-derived material in logs.
- Do not let a historical match bypass API-key permissions, request filters,
  account health, cooldown, priority, or concurrency gates.
- Do not require Redis; cache misses and cache errors continue through the normal
  scheduler path.
- Do not change upstream `Session_id` or `prompt_cache_key` generation.

## Risk and rollback

- Risk: common prompts can create false account affinity. Mitigation: ignore short
  text, use only user content, require multiple distinct overlaps (or strong
  repeated evidence for a single hash), and stop reinforcing repeatedly
  conflicting hashes.
- Risk: a stale cache record can point at an unavailable or unauthorized account.
  Mitigation: voting is limited to the same highest-priority healthy candidate
  layer used by fresh-affinity selection, and acquisition rechecks cooldown and
  concurrency.
- Risk: Redis latency can affect fresh-session selection. Mitigation: use one
  bounded MGET, one bounded atomic record operation, and fail open on errors.
- Rollback: revert this change; exact session affinity and HRW scheduling remain
  independently intact.

## Acceptance

- Eligible OpenAI/Responses-style and message-style user text produces stable,
  deduplicated hashes without retaining plaintext.
- A fresh request with sufficient message overlap selects the previously
  successful eligible account within the same downstream API-key scope.
- Exact session bindings and stateful continuations take precedence over message
  hints.
- Unavailable, filtered, lower-priority, or excluded accounts are never selected
  solely because of a message-hash record.
- Redis-backed deployments share observations across instances; memory-backed
  deployments retain equivalent local behavior.
- Cache errors and insufficient evidence fall back to existing selection without
  rejecting the request.

