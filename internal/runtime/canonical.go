package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/amscotti/jevlar/internal/config"
)

// ConfigHash returns the SHA-256 hex of the canonical serialisation of
// the resolved configuration: every field with its post-expansion,
// post-defaults value, map keys sorted, array order preserved (it is
// semantic). Formatting changes and comments in the file do not change
// the hash; any change to a resolved value does.
//
// Secret material is deliberately excluded: api_keys[].value contents and
// resolved file values never enter the serialisation, so the hash — which
// /admin/status publishes — cannot serve as a verifier for key material.
// The presence, ids and file paths of keys are included, so structural
// key changes do change the hash. Inbound auth hashes are configuration
// data and are included.
func ConfigHash(cfg *config.Config) string {
	sum := sha256.Sum256(canonicalJSON(cfg))
	return hex.EncodeToString(sum[:])
}

// canonicalJSON serialises the resolved configuration deterministically:
// map[string]any marshals with sorted keys at every level (encoding/json
// guarantees this), and arrays keep their configured order.
func canonicalJSON(cfg *config.Config) []byte {
	server := map[string]any{
		"listen":                 cfg.Server.Listen,
		"admin_listen":           cfg.Server.AdminListen,
		"admin_token_set":        cfg.Server.AdminToken != "",
		"request_timeout_ms":     cfg.Server.RequestTimeoutMs,
		"shutdown_grace_ms":      cfg.Server.ShutdownGraceMs,
		"read_header_timeout_ms": cfg.Server.ReadHeaderTimeoutMs,
		"max_body_bytes":         cfg.Server.MaxBodyBytes,
		"max_response_bytes":     cfg.Server.MaxResponseBytes,
		"max_inflight":           cfg.Server.MaxInflight,
		"log_level":              cfg.Server.LogLevel,
		"log_bodies":             cfg.Server.LogBodies,
	}

	authKeys := make([]any, 0, len(cfg.Auth.Keys))
	for _, k := range cfg.Auth.Keys {
		authKeys = append(authKeys, map[string]any{
			"id":   k.ID,
			"hash": k.Hash,
			"rpm":  k.RPM,
			"tpm":  k.TPM,
		})
	}

	accounts := make(map[string]any, len(cfg.Accounts))
	for name, a := range cfg.Accounts {
		accounts[name] = map[string]any{"rpm": a.RPM, "tpm": a.TPM}
	}

	providers := make([]any, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		keys := make([]any, 0, len(p.APIKeys))
		for _, k := range p.APIKeys {
			// The secret itself (value / resolved file contents) is
			// excluded on purpose; see ConfigHash.
			keys = append(keys, map[string]any{
				"id":         k.ID,
				"value_file": k.ValueFile,
				"value_set":  k.Value != "",
				"has_secret": k.Resolved != "",
			})
		}
		models := toAny(p.Models)
		providers = append(providers, map[string]any{
			"name":            p.Name,
			"base_url":        p.BaseURL,
			"path":            p.Path,
			"auth":            p.Auth,
			"account":         p.Account,
			"models":          models,
			"timeout_ms":      p.TimeoutMs,
			"max_concurrency": p.MaxConcurrency,
			"api_keys":        keys,
		})
	}

	models := make(map[string]any, len(cfg.Models))
	for alias, m := range cfg.Models {
		targets := make([]any, 0, len(m.Targets))
		for _, t := range m.Targets {
			targets = append(targets, map[string]any{"provider": t.Provider, "model": t.Model})
		}
		models[alias] = map[string]any{
			"targets":       targets,
			"policy":        m.Policy,
			"max_attempts":  m.MaxAttempts,
			"ewma_alpha":    m.EwmaAlpha,
			"hysteresis_ms": m.HysteresisMs,
			"explore_every": m.ExploreEvery,
			"description":   m.Description,
			"release_date":  m.ReleaseDate,
		}
	}

	doc := map[string]any{
		"config_version": cfg.ConfigVersion,
		"server":         server,
		"security": map[string]any{
			"strict_file_permissions": cfg.Security.StrictFilePermissions,
		},
		"usage": map[string]any{
			"enabled":        cfg.Usage.Enabled,
			"dir":            cfg.Usage.Dir,
			"retention_days": cfg.Usage.RetentionDays,
			"max_file_mb":    cfg.Usage.MaxFileMB,
		},
		"auth":      map[string]any{"keys": authKeys},
		"accounts":  accounts,
		"providers": providers,
		"models":    models,
		"breaker":   map[string]any{"threshold": cfg.Breaker.Threshold, "cooldown_ms": cfg.Breaker.CooldownMs},
	}
	// json.Marshal of map[string]any cannot fail (all values are
	// JSON-safe primitives, slices and maps); a panic here is a
	// programmer error in this function.
	b, err := json.Marshal(doc)
	if err != nil {
		panic("canonical config serialisation failed: " + err.Error())
	}
	return b
}

// toAny converts a string slice to []any for map[string]any composition.
func toAny(s []string) []any {
	out := make([]any, 0, len(s))
	for _, v := range s {
		out = append(out, v)
	}
	return out
}
