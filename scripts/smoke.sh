#!/usr/bin/env bash
# End-to-end smoke: built binary + real sockets + a fake upstream.
# Asserts the documented happy path and the two fail-closed paths.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

UP_PORT="${SMOKE_UP_PORT:-18310}"
PORT="${SMOKE_PORT:-18300}"
ADMIN="${SMOKE_ADMIN:-18301}"
TOKEN="smoke-token"
TMP="$(mktemp -d)"
pids=()
cleanup() {
  for pid in "${pids[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  rm -rf "$TMP"
}
trap cleanup EXIT

go build -o bin/fakeupstream ./internal/testutil/cmd/fakeupstream
go build -o bin/jevlar ./cmd/jevlar

./bin/fakeupstream -addr "127.0.0.1:${UP_PORT}" -delay 10ms &
pids+=($!)
for _ in $(seq 1 50); do
  curl -fsS "http://127.0.0.1:${UP_PORT}/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done

cat > "$TMP/jevlar.toml" <<CFG
config_version = 1
[server]
listen = "127.0.0.1:${PORT}"
admin_listen = "127.0.0.1:${ADMIN}"
request_timeout_ms = 5000
log_level = "warn"
[auth]
keys = [{ id = "smoke", hash = "$(./bin/jevlar keyhash "$TOKEN")" }]
[[providers]]
name = "fake"
base_url = "http://127.0.0.1:${UP_PORT}"
path = "/v1/systemone"
auth = "none"
models = ["jev-latest"]
timeout_ms = 2000
[models."jev-latest"]
targets = [{ provider = "fake", model = "jev-latest" }]
max_attempts = 2
CFG
chmod 600 "$TMP/jevlar.toml"

./bin/jevlar --config "$TMP/jevlar.toml" >"$TMP/jevlar.log" 2>&1 &
pids+=($!)
for _ in $(seq 1 50); do
  curl -fsS "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done

fail() { echo "smoke: $*" >&2; exit 1; }

[[ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT}/healthz")" == "200" ]] || fail "healthz"
[[ "$(curl -s "http://127.0.0.1:${PORT}/readyz")" == '{"status":"ready"}' ]] || fail "readyz"
[[ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT}/metrics")" == "200" ]] || fail "metrics"
[[ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${ADMIN}/admin/status")" == "200" ]] || fail "admin status"

# Unauthenticated request is refused before any upstream contact.
[[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${PORT}/v1/systemone" -d '{"model":"jev-latest"}')" == "401" ]] || fail "unauthenticated request was not 401"

code=$(curl -s -o "$TMP/body.json" -w '%{http_code}' -X POST "http://127.0.0.1:${PORT}/v1/systemone" \
  -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
  -d '{"model":"jev-latest","state":"smoke","questions":{"u":{"type":"noul","instructions":"urgent?"}}}')
[[ "$code" == "200" ]] || fail "authenticated request returned $code"
grep -q '"usage"' "$TMP/body.json" || fail "response has no usage block"

# Fail-closed: a bad config exits 2 without binding.
echo 'config_version = 9' > "$TMP/bad.toml"
set +e
./bin/jevlar --check --config "$TMP/bad.toml" >"$TMP/bad.out" 2>&1
rc=$?
set -e
[[ $rc -eq 2 ]] || fail "--check on a bad config exited $rc, want 2"

# SIGHUP reload keeps the listener.
kill -HUP "$(pgrep -f "bin/jevlar --config $TMP/jevlar.toml" | head -1)"
sleep 0.5
[[ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT}/healthz")" == "200" ]] || fail "listener dropped on reload"

echo "smoke: ok"
