# ADR 0005: No caching in v1

- Status: Accepted
- Date: 2026-10-31

## Context

Decision-model outputs are stochastic: the same question can return
different scores, choices and probabilities. A response cache would either
change observable behaviour (frozen answers where callers expect fresh
draws) or be deliberately unsound (occasionally serving someone else's
draw).

## Decision

Jevlar does not cache responses in v1. Every request goes upstream. The
seam for a future cache — model alias, request id, hashed body — exists in
the request path, but no cache is implemented, and none is added without a
superseding ADR that addresses stochasticity explicitly.

## Consequences

- Cost and latency optimisations must come from routing (fastest-policy
  selection), not from serving stored answers.
- No cache invalidation, TTL or memory-pressure problems exist in v1.
