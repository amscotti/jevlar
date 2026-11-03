package app

import (
	"crypto/tls"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/amscotti/jevlar/internal/runtime"
)

// Transport bounds that are not configuration knobs: they are fixed by
// what a gateway needs, not by what an operator should tune.
const (
	// idleConnTimeout retires an idle pooled connection.
	idleConnTimeout = 90 * time.Second
	// dialTimeout bounds the TCP connect for one upstream attempt.
	dialTimeout = 10 * time.Second
	// tlsHandshakeTimeout bounds the TLS handshake for one upstream attempt.
	tlsHandshakeTimeout = 10 * time.Second
	// expectContinueTimeout bounds waiting for a 100-continue before
	// sending the body.
	expectContinueTimeout = 1 * time.Second
)

// transportKey identifies one pooled transport. Providers sharing an
// origin, concurrency cap and timeout share the pool; anything that
// changes the pool's behaviour is part of the key.
type transportKey struct {
	origin   string
	maxConns int
	timeout  time.Duration
}

type transportEntry struct {
	transport *http.Transport
	refs      int
}

type slotsEntry struct {
	slots *runtime.Slots
	refs  int
}

// Registry owns the process-lifetime upstream pools: the pooled
// http.Transport and the concurrency Slots for every provider. It is
// created once by Serve and survives reloads, so a reload cannot orphan a
// connection pool or reset occupancy under in-flight requests.
//
// A snapshot acquires handles with Apply and gives them back with
// Release; the last release for an entry closes its idle connections and
// forgets it. Transport.CloseIdleConnections is safe while requests are
// in flight: it only retires idle sockets.
type Registry struct {
	mu         sync.Mutex
	transports map[transportKey]*transportEntry
	slots      map[string]*slotsEntry
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		transports: make(map[transportKey]*transportEntry),
		slots:      make(map[string]*slotsEntry),
	}
}

// Apply borrows the pool and slot handles for every provider in rt,
// creating them on first use. Call it once per snapshot, before
// publishing it.
func (r *Registry) Apply(rt *runtime.Runtime) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range rt.Providers {
		key := transportKey{
			origin:   p.BaseURL.Scheme + "://" + p.BaseURL.Host,
			maxConns: p.MaxConc,
			timeout:  p.Timeout,
		}
		entry := r.transports[key]
		if entry == nil {
			entry = &transportEntry{transport: newTransport(p.MaxConc, p.Timeout)}
			r.transports[key] = entry
		}
		entry.refs++
		p.Transport = entry.transport

		// Slots are per provider name, not per origin: two providers
		// pointing at the same host are separately rate-limited by how
		// they were configured.
		slotKey := p.Name + "|" + strconv.Itoa(p.MaxConc)
		se := r.slots[slotKey]
		if se == nil {
			se = &slotsEntry{slots: runtime.NewSlots(p.MaxConc)}
			r.slots[slotKey] = se
		}
		se.refs++
		p.Slots = se.slots
	}
}

// Release gives back the handles a retired snapshot borrowed. Entries
// whose last reference is gone close their idle connections and are
// forgotten.
func (r *Registry) Release(rt *runtime.Runtime) {
	if rt == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range rt.Providers {
		if p.Transport == nil {
			continue // never applied (e.g. a snapshot that failed to publish)
		}
		key := transportKey{
			origin:   p.BaseURL.Scheme + "://" + p.BaseURL.Host,
			maxConns: p.MaxConc,
			timeout:  p.Timeout,
		}
		if entry := r.transports[key]; entry != nil {
			entry.refs--
			if entry.refs <= 0 {
				entry.transport.CloseIdleConnections()
				delete(r.transports, key)
			}
		}
		slotKey := p.Name + "|" + strconv.Itoa(p.MaxConc)
		if se := r.slots[slotKey]; se != nil {
			se.refs--
			if se.refs <= 0 {
				delete(r.slots, slotKey)
			}
		}
	}
}

// Close retires every pool. It is used at process shutdown.
func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, entry := range r.transports {
		entry.transport.CloseIdleConnections()
		delete(r.transports, key)
	}
	clear(r.slots)
}

// newTransport builds the pooled transport for one provider. Upstream
// responses are always requested uncompressed so the usage block stays
// readable (see ADR 0003); auto-decompression is therefore disabled.
func newTransport(maxConns int, timeout time.Duration) *http.Transport {
	if maxConns < 1 {
		maxConns = 1
	}
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          maxConns * 2,
		MaxIdleConnsPerHost:   maxConns,
		MaxConnsPerHost:       maxConns,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
		ResponseHeaderTimeout: timeout,
		DisableCompression:    true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
}
