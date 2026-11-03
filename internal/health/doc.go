// Package health owns Jevlar's process-lifetime mutable state: per-key
// in-flight counters, cooldowns and disable status, per-provider circuit
// breakers, and per-account and per-client rate-limit buckets. The registry
// is created once at boot and outlives configuration reloads — a reload
// reconciles entries against the new provider specification, it never
// replaces the registry, so a disabled or cooling key cannot be resurrected
// by reloading. The package imports only the standard library; nothing in
// Jevlar may import it behind business logic that needs its state.
package health
