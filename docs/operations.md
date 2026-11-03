# Operations

This is the runbook for running Jevlar. It assumes the configuration
structure shown in `configs/example.toml` and the metrics described in the
README.

## Startup expectations

Jevlar **fails closed at boot**. Any configuration error prints every
problem with its field path and exits 2 before a socket is bound:

```
$ jevlar --check --config /etc/jevlar/config.toml
jevlar: configuration is invalid: providers[2].api_keys[0].value: variable "UPSTAGE_API_KEY" is not set
```

`--check` runs the identical pipeline without binding, so it belongs in
deployment validation and CI.

Exit codes: `0` success, `1` runtime/startup failure (for example a port
already in use), `2` configuration or usage error.

## Health and readiness

| Endpoint | Meaning |
|---|---|
| `GET /healthz` | the process is up (always 200 while serving) |
| `GET /readyz` | at least one configured provider has a usable key |
| `GET /metrics` | Prometheus exposition |
| `GET /admin/status` | redacted runtime state (admin listener) |
| `POST /admin/reload` | reload the configuration in place |
| `GET /admin/usage` | in-memory usage totals since boot |
| `GET /admin/usage?window=24h` | totals from durable rollups |

Wire the load balancer to `/readyz`, not `/healthz`: a gateway with no
usable key is up but cannot serve.

The admin listener is loopback-only unless `server.admin_token` is set. In
a container, either keep it on loopback and exec into the container, or
set a token of at least 32 characters and protect it like any credential.

## Reloading

Two triggers, one pipeline:

- `SIGHUP` (`systemctl reload jevlar`, `docker kill -s HUP <id>`)
- `POST /admin/reload`

The pipeline reads the file, expands `${VAR}`, decodes strictly, validates,
builds a snapshot, reconciles health state and swaps. On any failure the
old snapshot keeps serving, the failure is logged, and
`jevlar_config_reload_failures_total` increments.

**What survives a reload:** key disabled state (a 401 stays disabled until
an operator re-enables it), key and account cooldowns, breaker state,
client quotas and routing telemetry.

**What does not change:** listener addresses and `usage.*` settings.
Changing `server.listen` or `server.admin_listen` needs a restart; a reload
logs a warning saying so. The durable rollup writer and the
`/admin/usage?window=` reader are fixed at boot, so changing
`usage.enabled`, `usage.dir`, `usage.retention_days` or `usage.max_file_mb`
also needs a restart (a reload warns).

## Image and long-context requests

Image support is a gated provider beta. Three things must line up:

1. The client sends `TypeSafe-Danger-Zone-Images: true` (Jevlar forwards it).
2. The request uses a `box` question or an `assets` entry.
3. The routed model supports vision — on TypeSafe that is `jev-preview`,
   not `jev-latest`.

`TypeSafe-Danger-Zone-Long-Context: true` works the same way for long inputs.

Size the body cap for the traffic you expect:

```toml
[server]
max_body_bytes = 8388608   # 8 MiB
max_inflight   = 128
```

Worst-case buffering is roughly `max_body_bytes × max_inflight`, so raise one
and lower the other. A request over the cap is refused with 413 before any
upstream contact.

**Text-only providers silently drop images.** Ollama's System One models
accept the request, ignore the `assets` entirely, and return an answer whose
token count is unchanged — indistinguishable from the same request without
the image. Put vision aliases on their own model name so this cannot happen
by accident.

## Key rotation

1. Add the new key alongside the old one in `providers[].api_keys` and
   reload. New requests may use either key.
2. Once traffic has moved, remove the old key and reload.

A key that returns 401 is disabled automatically and *stays* disabled
across reloads. Re-enabling requires removing and re-adding it (or a
process restart) after fixing the credential — a deliberate choice: the
gateway will not silently retry a revoked key.

**403 is not 401.** Providers also use 403 for quota and plan limits, so a
403 cools the key for 60 seconds and increments
`jevlar_key_suspect_total` instead of disabling it.

## Account limits are per account, not per key

This is the most common operational surprise. If several keys belong to
the same `[accounts.<name>]` group, they share the provider's limits: one
429 cools the whole account for the upstream's `Retry-After`. Adding a
second key to the same account does **not** add capacity.

Use separate accounts (or separate provider entries) when the upstream
limit really is per credential.

## Reading the metrics

| Symptom | Look at |
|---|---|
| Clients see 429s | `jevlar_client_limited_total{reason="rpm"\|"tpm"}`, `jevlar_requests_total{status="429"}`, `Retry-After` on the response |
| Clients see 503s | `jevlar_breaker_state{state=2}`, `jevlar_requests_total{status="503"}`, `/admin/status` `breakers` |
| One provider is slower | `jevlar_request_duration_milliseconds`, `/admin/status` key and breaker sections |
| Tokens look wrong | `jevlar_usage_missing_total` — a rising counter means the provider changed its response shape |
| Reloads failing | `jevlar_config_reload_failures_total` and the log line carrying the field paths |

Dashboard minimum: requests by status class, p50/p99 latency, breaker
state, key state, client limited, and usage missing.

## When every key is cooling

The client gets **429 `capacity_exhausted`** with a `Retry-After` equal to
the earliest cooldown expiry, or **503 `capacity_exhausted`** when the
cause is concurrency rather than rate limits.

1. Check `/admin/status`: which keys are cooling, and until when?
2. If all keys of an account are cooling, the account hit its provider
   limit. Adding keys to the same account will not help.
3. If the account has `rpm`/`tpm` set, compare with
   `jevlar_requests_total` for that provider.
4. To shed load deliberately, lower `server.max_inflight` and reload.

## 401 storms

A 401 disables one key and fails over; it does not take the gateway down.
If *every* key is disabled, `/readyz` goes 503 and clients see 503
`capacity_exhausted`.

- Confirm the credential is valid outside Jevlar first.
- Fix the credential, then remove and re-add the key entry (reload alone
  will not clear the disabled state).

## Usage and rollups

With `usage.enabled = true`, Jevlar writes one JSONL line per hour to
`usage.dir`, fsyncs it, rotates at `max_file_mb`, and prunes files older
than `retention_days` (never the active file).

- A clean shutdown flushes the current interval.
- A `SIGKILL` loses at most the interval in progress.
- Rollups contain counts only: no request state, no question text, no key
  material. They are safe to ship to a log pipeline.

`GET /admin/usage?window=7d` (maximum 90 days) sums the rollups.

## Container and systemd notes

- The image runs as `nonroot` with a read-only root filesystem; mount the
  usage directory writable (`/data`).
- The systemd unit raises `LimitNOFILE=65536` because Jevlar holds one
  upstream connection per concurrency slot plus client sockets.
- Both rely on `shutdown_grace_ms` for a clean drain: if you reduce it,
  in-flight requests are cut at the grace deadline.

## Tuning checklist

| Knob | Raise when | Lower when |
|---|---|---|
| `max_concurrency` (provider) | upstream allows more parallelism | you see 429s blamed on concurrency |
| `max_inflight` (server) | clients queue outside Jevlar | memory grows with request size × concurrency |
| `request_timeout_ms` | upstream genuinely slow | clients time out first |
| `timeout_ms` (provider) | a provider is legitimately slow | you want faster failover |
| `max_response_bytes` | responses carry large answer payloads | memory is tight |
| `hysteresis_ms` | routing flaps between providers | you want faster convergence |
