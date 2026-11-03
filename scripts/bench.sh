#!/usr/bin/env bash
# Measure Jevlar's proxy overhead against a direct upstream call.
#
# Method (see docs/bench.md):
#   1. start a fake upstream with an injected delay
#   2. measure the upstream directly
#   3. start Jevlar in front of it and measure through the gateway
#   4. overhead = gateway percentile - direct percentile, same run, same host
#
# Requires: oha (preferred) or ab. Evidence is written to
# bin/bench-<timestamp>/ as JSON plus a summary.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

UPSTREAM_DELAY_MS="${UPSTREAM_DELAY_MS:-40}"
UP_CONNS="${UP_CONNS:-16}"
SAT_CONNS="${SAT_CONNS:-64}"
SAT_DURATION="${SAT_DURATION:-10s}"
PROXY_DURATION="${PROXY_DURATION:-20s}"
LOAD_QPS="${LOAD_QPS:-100}"
UP_PORT="${UP_PORT:-18090}"
PROXY_PORT="${PROXY_PORT:-18080}"
SAT_PROXY_PORT="${SAT_PROXY_PORT:-18082}"
TOKEN="${JEVLAR_BENCH_TOKEN:-bench-token}"
OUT="bin/bench-$(date +%Y%m%d-%H%M%S)"

if ! command -v oha >/dev/null 2>&1 && ! command -v ab >/dev/null 2>&1; then
  echo "bench: neither oha nor ab found; install one (brew install oha)" >&2
  exit 1
fi

mkdir -p "$OUT" bin

# A stale bench process from an earlier run would silently serve the load
# against the wrong configuration, so clear leftovers whose command line
# points at a bench/soak output directory (not an operator's dev instance).
pkill -f 'bin/jevlar --config bin/bench-' 2>/dev/null || true
pkill -f 'bin/jevlar --config bin/soak-' 2>/dev/null || true
pkill -f 'bin/fakeupstream -addr' 2>/dev/null || true
sleep 0.3

echo "==> building binaries"
go build -o bin/fakeupstream ./internal/testutil/cmd/fakeupstream
go build -o bin/jevlar ./cmd/jevlar

pids=()
cleanup() {
  for pid in "${pids[@]:-}"; do
    kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
}
trap cleanup EXIT

TOKEN_HASH="$(./bin/jevlar keyhash "$TOKEN")"
cat > "$OUT/jevlar.toml" <<EOF
config_version = 1

[server]
listen             = "127.0.0.1:${PROXY_PORT}"
request_timeout_ms = 30000
max_body_bytes     = 262144
max_response_bytes = 1048576
max_inflight       = 4096
log_level          = "error"

[auth]
keys = [{ id = "bench", hash = "${TOKEN_HASH}" }]

[[providers]]
name            = "fake"
base_url        = "http://127.0.0.1:${UP_PORT}"
path            = "/v1/systemone"
auth            = "none"
models          = ["jev-latest"]
timeout_ms      = 30000
max_concurrency = 256
EOF
chmod 600 "$OUT/jevlar.toml"

PAYLOAD='{"model":"jev-latest","state":"a customer could not pay","questions":{"urgent":{"type":"noul","instructions":"urgent?"}}}'
printf '%s' "$PAYLOAD" > "$OUT/request.json"

echo "==> starting upstream (delay ${UPSTREAM_DELAY_MS}ms) on :${UP_PORT}"
./bin/fakeupstream -addr "127.0.0.1:${UP_PORT}" -delay "${UPSTREAM_DELAY_MS}ms" &
pids+=($!)

for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:${UP_PORT}/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.1
done

echo "==> starting jevlar on :${PROXY_PORT}"
./bin/jevlar --config "$OUT/jevlar.toml" >"$OUT/jevlar.log" 2>&1 &
pids+=($!)

for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:${PROXY_PORT}/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
if ! curl -fsS "http://127.0.0.1:${PROXY_PORT}/healthz" >/dev/null 2>&1; then
  echo "bench: jevlar did not become healthy on :${PROXY_PORT}" >&2
  tail -5 "$OUT/jevlar.log" >&2 || true
  exit 1
fi

run_load() { # name url duration connections auth
  local name="$1" url="$2" duration="$3" conns="$4" auth="$5"
  local qps="${6:-}"
  local auth_args=""
  if [[ -n "$auth" ]]; then auth_args="Authorization: Bearer ${TOKEN}"; fi
  if command -v oha >/dev/null 2>&1; then
    # A fixed rate (when given) keeps the client's own queue out of the
    # percentiles, so the difference between the two runs measures the
    # gateway rather than scheduler luck in a saturated client.
    local -a base=(oha -z "$duration" -c "$conns" -m POST --no-tui
      -H 'Content-Type: application/json'
      -d "$PAYLOAD" --output-format json)
    if [[ -n "${qps:-}" ]]; then base+=(-q "$qps" --latency-correction); fi
    if [[ -n "$auth_args" ]]; then base+=(-H "$auth_args"); fi
    "${base[@]}" -o "$OUT/${name}.json" "$url" \
      >"$OUT/${name}.out" 2>"$OUT/${name}.err" || true
    if [[ ! -s "$OUT/${name}.json" ]]; then
      echo "bench: ${name} produced no measurement (see $OUT/${name}.err)" >&2
      exit 1
    fi
  else
    ab -q -t "${duration%s}" -c "$conns" -p "$OUT/request.json" -T 'application/json' \
      ${auth:+-H "Authorization: Bearer ${TOKEN}"} "$url" >"$OUT/${name}.txt" 2>&1 || true
    if [[ ! -s "$OUT/${name}.txt" ]]; then
      echo "bench: ${name} produced no measurement" >&2
      exit 1
    fi
  fi
}

echo "==> measuring direct upstream"
run_load direct "http://127.0.0.1:${UP_PORT}/v1/systemone" "$PROXY_DURATION" "$UP_CONNS" "" "$LOAD_QPS"
echo "==> measuring through jevlar"
run_load proxied "http://127.0.0.1:${PROXY_PORT}/v1/systemone" "$PROXY_DURATION" "$UP_CONNS" "auth" "$LOAD_QPS"

# Saturation: a zero-delay upstream so CPU, not latency, is the limit.
echo "==> measuring saturation (0ms upstream, ${SAT_CONNS} connections)"
./bin/fakeupstream -addr "127.0.0.1:$((UP_PORT + 100))" -delay 0ms &
pids+=($!)
SAT_UP_PORT=$((UP_PORT + 100))
sed "s|${UP_PORT}|${SAT_UP_PORT}|g; s|${PROXY_PORT}|${SAT_PROXY_PORT}|g" "$OUT/jevlar.toml" > "$OUT/jevlar-sat.toml"
./bin/jevlar --config "$OUT/jevlar-sat.toml" >"$OUT/jevlar-sat.log" 2>&1 &
pids+=($!)
for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:${SAT_PROXY_PORT}/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
if ! curl -fsS "http://127.0.0.1:${SAT_PROXY_PORT}/healthz" >/dev/null 2>&1; then
  echo "bench: saturation jevlar did not become healthy on :${SAT_PROXY_PORT}" >&2
  tail -5 "$OUT/jevlar-sat.log" >&2 || true
  exit 1
fi
run_load saturation "http://127.0.0.1:${SAT_PROXY_PORT}/v1/systemone" "$SAT_DURATION" "$SAT_CONNS" "auth"

python3 - "$OUT" <<'PY'
import json, os, statistics, sys

out = sys.argv[1]

def load_oha(name):
    path = os.path.join(out, name + ".json")
    if not os.path.exists(path):
        return None
    try:
        with open(path) as fh:
            text = fh.read().strip()
        if not text:
            return None
        return json.loads(text)
    except Exception:
        return None

def pct(values, p):
    if not values:
        return None
    values = sorted(values)
    k = (len(values) - 1) * (p / 100.0)
    lo, hi = int(k), min(int(k) + 1, len(values) - 1)
    return values[lo] + (values[hi] - values[lo]) * (k - lo)

summary = {}

direct = load_oha("direct")
proxied = load_oha("proxied")
saturation = load_oha("saturation")

if proxied and direct:
    def lat(doc, key):
        v = (doc.get("latencyPercentiles") or {}).get(key)
        return v * 1000 if v is not None else None  # oha reports seconds

    d50, d99 = lat(direct, "p50"), lat(direct, "p99")
    p50, p99 = lat(proxied, "p50"), lat(proxied, "p99")
    summary["direct_p50_ms"] = round(d50, 2) if d50 else None
    summary["proxied_p50_ms"] = round(p50, 2) if p50 else None
    summary["p50_overhead_ms"] = round(p50 - d50, 2) if (p50 and d50) else None
    summary["direct_p99_ms"] = round(d99, 2) if d99 else None
    summary["proxied_p99_ms"] = round(p99, 2) if p99 else None
    summary["p99_overhead_ms"] = round(p99 - d99, 2) if (p99 and d99) else None
    summary["proxied_rps"] = round(proxied.get("summary", {}).get("requestsPerSec", 0), 1)
    summary["direct_rps"] = round(direct.get("summary", {}).get("requestsPerSec", 0), 1)

if saturation:
    codes = saturation.get("statusCodeDistribution") or {}
    summary["saturation_codes"] = codes
    summary["saturation_rps"] = round(saturation.get("summary", {}).get("requestsPerSec", 0), 1)
    summary["saturation_success_rate"] = round(saturation.get("summary", {}).get("successRate", 0) * 100, 2)

summary["method"] = {
    "upstream_delay_ms": os.environ.get("UPSTREAM_DELAY_MS", "40"),
    "proxy_connections": os.environ.get("UP_CONNS", "16"),
    "saturation_connections": os.environ.get("SAT_CONNS", "64"),
    "load_qps": os.environ.get("LOAD_QPS", "100"),
    "tool": "oha" if os.path.exists(os.path.join(out, "proxied.json")) else "ab",
}

with open(os.path.join(out, "summary.json"), "w") as fh:
    json.dump(summary, fh, indent=2)

print(json.dumps(summary, indent=2))

failures = []
if summary.get("p50_overhead_ms") is not None and summary["p50_overhead_ms"] > 2.0:
    failures.append(f"p50 overhead {summary['p50_overhead_ms']}ms > 2.0ms")
if summary.get("p99_overhead_ms") is not None and summary["p99_overhead_ms"] > 10.0:
    failures.append(f"p99 overhead {summary['p99_overhead_ms']}ms > 10.0ms")
if summary.get("saturation_rps") and summary["saturation_rps"] < 6000:
    failures.append(f"saturation {summary['saturation_rps']} rps < 6000")
codes = summary.get("saturation_codes") or {}
if codes and set(codes) != {"200"}:
    failures.append(f"saturation observed non-200 responses: {codes}")

print("\nevidence: " + out)
if failures:
    print("FAILED gates: " + "; ".join(failures))
    sys.exit(1)
print("all performance gates passed")
PY
