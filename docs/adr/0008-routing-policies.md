# ADR 0008: Routing policies and the attempt budget

- Status: Accepted
- Date: 2026-11-01

## Context

An alias may name several `(provider, model)` targets. The gateway has to
decide which to try first, how many attempts a request may make, and how a
target that names a different model is reached.

## Decision

**Priority** (default) tries targets in configuration order. **Fastest**
ranks them by an exponentially weighted moving average of upstream
round-trip time, with three properties that make it safe to leave on:

1. **Only working answers are sampled.** A 2xx or a request-shaped rejection
   (400/404/409/422) proves the provider works; 429, 401, 403, 5xx and
   transport failures are excluded so a fast failure cannot attract traffic.
2. **Hysteresis.** The leader changes only when a challenger is faster by
   more than `hysteresis_ms`, which stops two providers a millisecond apart
   from alternating.
3. **Deterministic exploration, no goroutine.** A provider with no sample is
   tried before a known provider is repeated (otherwise a slow incumbent is
   never displaced), and every `explore_every`-th request rotates a
   non-leader to the front. Selection is a counter, not a background loop,
   so behaviour is reproducible in tests.

Telemetry lives in the process-lifetime health registry, so a reload does
not erase what the gateway has learned about its providers.

**Attempt budget.** The target order is computed once, before the first
attempt, so a policy change mid-request cannot revisit a failed target.
Attempt *i* gets `min(provider.timeout, remaining / n)` where
`n = 1 + min(max_attempts − i, targets − i)`, floored at 100 ms; below the
floor the request ends 504 rather than burning a doomed attempt.

**Model rewrite.** A target may name a different model than the client
asked for. Exactly one JSON value is replaced — the top-level `model`
string — and every other byte is copied through. Model ids are restricted
to `[A-Za-z0-9._:-]+` at boot so a splice can never need escaping.

**X-Provider** pins a request to one target of the alias. It is validated
against the alias's own targets: an unknown name is 400, never a silent
fallback, so a pin cannot widen the routing table.

## Consequences

- Failover is invisible to clients and observable in `X-Jevlar-Attempts`,
  `jevlar_attempts_total` and the structured request log.
- 400/409/422 never fail over: the request is wrong, not the provider.
  404 does fail over, because a target that does not serve a model is
  exactly the reason to have another target.
- The byte-integrity invariant is a fuzz target: rewriting and restoring
  the model value must reproduce the original body hash.
