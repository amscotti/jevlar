#!/usr/bin/env bash
# Run the official TypeSafe Python SDK against Jevlar, end to end.
# Requires TYPESAFE_API_KEY and network access.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

: "${TYPESAFE_API_KEY:?set TYPESAFE_API_KEY (see .env) }"
PORT="${SDK_PORT:-28700}"
ADMIN="${SDK_ADMIN:-28701}"
TMP="$(mktemp -d)"
VENV=".venv-sdk"
pids=()
cleanup() {
  for pid in "${pids[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  rm -rf "$TMP"
}
trap cleanup EXIT

go build -o bin/jevlar ./cmd/jevlar

TOKEN="sdk-token"
LIMITED="limited-token"
cat > "$TMP/jevlar.toml" <<CFG
config_version = 1

[server]
listen = "127.0.0.1:${PORT}"
admin_listen = "127.0.0.1:${ADMIN}"
request_timeout_ms = 120000
log_level = "warn"
# Image uploads exceed the text-oriented default cap.
max_body_bytes = 8388608
max_inflight   = 64

[auth]
keys = [
  { id = "sdk", hash = "$(./bin/jevlar keyhash "$TOKEN")" },
  { id = "limited", hash = "$(./bin/jevlar keyhash "$LIMITED")", rpm = 1 },
]

[[providers]]
name = "typesafe"
base_url = "https://api.typesafe.ai"
path = "/v1/systemone"
auth = "bearer"
models = ["jev-latest", "jev-preview", "jev-1.14"]
timeout_ms = 120000
max_concurrency = 8
api_keys = [{ id = "primary", value = "\${TYPESAFE_API_KEY}" }]

[models."jev-latest"]
targets = [{ provider = "typesafe", model = "jev-latest" }]
max_attempts = 1
description = "Latest general-purpose Jev decision model (TypeSafe)."
release_date = "2026-09-15"

# Vision (image) support is a preview-model capability on the provider side.
[models."jev-preview"]
targets = [{ provider = "typesafe", model = "jev-preview" }]
max_attempts = 1
description = "Preview Jev model; required for image and box questions."
release_date = "2026-10-14"
CFG
chmod 600 "$TMP/jevlar.toml"

./bin/jevlar --config "$TMP/jevlar.toml" > "$TMP/jevlar.log" 2>&1 &
pids+=($!)
for _ in $(seq 1 50); do
  curl -fsS "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
curl -fsS "http://127.0.0.1:${PORT}/healthz" >/dev/null || { cat "$TMP/jevlar.log"; exit 1; }

if [[ ! -x "$VENV/bin/python" ]]; then
  python3 -m venv "$VENV"
  "$VENV/bin/pip" -q install --upgrade pip
fi
"$VENV/bin/pip" -q install -r tests/sdk/requirements.txt

JEVLAR_BASE_URL="http://127.0.0.1:${PORT}" \
JEVLAR_TOKEN="$TOKEN" \
JEVLAR_LIMITED="$LIMITED" \
  "$VENV/bin/python" tests/sdk/compat.py
