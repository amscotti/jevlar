# SDK compatibility suite

Proves that a Python application using the **official TypeSafe SDK**
(`typesafe-sdk`) works unchanged through Jevlar. The SDK is pointed at the
gateway with its own `base_url` option; nothing in the client is patched.

```sh
TYPESAFE_API_KEY=... mise run sdk-compat
```

The runner builds Jevlar, starts it in front of the real TypeSafe API with
two client tokens (one with `rpm = 1` for the quota check), creates a
throwaway virtualenv, and runs `compat.py`.

What it covers:

| Check | Proves |
|---|---|
| `models.list()` | `/v1/models` matches the SDK's `ListModelsResponse` schema |
| `system_one()` noul + choice | the request the SDK builds is forwarded and the typed response parses |
| `system_one()` score | a second question type survives the round trip |
| provider error passthrough | a provider 400 reaches the SDK as `TypeSafeBadRequestError` |
| 401 request id | Jevlar-generated errors carry `x-typesafe-request-id`, so `error.request_id` is populated |
| client rpm quota | Jevlar's own 429 maps to `TypeSafeRateLimitError` with `Retry-After` |
| request-id headers | both Jevlar's id and the provider's id are observable |
| zstd compression | an SDK-compressed request body is forwarded byte-for-byte |
| image upload (multipart+zstd, box) | a real image upload with a box question returns coordinates matching the drawn rectangle |
| long-context danger flag | a ~90 KB state with `TypeSafe-Danger-Zone-Long-Context: true` is accepted and billed for long context |

This suite needs the real API key and network access, so it is not part of
`mise run ci`; run it before a release.
