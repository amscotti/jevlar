package app

import (
	"time"

	"github.com/amscotti/jevlar/internal/config"
	"github.com/amscotti/jevlar/internal/health"
	"github.com/amscotti/jevlar/internal/keyring"
	"github.com/amscotti/jevlar/internal/runtime"
)

// healthDesired derives the process-lifetime identities a configuration
// wants to exist. Reconcile keeps matching entries' state and creates or
// retires the rest.
func healthDesired(cfg *config.Config) health.Desired {
	d := health.Desired{
		Keys:      make([]health.KeyRef, 0, len(cfg.Providers)),
		Providers: make([]health.ProviderRef, 0, len(cfg.Providers)),
		Accounts:  make([]health.AccountRef, 0, len(cfg.Accounts)),
		Clients:   make([]health.ClientRef, 0, len(cfg.Auth.Keys)),
	}
	for i := range cfg.Providers {
		p := cfg.Providers[i]
		d.Providers = append(d.Providers, health.ProviderRef{
			Name:             p.Name,
			BreakerThreshold: cfg.Breaker.Threshold,
			BreakerCooldown:  time.Duration(cfg.Breaker.CooldownMs) * time.Millisecond,
		})
		for _, k := range p.APIKeys {
			d.Keys = append(d.Keys, health.KeyRef{Provider: p.Name, ID: k.ID})
		}
	}
	for name, a := range cfg.Accounts {
		d.Accounts = append(d.Accounts, health.AccountRef{Name: name, RPM: a.RPM, TPM: a.TPM})
	}
	for _, k := range cfg.Auth.Keys {
		d.Clients = append(d.Clients, health.ClientRef{ID: k.ID, RPM: k.RPM, TPM: k.TPM})
	}
	return d
}

// attachHealth reconciles the registry against cfg and wires the
// resulting key pools into the freshly built snapshot. Registry state
// survives the reload; only identities are added or removed.
func (s *server) attachHealth(cfg *config.Config, rt *runtime.Runtime) {
	s.health.Reconcile(healthDesired(cfg))
	rt.Health = s.health
	for name, p := range rt.Providers {
		ids := make([]string, 0, len(p.Keys))
		for _, k := range p.Keys {
			ids = append(ids, k.ID)
		}
		pool := keyring.NewPool(name, s.health.KeysFor(name, ids))
		p.Pool = pool
		for _, st := range pool.Status() {
			var v float64
			switch st.State {
			case "cooldown":
				v = 1
			case "disabled":
				v = 2
			}
			s.metrics.KeyState(name, st.ID, v)
		}
		if b := s.health.Breaker(name); b != nil {
			s.syncBreakerGauge(name, b)
		}
	}
	for name := range cfg.Accounts {
		if acc := s.health.Account(name); acc != nil {
			secs := 0.0
			if cooling, until := acc.Cooling(); cooling {
				secs = time.Until(until).Seconds()
			}
			s.metrics.AccountCooldown(name, secs)
		}
	}
	// Gauge series for identities the reload removed would otherwise stay
	// exposed at their last value forever.
	keysByProvider := make(map[string]map[string]bool, len(rt.Providers))
	providerNames := make([]string, 0, len(rt.Providers))
	for name, p := range rt.Providers {
		providerNames = append(providerNames, name)
		ids := make(map[string]bool, len(p.Keys))
		for _, k := range p.Keys {
			ids[k.ID] = true
		}
		keysByProvider[name] = ids
	}
	accountNames := make([]string, 0, len(cfg.Accounts))
	for name := range cfg.Accounts {
		accountNames = append(accountNames, name)
	}
	s.metrics.PruneIdentities(keysByProvider, providerNames, accountNames)
}

// syncBreakerGauge publishes a breaker's state as a gauge.
func (s *server) syncBreakerGauge(provider string, b *health.Breaker) {
	var v float64
	if b.State() == health.StateCooldown {
		if b.Open() {
			v = 2
		} else {
			v = 1
		}
	}
	s.metrics.BreakerState(provider, v)
}
