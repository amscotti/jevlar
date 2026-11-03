# Changelog

All notable changes to Jevlar are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-11-02

First release. Scope is deliberately narrow: one wire format, four
features (proxy, fallback, usage, key management), no chat, no streaming,
no database, no UI.

### Added

- The complete v1 configuration schema: `server` (admin listener and
  token, request/shutdown/header timeouts, body and response caps,
  in-flight limit, log level and body logging), `security` (strict file
  permissions), `usage` (rollup settings), `auth` (inbound client keys
  with hashed tokens and rpm/tpm quotas), `accounts` (per-account provider
  limits), `providers` (timeouts, concurrency caps, model lists, key
  pools with `value`/`value_file`), `models` (alias routing with ordered
  targets, priority/fastest policy, attempt and EWMA tuning) and
  `breaker`. Every field is validated with aggregated, field-path errors;
  `configs/example.toml` shows the full surface.
- `${VAR}` expansion covers map-keyed tables (`accounts`, `models`); map
  keys themselves are never expanded.
- Outbound provider keys can be loaded from files (`api_keys[].value_file`,
  re-read on every reload) with a file-permission policy: group/world-
  accessible secret files warn by default and are rejected under
  `security.strict_file_permissions = true`; the same policy applies to
  the configuration file itself. Literal inline key values warn at load.
  A trailing LF or CRLF is trimmed from secret files.
- `jevlar keyhash <token>` prints the SHA-256 hex for `[auth] keys[].hash`.
- `internal/runtime`: the immutable serving snapshot (`Runtime`, provider
  views, key specs, model table) built from a validated configuration,
  plus a configuration hash that covers the resolved configuration —
  comments and formatting do not change it — and excludes secret material.
- The request path (`internal/proxy`): `POST /v1/systemone` with inbound
  bearer authentication, bounded body reading, top-level `model` scanning,
  an attempt loop with a per-attempt deadline budget, byte-preserving
  forwarding and commit-after-read response buffering. Jevlar never
  streams or truncates a response: an over-cap upstream body aborts the
  attempt.
- Model rewrite: the top-level `model` value is spliced when a target names
  a different model, with every other byte preserved. Model ids are
  validated to `[A-Za-z0-9._:-]+` at boot so a splice can never need
  escaping. Covered by a corpus test and fuzz targets that assert
  restoring the value reproduces the original body.
- `X-Provider` pinning, validated against the alias's own targets
  (unknown name → 400, open breaker → 503).
- Failover by outcome: 5xx, timeouts, 429, 401/403 and 404 move to the
  next target; 400/409/422 pass through unchanged and never fail over;
  when every target returns 404 the last one is passed through.
- `GET /v1/models` (sorted union of aliases and provider model ids),
  `/readyz` (ready when at least one provider can serve), `/healthz`, and
  405 for wrong methods on `/v1` routes.
- Per-provider pooled transports and concurrency slots owned by a
  process-lifetime registry that survives reloads: `MaxConnsPerHost`,
  `MaxIdleConnsPerHost`, 90 s idle timeout, HTTP/2 enabled, and a
  non-blocking slot pool that rejects rather than queuing.
- Process-lifetime health state (`internal/health`): per-key cooldown and
  disable state, per-provider circuit breakers with a single half-open
  probe that re-opens on failure, per-account rpm/tpm buckets with
  account-wide cooldown, and per-client quota buckets. `Reconcile` maps
  configured identities onto existing entries, so a reload can never
  resurrect a 401-disabled key or reset a 429 cooldown.
- Key selection (`internal/keyring`): power-of-two choice among healthy
  keys with a bounded fallback scan, deferred release, and the earliest
  cooldown expiry for `Retry-After` on exhaustion.
- Outcome policy in the request path: 429 cools the key (honouring
  `Retry-After`, otherwise jittered exponential backoff capped at 30 s) and,
  when the provider is grouped, the whole account; 401 disables the key
  until an operator re-enables it; 403 cools for 60 s and increments
  `jevlar_key_suspect_total` instead of disabling, because 403 also means
  quota. A key-scoped failure retries the same target with another key
  before moving on.
- Routing policies: `priority` (configuration order) and `fastest` (EWMA
  upstream latency with hysteresis, deterministic exploration, and no
  background goroutine). Latency telemetry survives reloads, and only
  responses that prove a provider works are sampled.
- `docs/adr/0008-routing-policies.md`.
- Usage accounting (`internal/usage`): read-only parsing of
  `usage.input_tokens`, `usage.output_tokens` and the optional
  `usage.cost`, attributed per client, provider, key and model. A response
  without a usage block increments `jevlar_usage_missing_total`; tokens are
  never guessed.
- Client budgets: `rpm` is enforced before any upstream contact, `tpm` is
  charged post-hoc so the request that overruns completes and the next one
  is refused with a computed `Retry-After`. Account token minutes are
  charged the same way.
- Durable usage rollups: hourly JSONL intervals with fsync, hourly and
  size-based rotation, day-based retention pruning that never deletes the
  active file or a file it did not write, and a final flush on clean
  shutdown. Interval deltas (not cumulative totals) keep the windows
  summable.
- Decision drift telemetry: choice distributions and confidence buckets
  per model, extracted from `answers` only (never question text or state)
  and stored in the same rollups.
- `GET /admin/usage` with totals rolled up by client, provider, key and
  model, plus the missing-usage count; `GET /admin/usage?window=24h` sums
  durable rollups for a bounded window (maximum 90 days), so "last week"
  survives a restart.
- The admin plane: `GET /admin/status` (redacted serving summary),
  `POST /admin/reload` and `GET /admin/usage` on `server.admin_listen`.
  Access is a constant-time bearer token (both sides hashed, so length
  does not leak) or, without a token, a loopback-only guard that also
  rejects a non-loopback `Host` (DNS rebinding) and cross-site browser
  requests. Empty `admin_listen` disables the plane.
- Configuration reload: SIGHUP and `POST /admin/reload` run the same
  load–validate–build–swap pipeline; failures keep the previous runtime
  serving and increment `jevlar_config_reload_failures_total`. Listener
  addresses and `usage.*` settings are fixed at boot and warn on change.
- Structured request logging (`slog` JSON) with request id, client,
  provider, requested and resolved model, status, attempts and duration.
- Metrics: `jevlar_requests_total`, `jevlar_request_duration_milliseconds`,
  `jevlar_attempts_total`, `jevlar_upstream_errors_total`,
  `jevlar_response_too_large_total`, `jevlar_requests_inflight`,
  `jevlar_key_state`, `jevlar_breaker_state`, `jevlar_key_suspect_total`,
  `jevlar_account_cooldown_seconds`, `jevlar_input_tokens_total`,
  `jevlar_output_tokens_total`, `jevlar_cost_total`,
  `jevlar_usage_missing_total`, `jevlar_client_limited_total` and
  `jevlar_build_info`. Gauge series for identities removed by a reload are
  pruned; a Prometheus exposition test re-parses `/metrics` with the
  canonical text parser.
- Official TypeSafe Python SDK compatibility: `/v1/models` serves the
  System One shape (`{"models":[{"name","description","release_date"}]}`)
  alongside the OpenAI-style `data` array; every response carries
  `X-TypeSafe-Request-Id` and `X-Request-Id`, with a provider id preserved
  as `X-TypeSafe-Upstream-Request-Id`; errors use the SDK's envelope; and
  `Retry-After` is emitted on 429/503, including HTTP-date form.
- Image and long-context support: `multipart/form-data` bodies (the SDK's
  image path) are routed by reading the JSON `request` part while every
  byte is forwarded unchanged; zstd-compressed request bodies are decoded
  for inspection only. `TypeSafe-Danger-Zone-Images` and
  `TypeSafe-Danger-Zone-Long-Context` are forwarded.
- Container image (distroless, non-root, read-only rootfs, built for
  `linux/amd64` and `linux/arm64`), a `HEALTHCHECK` backed by the
  `jevlar healthcheck` subcommand, and a hardened systemd unit with
  `ExecReload`, `EnvironmentFile` and `LimitNOFILE=65536`.
- `configs/container.toml`, a runnable container configuration that binds
  all interfaces.
- `docs/operations.md` runbook and the provider matrix in the README.
- `docs/bench.md` and `scripts/bench.sh`: same-run direct-versus-proxy
  overhead measurement at a fixed rate, plus an unthrottled saturation
  run that fails if any non-200 response is observed.
- `scripts/soak.sh` and `scripts/smoke.sh`: sustained two-provider soak
  and an end-to-end smoke test over real sockets.
- `internal/testutil/cmd/fakeupstream`: a standalone scriptable provider
  used by the benchmark, soak and smoke scripts.
- `tests/sdk/compat.py` and `mise run sdk-compat`: the official TypeSafe
  Python SDK driven against Jevlar end to end (models, all question types,
  typed errors, quotas, request ids, zstd compression, image upload and
  long context). The suite is pinned to the verified SDK version.
- CI and release automation: formatting, lint, vet, race tests, coverage
  gates, dependency and vulnerability checks, fuzzing and smoke tests on
  every push and pull request; a tag push builds Linux and macOS
  (`amd64`/`arm64`) archives with checksums and SPDX SBOMs and publishes a
  multi-arch container image to GHCR.
