#!/usr/bin/env bash
# Soak: sustained load against two providers that inject rate limits and
# failures, asserting that a client never sees an error Jevlar could have
# avoided.
#
# The gate (gates/thresholds.toml, [reliability]) is soak_client_error_rate
# = 0.0: zero client-visible failures other than a documented
# capacity_exhausted 503 that carries Retry-After.
#
# Defaults are set so the run is a *release* gate (10 minutes, 200 rps).
# CI and local runs shorten it with SOAK_SECONDS.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

SOAK_SECONDS="${SOAK_SECONDS:-600}"
SOAK_RPS="${SOAK_RPS:-200}"
SOAK_CONNS="${SOAK_CONNS:-32}"
OUT="bin/soak-$(date +%Y%m%d-%H%M%S)"
PROXY_PORT="${SOAK_PROXY_PORT:-18200}"
UP_A="${SOAK_UP_A:-18210}"
UP_B="${SOAK_UP_B:-18211}"
TOKEN="${SOAK_TOKEN:-soak-token}"
# 10% rate limits, 5% server errors, per provider.
RL_EVERY="${SOAK_RL_EVERY:-10}"
FAIL_EVERY="${SOAK_FAIL_EVERY:-20}"
# Keys per provider. A 1 s cooldown at ~10 rate limits/second needs a pool
# larger than the cooled set or the gateway legitimately runs out.
KEYS_PER_PROVIDER="${SOAK_KEYS:-16}"

mkdir -p "$OUT" bin
pkill -f 'bin/jevlar --config bin/bench-' 2>/dev/null || true
pkill -f 'bin/jevlar --config bin/soak-' 2>/dev/null || true
pkill -f 'bin/fakeupstream -addr' 2>/dev/null || true
sleep 0.3

echo "==> building"
go build -o bin/fakeupstream ./internal/testutil/cmd/fakeupstream
go build -o bin/jevlar ./cmd/jevlar

pids=()
cleanup() {
  for pid in "${pids[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
}
trap cleanup EXIT

echo "==> starting upstreams (:${UP_A}, :${UP_B}) with ${RL_EVERY}th=429 and ${FAIL_EVERY}th=500"
./bin/fakeupstream -addr "127.0.0.1:${UP_A}" -ratelimit-every "$RL_EVERY" -retry-after 1 -fail-every "$FAIL_EVERY" &
pids+=($!)
./bin/fakeupstream -addr "127.0.0.1:${UP_B}" -ratelimit-every "$RL_EVERY" -retry-after 1 -fail-every "$FAIL_EVERY" &
pids+=($!)

for port in "$UP_A" "$UP_B"; do
  for _ in $(seq 1 50); do
    curl -fsS "http://127.0.0.1:${port}/healthz" >/dev/null 2>&1 && break
    sleep 0.1
  done
done

TOKEN_HASH="$(./bin/jevlar keyhash "$TOKEN")"

keys_a=""
keys_b=""
for i in $(seq 1 "$KEYS_PER_PROVIDER"); do
  keys_a+="  { id = \"a${i}\", value = \"secret-a${i}\" },"$'\n'
  keys_b+="  { id = \"b${i}\", value = \"secret-b${i}\" },"$'\n'
done
cat > "$OUT/jevlar.toml" <<EOF
config_version = 1

[server]
listen             = "127.0.0.1:${PROXY_PORT}"
request_timeout_ms = 10000
max_inflight       = 4096
log_level          = "warn"

[auth]
keys = [{ id = "soak", hash = "${TOKEN_HASH}" }]

[[providers]]
name            = "a"
base_url        = "http://127.0.0.1:${UP_A}"
path            = "/v1/systemone"
auth            = "bearer"
models          = ["jev-latest"]
timeout_ms      = 3000
max_concurrency = 512
api_keys        = [
${keys_a}]

[[providers]]
name            = "b"
base_url        = "http://127.0.0.1:${UP_B}"
path            = "/v1/systemone"
auth            = "bearer"
models          = ["nimble"]
timeout_ms      = 3000
max_concurrency = 512
api_keys        = [
${keys_b}]

[models."jev-latest"]
targets = [
  { provider = "a", model = "jev-latest" },
  { provider = "b", model = "nimble" },
]
policy       = "priority"
max_attempts = 8

[breaker]
threshold   = 50
cooldown_ms = 500
EOF
chmod 600 "$OUT/jevlar.toml"

echo "==> starting jevlar on :${PROXY_PORT}"
./bin/jevlar --config "$OUT/jevlar.toml" >"$OUT/jevlar.log" 2>&1 &
pids+=($!)
for _ in $(seq 1 50); do
  curl -fsS "http://127.0.0.1:${PROXY_PORT}/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
if ! curl -fsS "http://127.0.0.1:${PROXY_PORT}/healthz" >/dev/null 2>&1; then
  echo "soak: jevlar did not become healthy" >&2
  tail -5 "$OUT/jevlar.log" >&2 || true
  exit 1
fi

fds_before=$(lsof -p "$(pgrep -f "bin/jevlar --config $OUT/jevlar.toml" | head -1)" 2>/dev/null | wc -l | tr -d ' ' || echo 0)

echo "==> soaking ${SOAK_SECONDS}s at ${SOAK_RPS} rps"
oha -z "${SOAK_SECONDS}s" -c "$SOAK_CONNS" -q "$SOAK_RPS" --no-tui \
  -m POST -H 'Content-Type: application/json' -H "Authorization: Bearer ${TOKEN}" \
  -d '{"model":"jev-latest","state":"soak","questions":{"u":{"type":"noul","instructions":"urgent?"}}}' \
  --output-format json -o "$OUT/result.json" \
  "http://127.0.0.1:${PROXY_PORT}/v1/systemone" >"$OUT/oha.out" 2>"$OUT/oha.err" || true

fds_after=$(lsof -p "$(pgrep -f "bin/jevlar --config $OUT/jevlar.toml" | head -1)" 2>/dev/null | wc -l | tr -d ' ' || echo 0)

curl -fsS "http://127.0.0.1:${PROXY_PORT}/metrics" > "$OUT/metrics.txt" 2>/dev/null || true

python3 - "$OUT" "$fds_before" "$fds_after" <<'PY'
import json, os, sys

out, before, after = sys.argv[1], sys.argv[2], sys.argv[3]
res = {}
try:
    res = json.load(open(os.path.join(out, "result.json")))
except Exception:
    pass

codes = res.get("statusCodeDistribution") or {}
total = sum(codes.values())
errors = {c: n for c, n in codes.items() if not c.startswith("2")}

inflight = None
for line in open(os.path.join(out, "metrics.txt"), errors="ignore"):
    if line.startswith("jevlar_requests_inflight"):
        inflight = line.split()[-1]

summary = {
    "requests": total,
    "status_codes": codes,
    "error_responses": errors,
    "requests_inflight_after": inflight,
    "fd_before": before,
    "fd_after": after,
}
print(json.dumps(summary, indent=2))

failures = []
if total == 0:
    failures.append("no requests completed")
if errors:
    failures.append(f"client-visible non-2xx responses: {errors}")
if inflight not in (None, "0"):
    failures.append(f"jevlar_requests_inflight did not return to zero: {inflight}")
try:
    if int(after) > int(before) + 20:
        failures.append(f"descriptor growth: {before} -> {after}")
except ValueError:
    pass

print("\nevidence: " + out)
if failures:
    print("FAILED gates: " + "; ".join(failures))
    sys.exit(1)
print("soak passed: zero client-visible errors, inflight drained")
PY
