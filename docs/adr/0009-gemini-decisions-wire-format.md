# ADR 0009: Gemini Decisions wire format adapter

- Status: Accepted
- Date: 2026-11-16

## Context

Google AI Studio exposes `gemini-decision-1` at
`https://generativelanguage.googleapis.com/v1beta/decisions`. It is a
System-1, zero-output-token decision model whose `noul`, `choice`, and
`score` answer structures match System One, so callers using the official
TypeSafe SDK can consume its answers without changing application code.

However, its HTTP wire format differs from System One in five ways:

1. **Authentication header.** Google expects `X-Goog-Api-Key: <key>` rather
   than `Authorization: Bearer <key>`.
2. **Request body shape.** Gemini expects `input` (a string or array of
   `{"type":"text","text":...}` / `{"type":"image","data":...,"mime_type":...}`
   parts) instead of `state` and `assets`, and expects text strings rather than
   arbitrary JSON objects for structured `state`, `instructions`, and criteria.
   `noul` criteria keys are `true_description` and `false_description` rather
   than `true` and `false`. Image parts must be inline base64 rather than
   `multipart/form-data` binary parts.
3. **Response shape.** Gemini omits top-level `model` in the response body and
   reports `usage.total_input_tokens` / `usage.total_output_tokens` rather
   than `usage.input_tokens` / `usage.output_tokens`.
4. **Invalid-key error shape.** Google returns HTTP 400 (or 403) with a Google
   RPC error payload whose `error.details[].reason` is `API_KEY_INVALID` or
   `API_KEY_EXPIRED`, rather than HTTP 401.
5. **Multimodal scope.** `gemini-decision-1` supports `noul`, `choice`, and
   `score` over images, whereas `box` questions are TypeSafe-specific and
   unsupported.

## Decision

1. **Explicit provider configuration.** Add `providers[].auth = "x-goog-api-key"`
   (alongside `"bearer"` and `"none"`) and
   `providers[].wire_format = "gemini-decisions"` (default `"systemone"`).
   Providers using `"systemone"` keep 100% of ADR 0003 byte passthrough.
2. **Isolated stdlib-only adapter.** Isolate all Gemini request translation,
   response normalization, and invalid-key error detection in a new
   standard-library-only package `internal/wire` per ADR 0006, preserving
   ADR 0001's dependency budget.
3. **Request translation (`TranslateGeminiRequest`).** Decode JSON or
   `multipart/form-data` requests from the immutable `inspectBody`, rewrite
   `model` to the selected target model, map `state` and `assets` (both
   multipart parts and inline `data_base64`) into `input`, rename `noul`
   criteria `true`/`false` to `true_description`/`false_description`, compact
   non-string JSON `state`/`instructions`/criteria to strings, and preserve
   unknown top-level fields and `box` questions so upstream validation
   behavior remains authentic.
4. **Response and usage normalization.** `NormalizeGeminiResponse` populates
   missing `model` with the target model id and fills `usage.input_tokens` /
   `usage.output_tokens` from `total_input_tokens` / `total_output_tokens` so
   TypeSafe SDK clients decode responses cleanly; `internal/usage` applies the
   same fallback when recording token metrics and budgets.
5. **Credential handling and error classification.** Strip inbound
   `X-Goog-Api-Key` alongside `Authorization` before injecting the selected
   provider's credential. Treat `IsGeminiInvalidKeyError` (`API_KEY_INVALID` or
   `API_KEY_EXPIRED` in Google RPC error details on 400/403, plus 401) as a
   key-disabling auth failure instead of passing a 400 through as a client
   error.

## Consequences

- **Mixed-wire failover works transparently.** Because `inspectBody` stays
  immutable across attempts, a single `[models.<alias>]` can route between a
  `"systemone"` provider (pure byte passthrough or `model`-only splice) and a
  `"gemini-decisions"` provider (translated per attempt) without cross-attempt
  mutation.
- **`box` remains TypeSafe-specific.** If a `box` question is routed to a
  Gemini target, it is left untouched and Gemini's HTTP 400 passes through to
  the client unchanged.
- **Inbound `X-Goog-Api-Key` is stripped.** Client headers cannot leak upstream
  even if a caller sends `X-Goog-Api-Key` directly.
- **Core proxy remains thin.** Protocol adaptation is confined to
  `internal/wire`, leaving the default System One path free of JSON
  re-encoding.
