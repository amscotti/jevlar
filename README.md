# Jevlar

[![CI](https://github.com/amscotti/jevlar/actions/workflows/ci.yml/badge.svg)](https://github.com/amscotti/jevlar/actions/workflows/ci.yml)

Jevlar (Jev + Kevlar) is a small, self-hosted gateway that sits between your
applications and decision-model APIs that speak the System One wire format.
Callers authenticate once against Jevlar; Jevlar picks a healthy provider and
key, retries elsewhere when the first choice fails, keeps rate-limited and
revoked keys away from the hot path, and records what every caller consumed —
all from one TOML file, with no database and no UI.

The product promise is boring reliability: a decision call should never fail
because one key hit its limit, and nobody should have to read a provider
dashboard to know what a feature costs.

## Quickstart

Requires [mise](https://mise.jdx.dev/) (pins Go and golangci-lint; see
`mise.toml`).

```sh
mise install && mise run check   # fmt + lint + vet + race tests + build + coverage gate

go run ./cmd/jevlar --config configs/minimal.toml
```

In another terminal:

```sh
curl http://127.0.0.1:8080/healthz   # {"status":"ok"}
curl http://127.0.0.1:8080/metrics   # Prometheus text, incl. jevlar_build_info
```

`configs/minimal.toml` needs no environment variables. Stop the server with
Ctrl-C or SIGTERM — the first signal shuts down gracefully within
`server.shutdown_grace_ms`; a second signal closes the server immediately.
Reload the configuration with SIGHUP: the file is re-read, re-validated and
swapped in atomically; a failed reload keeps the previous configuration
serving and increments `jevlar_config_reload_failures_total`.

Validate a configuration without starting anything:

```sh
go run ./cmd/jevlar --check --config path/to/jevlar.toml   # exit 0 valid, 2 invalid
```

Configure an inbound client key by hashing its token (only the hash is
stored in the configuration):

```sh
go run ./cmd/jevlar keyhash my-client-token
# paste the printed SHA-256 hex into [auth] keys[].hash
```

## Providers

Jevlar speaks one wire format (System One) and reaches it through any
number of configured providers.

| Provider | Base URL + path | Auth | Notes |
|---|---|---|---|
| TypeSafe | `https://api.typesafe.ai` + `/v1/systemone` | bearer | `jev-latest`, `jev-1.13.0`, `jev-1.14`; limits are **per account** |
| Upstage Solar Decide | `https://api.upstage.ai` + `/v1/systemone` | bearer | `solar-decide`; max 26 choices per question (422 beyond) |
| Ollama 0.35+ | `http://127.0.0.1:11434` + `/v1/systemone` | none | `nimble`, `tev1`; ≤64 questions, 64 KiB body |
| OpenRouter | `https://openrouter.ai` + `/api/alpha/decisions` | bearer | `typesafe/jev-1.14`; reports `usage.cost` |
| Kev (self-hosted) | operator-defined | optional | Apache-2.0, runs locally |

The same model may have a different id per provider, so a routing target
names the model explicitly:

```toml
[models."jev-latest"]
targets = [
  { provider = "typesafe",     model = "jev-latest" },
  { provider = "ollama-local", model = "nimble" },
]
policy       = "priority"   # or "fastest"
max_attempts = 3
```

Jevlar splices that `model` value into the request and forwards every other
byte unchanged.

> **Account limits do not multiply with keys.** If several keys share an
> `[accounts.<name>]` group, they share the provider's per-account limit:
> one 429 cools the whole account. Adding a second key to the same account
> does not add capacity. See `docs/operations.md`.

## Running it

```sh
mise run build && ./bin/jevlar --config configs/getting-started.toml
```

Container and systemd:

```sh
docker build -t jevlar:dev .
# container.toml binds 0.0.0.0 so the published port reaches the process;
# .env must define TYPESAFE_API_KEY and JEVLAR_CLIENT_KEY_HASH
# (go run ./cmd/jevlar keyhash my-client-token).
docker run --rm -p 8080:8080 \
  -v "$PWD/configs/container.toml:/etc/jevlar/config.toml:ro" \
  --env-file .env jevlar:dev

sudo useradd --system --no-create-home --shell /usr/sbin/nologin jevlar
sudo install -m 0755 bin/jevlar /usr/local/bin/jevlar
sudo install -d /etc/jevlar
sudo install -m 0600 configs/example.toml /etc/jevlar/config.toml   # then edit
sudo install -m 0644 jevlar.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now jevlar
sudo systemctl reload jevlar     # SIGHUP: atomic config reload
```

Under systemd, put secrets in `/etc/jevlar/jevlar.env` (read by
`EnvironmentFile=` in the unit) or use `api_keys[].value_file`.

See `docs/operations.md` for the runbook (key rotation, 401 storms, all
keys cooling, metric interpretation) and `docs/bench.md` for the
performance method.

## Client compatibility

Jevlar is wire-compatible with the official **TypeSafe Python SDK**
(`pip install typesafe-sdk`). Point the SDK at the gateway with its own
`base_url` option — no patching, no proxy shim:

```python
from typesafe_sdk import TypeSafeClient, Noul, Choice

client = TypeSafeClient(api_key="my-client-token", base_url="http://127.0.0.1:8080")

models = client.models.list()          # GET /v1/models
answer = client.system_one(
    "A customer has tried to connect Stripe for three days.",
    {
        "urgent": Noul(instructions="Does this message express urgency?"),
        "team": Choice(instructions="Which team?",
                       criteria={"billing": "Payments", "technical": "Bugs"}),
    },
    model="jev-latest",
)
print(answer.answers["team"].choice, answer.usage.input_tokens)
```

Compatibility details worth knowing:

- `GET /v1/models` serves the System One shape the SDK parses
  (`{"models":[{"name","description","release_date"}]}`) **and** the
  OpenAI-style `{"data":[{"id"}]}` shape, in one document. Both require the
  client token.
- Errors Jevlar generates carry the SDK's error envelope
  (`{"error":{"type","message","request_id"}}`), which the SDK maps to its
  typed exceptions (`TypeSafeAuthenticationError`, `TypeSafeRateLimitError`,
  …).
- Every response carries `x-typesafe-request-id` (Jevlar's id, also in
  `X-Request-Id`), so `error.request_id` correlates with Jevlar's logs. A
  provider's own id is preserved as `x-typesafe-upstream-request-id`.
- `Retry-After` is emitted on 429/503 responses; the SDK honours it,
  including HTTP-date form.
- Model metadata in the list comes from the alias's `description` and
  `release_date` when configured; unknown dates are reported as an empty
  string rather than invented.

### Images and the provider's danger-zone features

TypeSafe's image support is a gated beta: it needs the
`TypeSafe-Danger-Zone-Images: true` request header, a `box` question (or an
`assets` entry), and a vision-capable model — currently **`jev-preview`**.
`TypeSafe-Danger-Zone-Long-Context: true` similarly unlocks long inputs.

Jevlar forwards those headers, and accepts both ways the SDK sends images:

- **multipart/form-data** (what the SDK uses): a JSON part named `request`
  plus one binary part per image. Jevlar reads the model from the JSON part
  and forwards every byte, boundaries and image data included.
- **inline JSON** `assets` with `data_base64`, which passes through as an
  ordinary JSON body.

The SDK compresses request bodies with zstd above 1 KiB, so a real image
upload arrives compressed *and* multipart. Jevlar decodes a copy to find the
model; the bytes forwarded upstream stay exactly as received unless a model
rewrite is required (then the body is sent decoded, since the compressed
bytes no longer describe the new document).

Image requests need a body cap larger than the default:

```toml
[server]
max_body_bytes = 8388608   # 8 MiB: photos, not just text
max_inflight   = 128       # keeps worst-case buffering ~1 GiB
```

> **Never route image traffic to a text-only provider.** Local System One
> models (Ollama `nimble`/`tev1`) accept the request, **silently ignore the
> image**, and answer anyway — verified: identical token counts and identical
> answers with and without the image. Give image models their own alias
> pointed at a vision provider; do not reuse the text alias as
> `configs/example.toml` does.

`mise run sdk-compat` runs the real SDK against a live gateway (needs
`TYPESAFE_API_KEY`) and includes an image upload and a long-context request;
see `tests/sdk/README.md`.

## Admin plane

When `server.admin_listen` is set, Jevlar serves `GET /admin/status`
(version, uptime, config hash, providers, model table, auth key ids,
breaker settings — never key material) and `POST /admin/reload` (the same
pipeline as SIGHUP). Leave `admin_listen` empty to disable the admin plane
entirely. Without `admin_token` the admin listener must be loopback and
the handler independently rejects non-loopback connections; with a token
(≥ 32 characters) requests must present `Authorization: Bearer <token>`.
`configs/example.toml` shows the complete configuration surface.

## Development

Everything runs through mise tasks (see `mise.toml`); `mise tasks` lists
them:

```sh
mise run fmt         # rewrite with gofumpt (via golangci-lint fmt)
mise run lint        # golangci-lint with the project configuration
mise run test        # unit + integration tests
mise run test-race   # tests under the race detector
mise run cover       # coverage profile, per-function report, threshold gate
mise run build       # compile bin/jevlar with the git-derived version
mise run check       # everything CI gates on
mise run ci          # check + tidy-check + deps-gate + vuln (what CI runs)
```

Tests never touch the network; upstreams are faked with
`internal/testutil`. Configuration rules and their enforcement live in
`internal/config`; the pre-registered quality gates are in
`gates/thresholds.toml` (enforced by `scripts/coverage_gate.sh`); notable
changes are recorded in `CHANGELOG.md`; significant decisions are recorded in
`docs/adr/`.

## Releasing

Releases are tag-driven. Pushing a semver tag runs the **Release** workflow,
which tests the tree first and then publishes:

- Linux and macOS archives for `amd64` and `arm64` (tar.gz, with checksums
  and SPDX SBOMs) from goreleaser;
- a multi-arch container image on GHCR (`linux/amd64`, `linux/arm64`),
  published only after the binary release succeeds.

```sh
git tag v0.1.1
git push origin v0.1.1
```

The **CI** workflow runs on every push to `main` and every pull request:
the full `mise run ci` gate (formatting, lint, vet, race tests, coverage,
tidy, dependency gate, `govulncheck`, fuzzing, smoke test), a goreleaser
config check plus snapshot cross-build, and a container image build.

## Licence

Apache-2.0 — see `LICENSE` and `NOTICE`.
