# ADR 0003: Byte passthrough with a single declared splice

- Status: Accepted
- Date: 2026-10-31

## Context

Providers interpret request bodies; any re-encoding (JSON unmarshal/marshal,
whitespace, key reordering, float formatting) risks changing semantics the
gateway does not know about. At the same time, routing may need to send a
different model id upstream than the alias the client asked for.

## Decision

The request body is buffered once and forwarded byte-for-byte with exactly
one declared splice: the value of the top-level `model` key, replaced only
when the routing target names a different model. The response body is never
re-encoded — it is buffered up to a cap and committed to the client only
after it has been fully read. v1 never streams a response, so a mid-flight
upstream failure can never produce a 200 with a truncated body.

## Consequences

- Byte integrity is provable: a test hashes the body received upstream and
  asserts equality, and the rewrite fuzz target asserts that replacing the
  spliced value restores the original bytes.
- A duplicate top-level `model` key is rejected 400 (providers disagree on
  which value wins); escaped keys are matched literally.
- Model ids are validated `[A-Za-z0-9._:-]+` so splicing cannot corrupt JSON.

## Addendum: what is deliberately *not* passed through

Three header-level deviations support the rest of the design. They are
decisions, not oversights:

1. **`Accept-Encoding: identity` is forced upstream.** Go's transport does not
   transparently decompress a response when the *client* supplied
   `Accept-Encoding`, so a gzipped upstream body would reach the usage parser
   as compressed bytes, silently reporting zero tokens forever. Requesting an
   uncompressed response keeps `usage` readable; `DisableCompression` on the
   pooled transport stops Go from adding its own `gzip`. The cost is that a
   client asking for gzip receives an uncompressed body.
2. **Hop-by-hop headers are stripped in both directions**, including any header
   named by `Connection`, per RFC 9110 §7.6.1.
3. **The client's `Authorization` never reaches a provider.** It is replaced by
   the selected provider key, or removed entirely for `auth = "none"`.

A response body above `server.max_response_bytes` aborts the attempt (502 once
no targets remain) rather than being streamed or truncated. Jevlar counts
`jevlar_response_too_large_total` when that happens.
