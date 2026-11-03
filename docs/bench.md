# Benchmarks

Jevlar's performance gates are pre-registered in `gates/thresholds.toml` and
exercised by `scripts/bench.sh`. This document records the method so a result
can be reproduced and falsified, not just quoted.

## What is measured

| Gate | Value | What it means |
|---|---|---|
| `proxy_overhead_p50_ms` | ≤ 2.0 | median added latency versus calling the upstream directly |
| `proxy_overhead_p99_ms` | ≤ 10.0 | tail added latency at the same rate |
| `saturation_min_rps` | ≥ 6,000 | throughput with a zero-latency upstream and 64 connections |
| `startup_ms` | ≤ 100 | process start to a serving socket |
| `idle_rss_mb` | ≤ 40 | resident memory while idle |
| `binary_mb` | ≤ 16 | stripped release binary |

## Method

`scripts/bench.sh` runs two measurements in the same process lifetime on the
same host, because comparing a gateway run to a stored number from another
machine measures the machine, not the gateway:

1. Start `fakeupstream` (`internal/testutil/cmd/fakeupstream`) with a 40 ms
   injected delay. It returns a small System One-shaped JSON body and echoes
   the SHA-256 of the request body.
2. Measure the upstream **directly** for 20 s at a fixed 100 requests/second
   across 16 connections.
3. Start Jevlar in front of the same upstream and measure it identically.
4. Overhead = gateway percentile − direct percentile.

The load is rate-limited (`-q 100`) on purpose. At 16 connections and a 40 ms
upstream, an unthrottled client saturates its own connection pool, so the
percentiles measure queueing luck rather than the gateway; with a fixed rate
well below saturation every request starts from an idle connection and the
delta is the gateway's cost. A cross-check at 1 connection and a zero-latency
upstream puts Jevlar's per-request cost at roughly 0.12 ms.

The saturation run is unthrottled: 64 connections, zero-latency upstream, 10 s,
and the script fails if any response other than 200 is observed.

## Tooling note

The original design named `wrk`; this implementation uses `oha` because it
emits machine-readable JSON with latency percentiles, which makes the gate
assertable rather than eyeballed. The script falls back to `ab` when `oha` is
absent (with the percentile comparison skipped).

## Latest recorded run

Host: Apple silicon macOS, Go 1.27.3, loopback interface.

```json
{
  "direct_p50_ms": 41.62,
  "proxied_p50_ms": 42.36,
  "p50_overhead_ms": 0.74,
  "direct_p99_ms": 52.11,
  "proxied_p99_ms": 54.5,
  "p99_overhead_ms": 2.4,
  "saturation_rps": 17666.6,
  "saturation_success_rate": 100.0,
  "method": {
    "upstream_delay_ms": "40",
    "proxy_connections": "16",
    "saturation_connections": "64",
    "load_qps": "100"
  }
}
```

Evidence, including the per-run raw JSON, is written to `bin/bench-<timestamp>/`
(ignored by git).

## Interpreting a regression

- **p50 overhead up, p99 flat**: an extra allocation or serialisation pass on
  the hot path. Check the model scan and header cloning first.
- **p99 overhead up, p50 flat**: contention — the provider slot pool, the
  transport, or a lock on the reload path.
- **saturation rps down, overhead flat**: CPU per request grew; run
  `go test -bench . -benchmem ./internal/proxy/` and compare allocations.
