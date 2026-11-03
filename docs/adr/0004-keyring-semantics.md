# ADR 0004: Account-scoped limits, key-scoped health, state that survives reload

- Status: Accepted
- Date: 2026-10-31

## Context

Provider rate limits apply to the account, not the individual API key. A key
pool therefore does not multiply capacity: one key's 429 means the account's
other keys will 429 too. Separately, keys get revoked (401) and flagged
(403), and operators reload configuration without wanting to reset a
cooldown or resurrect a revoked key.

## Decision

The health registry models both scales: per-key state (in-flight, cooldown,
disabled) and per-account state (token bucket, cooldown), plus per-provider
breakers. A 429 cools both the key and its account for the `retry-after`
window; 401 disables the key until an operator acts (a reload never
re-enables it); 403 cools for a fixed window and counts the key as suspect.
Selection is an O(1) power-of-two choice among healthy keys, and the
account's rpm/tpm budget is checked before a key is selected. The registry
is created once at boot and reconciled — never replaced — on reload:
entries whose key still exists keep their state.

## Consequences

- The "three attempts, three 429s" pattern that follows from ignoring
  account-scoped limits is designed out.
- Reload continuity is asserted by an integration test: disable a key,
  reload, and assert it stays disabled.
- Exhaustion (every key cooling or disabled) returns 429 or 503 with
  `retry-after` set to the earliest expiry.
