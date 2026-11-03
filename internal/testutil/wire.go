package testutil

import (
	"net/http"
	"time"

	"github.com/amscotti/jevlar/internal/health"
	"github.com/amscotti/jevlar/internal/keyring"
	"github.com/amscotti/jevlar/internal/runtime"
)

// Wire assigns a pooled transport, a concurrency slot pool and a keyring
// pool (backed by a fresh health registry) to every provider in rt,
// mirroring what internal/app does at boot. It exists so package tests can
// exercise the proxy without importing app. The returned function closes
// the idle connections.
func Wire(rt *runtime.Runtime) func() {
	return WireWithHealth(rt, health.New())
}

// WireWithHealth is Wire against a caller-supplied registry, so a test can
// keep state across snapshots (the reload-continuity case).
func WireWithHealth(rt *runtime.Runtime, reg *health.Registry) func() {
	desired := health.Desired{}
	for i := range rt.Config.Providers {
		p := rt.Config.Providers[i]
		desired.Providers = append(desired.Providers, health.ProviderRef{
			Name:             p.Name,
			BreakerThreshold: rt.Config.Breaker.Threshold,
			BreakerCooldown:  time.Duration(rt.Config.Breaker.CooldownMs) * time.Millisecond,
		})
		for _, k := range p.APIKeys {
			desired.Keys = append(desired.Keys, health.KeyRef{Provider: p.Name, ID: k.ID})
		}
	}
	for name, a := range rt.Config.Accounts {
		desired.Accounts = append(desired.Accounts, health.AccountRef{Name: name, RPM: a.RPM, TPM: a.TPM})
	}
	for _, k := range rt.Config.Auth.Keys {
		desired.Clients = append(desired.Clients, health.ClientRef{ID: k.ID, RPM: k.RPM, TPM: k.TPM})
	}
	reg.Reconcile(desired)
	rt.Health = reg

	transports := make([]*http.Transport, 0, len(rt.Providers))
	for name, p := range rt.Providers {
		t := &http.Transport{
			MaxIdleConnsPerHost: p.MaxConc,
			MaxConnsPerHost:     p.MaxConc,
			IdleConnTimeout:     time.Minute,
			DisableCompression:  true,
		}
		p.Transport = t
		p.Slots = runtime.NewSlots(p.MaxConc)
		ids := make([]string, 0, len(p.Keys))
		for _, k := range p.Keys {
			ids = append(ids, k.ID)
		}
		p.Pool = keyring.NewPool(name, reg.KeysFor(name, ids))
		transports = append(transports, t)
	}
	return func() {
		for _, t := range transports {
			t.CloseIdleConnections()
		}
	}
}
