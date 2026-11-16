# SDK compatibility suite

Proves that a Python application using the **official TypeSafe SDK**
(`typesafe-sdk`) works unchanged through Jevlar. The SDK is pointed at the
gateway with its own `base_url` option; nothing in the client is patched.

```sh
TYPESAFE_API_KEY=... mise run sdk-compat
GEMINI_API_KEY=...   mise run sdk-compat
# or put TYPESAFE_API_KEY and/or GEMINI_API_KEY in .env:
mise run sdk-compat
```

If `.env` exists in the repository root, the runner loads it automatically. At
least one of `TYPESAFE_API_KEY` or `GEMINI_API_KEY` must be set; the runner
builds Jevlar, generates a config with the configured provider(s) and two
client tokens (one with `rpm = 1` for the quota check), creates a throwaway
virtualenv, and runs `compat.py` with `JEVLAR_HAS_TYPESAFE` and
`JEVLAR_HAS_GEMINI`.

What it covers:

| Check | Scope | Proves |
|---|---|---|
| `models.list()` | TypeSafe | `/v1/models` matches the SDK's `ListModelsResponse` schema |
| `system_one()` noul + choice | TypeSafe | the request the SDK builds is forwarded and the typed response parses |
| `system_one()` score | TypeSafe | a second question type survives the round trip |
| provider error passthrough | TypeSafe | a provider 400 reaches the SDK as `TypeSafeBadRequestError` |
| 401 request id | Generic | Jevlar-generated errors carry `x-typesafe-request-id`, so `error.request_id` is populated |
| client rpm quota | TypeSafe | Jevlar's own 429 maps to `TypeSafeRateLimitError` with `Retry-After` |
| request-id headers | Generic | both Jevlar's id and the provider's id are observable |
| zstd compression | TypeSafe | an SDK-compressed request body is forwarded byte-for-byte |
| image upload (multipart+zstd, box) | TypeSafe | a real image upload with a box question returns coordinates matching the drawn rectangle |
| long-context danger flag | TypeSafe | a ~90 KB state with `TypeSafe-Danger-Zone-Long-Context: true` is accepted and billed for long context |
| `gemini models.list()` | Gemini | `gemini-decision-1` appears in `/v1/models` with `description` and `release_date` |
| `gemini system_one()` noul+choice | Gemini | translated request/response parses with `model == "gemini-decision-1"`, `input_tokens > 0`, `output_tokens == 0` |
| `gemini system_one()` NoulCriteria | Gemini | `true`/`false` `NoulCriteria` translates to `true_description`/`false_description` |
| `gemini system_one()` score | Gemini | `Score` questions translate and parse cleanly |
| `gemini system_one()` structured JSON state | Gemini | dictionary `state` is serialized cleanly to `input` |
| `gemini image upload (multipart+zstd, noul)` | Gemini | multipart+zstd image upload translates to inline base64 `input` (`usage.input_tokens > 500`) |
| `gemini provider error passthrough` | Gemini | Gemini 400 surfaces as `TypeSafeBadRequestError` with `request_id` |

This suite needs live API key(s) and network access, so it is not part of
`mise run ci`; run it before a release.
