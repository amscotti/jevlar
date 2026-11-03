# ADR 0006: System One wire format only, no chat adapter

- Status: Accepted
- Date: 2026-10-31

## Context

Jevlar fronts decision-model APIs that speak the System One wire format:
`POST /v1/systemone`, one small JSON request with typed questions, one JSON
response with choices, scores and probabilities, no streaming. Other
providers offer different envelopes (chat completions, Cloudflare's AI
Gateway format), and adapting between them is a real feature other gateways
offer.

## Decision

Jevlar speaks exactly one wire format: System One. There is no chat
endpoint, no streaming, and no protocol adaptation layer. Features that
require understanding model output beyond the usage block are out of scope;
the `model` splice (ADR 0003) is the only body inspection on the hot path.

## Consequences

- The request path stays thin: buffer, authenticate, route, splice at most
  one value, forward, buffer, commit.
- Providers with different envelopes (Cloudflare, Vercel) are documented as
  unsupported rather than half-supported.
- A future adapter is a new surface with its own ADR, not a retrofit of the
  proxy handler.
