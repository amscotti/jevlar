package health

import (
	"sync"
	"time"
)

// State is the externally visible condition of a key or breaker.
type State string

const (
	// StateHealthy means the entry may be used.
	StateHealthy State = "healthy"
	// StateCooldown means the entry is temporarily unusable until a
	// deadline that an upstream (or the gateway's own backoff) set.
	StateCooldown State = "cooldown"
	// StateDisabled means the entry is unusable until an operator
	// re-enables it; reloads never clear it.
	StateDisabled State = "disabled"
)

// KeyRef identifies one provider key across reloads.
type KeyRef struct {
	// Provider is the provider name the key belongs to.
	Provider string
	// ID is the configured key id.
	ID string
}

// ProviderRef identifies one provider across reloads.
type ProviderRef struct {
	// Name is the provider name.
	Name string
	// BreakerThreshold is the consecutive-failure count that opens the
	// breaker; 0 disables the breaker.
	BreakerThreshold int
	// BreakerCooldown is how long an open breaker waits before probing.
	BreakerCooldown time.Duration
}

// AccountRef describes one account's provider-side limits. Zero values
// mean unlimited.
type AccountRef struct {
	// Name is the account name.
	Name string
	// RPM is the account's requests-per-minute budget.
	RPM int
	// TPM is the account's tokens-per-minute budget.
	TPM int
}

// ClientRef describes one inbound client's quota. Zero values mean
// unlimited.
type ClientRef struct {
	// ID is the client id.
	ID string
	// RPM is the client's requests-per-minute budget.
	RPM int
	// TPM is the client's tokens-per-minute budget.
	TPM int
}

// Desired is the full set of identities a configuration wants to exist.
// Reconcile retains, creates and retires to match it.
type Desired struct {
	// Keys are the provider keys that must exist.
	Keys []KeyRef
	// Providers are the providers that must exist.
	Providers []ProviderRef
	// Accounts are the accounts that must exist.
	Accounts []AccountRef
	// Clients are the inbound clients that must exist.
	Clients []ClientRef
}

// Registry is the process-lifetime health state. Create one with New and
// share it across reloads; it is safe for concurrent use.
type Registry struct {
	now func() time.Time

	routes *routes

	mu        sync.RWMutex
	keys      map[KeyRef]*Key
	providers map[string]*Breaker
	accounts  map[string]*Account
	clients   map[string]*Client
}

// New returns an empty Registry using the wall clock and a global random
// source.
func New() *Registry {
	return &Registry{
		routes:    newRoutes(),
		now:       time.Now,
		keys:      make(map[KeyRef]*Key),
		providers: make(map[string]*Breaker),
		accounts:  make(map[string]*Account),
		clients:   make(map[string]*Client),
	}
}

// SetClock replaces the clock used for every deadline computation. It must
// be called before the registry is shared.
func (r *Registry) SetClock(now func() time.Time) { r.now = now }

// Reconcile makes the registry match desired: existing entries keep their
// state, new entries start healthy, and entries absent from desired are
// dropped. It is called on every successful reload.
func (r *Registry) Reconcile(desired Desired) {
	r.mu.Lock()
	defer r.mu.Unlock()

	keys := make(map[KeyRef]*Key, len(desired.Keys))
	for _, ref := range desired.Keys {
		if existing, ok := r.keys[ref]; ok {
			keys[ref] = existing
			continue
		}
		k := newKey(r.now)
		k.provider = ref.Provider
		k.id = ref.ID
		keys[ref] = k
	}
	r.keys = keys

	providers := make(map[string]*Breaker, len(desired.Providers))
	for _, ref := range desired.Providers {
		if existing, ok := r.providers[ref.Name]; ok {
			existing.configure(ref.BreakerThreshold, ref.BreakerCooldown)
			providers[ref.Name] = existing
			continue
		}
		providers[ref.Name] = newBreaker(ref.BreakerThreshold, ref.BreakerCooldown, r.now)
	}
	r.providers = providers

	accounts := make(map[string]*Account, len(desired.Accounts))
	for _, ref := range desired.Accounts {
		if existing, ok := r.accounts[ref.Name]; ok {
			existing.configure(ref.RPM, ref.TPM)
			accounts[ref.Name] = existing
			continue
		}
		a := newAccount(ref.RPM, ref.TPM, r.now)
		a.name = ref.Name
		accounts[ref.Name] = a
	}
	r.accounts = accounts

	clients := make(map[string]*Client, len(desired.Clients))
	for _, ref := range desired.Clients {
		if existing, ok := r.clients[ref.ID]; ok {
			existing.configure(ref.RPM, ref.TPM)
			clients[ref.ID] = existing
			continue
		}
		c := newClient(ref.RPM, ref.TPM, r.now)
		c.id = ref.ID
		clients[ref.ID] = c
	}
	r.clients = clients
}

// Key returns the state for a configured key, or nil when the key is not
// in the current configuration.
func (r *Registry) Key(ref KeyRef) *Key {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.keys[ref]
}

// KeysFor returns the key states of one provider in configuration order,
// which is the pool's candidate list.
func (r *Registry) KeysFor(provider string, ids []string) []*Key {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Key, 0, len(ids))
	for _, id := range ids {
		if k, ok := r.keys[KeyRef{Provider: provider, ID: id}]; ok {
			out = append(out, k)
		}
	}
	return out
}

// Breaker returns the circuit breaker for a provider, or nil when the
// provider is not configured.
func (r *Registry) Breaker(provider string) *Breaker {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.providers[provider]
}

// Account returns the limit state for an account, or nil when the account
// is not configured.
func (r *Registry) Account(name string) *Account {
	if name == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.accounts[name]
}

// Client returns the quota state for an inbound client, or nil when the
// client is not configured.
func (r *Registry) Client(id string) *Client {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.clients[id]
}

// Now returns the registry's current time, so callers compute deadlines
// with the same clock the state was written with.
func (r *Registry) Now() time.Time { return r.now() }
