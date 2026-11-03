// Package usage turns upstream "usage" blocks into per-client, per-provider
// and per-key token and cost counters, and (when enabled) writes durable
// hourly JSONL rollups. Counters are in-memory and atomic; the package never
// guesses a missing usage block — it counts it missing. It may import
// internal/metrics and the standard library.
package usage
