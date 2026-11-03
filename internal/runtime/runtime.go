// Package runtime holds the immutable Runtime snapshot a request is served
// against: the validated configuration, the model-alias routing table and
// the immutable provider views with their resolved key material.
// Snapshots are swapped wholesale by internal/app; a request loads a
// snapshot once at admission and never observes a reload mid-flight.
//
// This package owns the configuration hash: a SHA-256 over a canonical
// serialisation of the resolved configuration (see canonical.go), so
// comments and formatting never count as a change while any resolved
// non-secret value does. Secret values are deliberately excluded — the
// hash is published by /admin/status and must not become a verifier of
// key material.
//
// The package is the only one that composes the configuration with
// routing and key material. Transports and key pools are borrowed from
// later subsystems and are not part of the snapshot yet.
package runtime

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/amscotti/jevlar/internal/config"
	"github.com/amscotti/jevlar/internal/health"
	"github.com/amscotti/jevlar/internal/keyring"
)

// Runtime is one immutable serving snapshot. Build one with Build; app
// publishes it via atomic.Pointer and admin and the proxy read the same
// pointer.
type Runtime struct {
	// Config is the resolved, validated configuration this snapshot was
	// built from.
	Config *config.Config
	// ConfigHash is the SHA-256 of the canonical serialisation of Config
	// (resolved values, secrets excluded); it identifies the
	// configuration /admin/status reports.
	ConfigHash string
	// Models maps a model alias to its ordered targets; the slice is the
	// failover order.
	Models map[string][]Target
	// Providers maps a provider name to its immutable view.
	Providers map[string]*Provider
	// StartedAt is stamped by Build (snapshot creation); app overwrites
	// it with the process boot time on every snapshot so uptime is
	// process uptime, not since-the-last-reload.
	StartedAt time.Time
	// Clients maps the SHA-256 of an inbound client token to its client
	// view. Lookup is by digest, so the presented token is hashed once and
	// never compared against secrets in a loop.
	Clients map[[sha256.Size]byte]*Client
	// Health is the process-lifetime health registry this snapshot borrows
	// key state, breakers and account limits from. It outlives reloads.
	Health *health.Registry
}

// Client is one admitted inbound caller.
type Client struct {
	// ID identifies the client in logs, metrics and budgets.
	ID string
	// RPM is the client's requests-per-minute budget; 0 means unlimited.
	RPM int
	// TPM is the client's tokens-per-minute budget; 0 means unlimited.
	TPM int
}

// Target is one (provider, model) routing step.
type Target struct {
	// Provider names the target provider.
	Provider string
	// Model is the model id sent upstream; it may differ from the
	// requested alias.
	Model string
}

// Provider is the immutable serving view of one configured provider. Key
// material is held in Keys; it is memory-only and never serialised.
type Provider struct {
	// Name identifies the provider.
	Name string
	// BaseURL is the parsed provider origin.
	BaseURL *url.URL
	// Path is the upstream request path.
	Path string
	// Auth is the upstream auth mode ("bearer" or "none").
	Auth string
	// Account names the provider's limit group; empty when ungrouped.
	Account string
	// Timeout bounds one upstream attempt on this provider.
	Timeout time.Duration
	// MaxConc caps the provider's concurrent upstream requests.
	MaxConc int
	// MaxResponseBytes caps one upstream response body; a larger response
	// aborts the attempt rather than being streamed or truncated.
	MaxResponseBytes int64
	// Keys is the provider's key pool with resolved secrets.
	Keys []KeySpec
	// Transport is the pooled HTTP transport for this provider. It is
	// owned by the process-lifetime registry in internal/app, which reuses
	// it across reloads and closes it only when the provider is removed;
	// the snapshot only borrows it and never closes it.
	Transport *http.Transport
	// Slots bounds the provider's concurrent upstream requests. Like
	// Transport it is owned by internal/app and borrowed here, so a
	// reload cannot reset occupancy under in-flight requests.
	Slots *Slots
	// Pool selects among the provider's keys using process-lifetime key
	// state. internal/app builds it after the health registry is reconciled;
	// auth = "none" providers receive an empty pool that the proxy never
	// consults.
	Pool *keyring.Pool
}

// Secret returns the resolved secret for a key id. The id comes from the
// health registry's Key, which never holds the secret itself.
func (p *Provider) Secret(id string) (string, bool) {
	for i := range p.Keys {
		if p.Keys[i].ID == id {
			return p.Keys[i].Value, true
		}
	}
	return "", false
}

// KeySpec is one provider key with its resolved secret. The value lives
// in memory only: it is never written to logs, metrics or /admin/status
// and is excluded from the configuration hash.
type KeySpec struct {
	// ID identifies the key in logs, metrics and /admin/status.
	ID string
	// Value is the resolved secret (expanded inline value or file
	// contents).
	Value string
}

// Build turns a validated configuration into a Runtime snapshot: provider
// views with parsed origins and resolved keys, the model routing table,
// and the configuration hash. It does not re-run validation; callers run
// Config.Validate first. It returns an error (field-anchored where
// possible) when a value that validation should have checked does not
// parse — a caller that skips validation gets a fail-closed error, not a
// panic.
func Build(cfg *config.Config) (*Runtime, error) {
	providers := make(map[string]*Provider, len(cfg.Providers))
	for i := range cfg.Providers {
		p := cfg.Providers[i]
		base, err := url.Parse(p.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("providers[%d].base_url: %w", i, err)
		}
		if base.Host == "" {
			return nil, fmt.Errorf("providers[%d].base_url: missing host", i)
		}
		keys := make([]KeySpec, 0, len(p.APIKeys))
		for _, k := range p.APIKeys {
			keys = append(keys, KeySpec{ID: k.ID, Value: k.Resolved})
		}
		providers[p.Name] = &Provider{
			Name:             p.Name,
			BaseURL:          base,
			Path:             p.Path,
			Auth:             p.Auth,
			Account:          p.Account,
			Timeout:          time.Duration(p.TimeoutMs) * time.Millisecond,
			MaxConc:          p.MaxConcurrency,
			MaxResponseBytes: int64(cfg.Server.MaxResponseBytes),
			Keys:             keys,
		}
	}

	clients := make(map[[sha256.Size]byte]*Client, len(cfg.Auth.Keys))
	for _, k := range cfg.Auth.Keys {
		digest, err := ParseClientHash(k.Hash)
		if err != nil {
			return nil, fmt.Errorf("auth.keys[%q].hash: %w", k.ID, err)
		}
		clients[digest] = &Client{ID: k.ID, RPM: k.RPM, TPM: k.TPM}
	}

	models := make(map[string][]Target, len(cfg.Models))
	for alias, m := range cfg.Models {
		targets := make([]Target, 0, len(m.Targets))
		for _, t := range m.Targets {
			targets = append(targets, Target{Provider: t.Provider, Model: t.Model})
		}
		models[alias] = targets
	}

	return &Runtime{
		Config:     cfg,
		ConfigHash: ConfigHash(cfg),
		Models:     models,
		Providers:  providers,
		Clients:    clients,
		StartedAt:  time.Now(),
	}, nil
}
