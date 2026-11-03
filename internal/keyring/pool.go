// Package keyring selects which provider key serves a request. It sits
// between the proxy and the process-lifetime health registry: the registry
// owns key state, the pool owns the selection policy.
//
// Selection is power-of-two: two candidates are drawn at random and the
// first healthy one wins, which keeps the hot path O(1) and avoids
// herding every request onto the first key. When both drawn candidates
// are unavailable the pool falls back to a linear scan — key pools are
// small (single digits), and returning "no capacity" while a healthy key
// exists would be worse than an O(n) scan on the unhappy path.
package keyring

import (
	"math/rand/v2"
	"time"

	"github.com/amscotti/jevlar/internal/health"
)

// Pool is the key pool of one provider. Create it with NewPool after the
// health registry is reconciled.
type Pool struct {
	provider string
	keys     []*health.Key
	randIntN func(int) int
}

// NewPool returns a pool over the given key states. The slice order is
// the configuration order and is preserved for status output.
func NewPool(provider string, keys []*health.Key) *Pool {
	return &Pool{provider: provider, keys: keys, randIntN: rand.IntN}
}

// SetRand replaces the random source, for deterministic tests.
func (p *Pool) SetRand(n func(int) int) { p.randIntN = n }

// Provider returns the pool's provider name.
func (p *Pool) Provider() string { return p.provider }

// Len reports how many keys the pool holds.
func (p *Pool) Len() int { return len(p.keys) }

// Keys returns the key states in configuration order.
func (p *Pool) Keys() []*health.Key { return p.keys }

// Acquire selects a healthy key and increments its in-flight counter. It
// reports false when every key is cooling or disabled. A caller that gets
// true must call Release with the same key.
func (p *Pool) Acquire() (*health.Key, bool) {
	n := len(p.keys)
	if n == 0 {
		return nil, false
	}
	if n == 1 {
		k := p.keys[0]
		if !k.Available() {
			return nil, false
		}
		k.Acquire()
		return k, true
	}

	first := p.randIntN(n)
	second := p.randIntN(n)
	if k := p.keys[first]; k.Available() {
		k.Acquire()
		return k, true
	}
	if k := p.keys[second]; k.Available() {
		k.Acquire()
		return k, true
	}
	// Both drawn candidates are unavailable; a healthy key may still exist.
	for _, k := range p.keys {
		if k.Available() {
			k.Acquire()
			return k, true
		}
	}
	return nil, false
}

// Release returns a key acquired by Acquire.
func (p *Pool) Release(k *health.Key) {
	if k != nil {
		k.Release()
	}
}

// EarliestCooldown returns the soonest point at which any key of the pool
// becomes usable again, or the zero time when no key is cooling. It is
// what "every key is cooling" responses put in Retry-After.
func (p *Pool) EarliestCooldown() time.Time {
	var earliest time.Time
	for _, k := range p.keys {
		if k.State() == health.StateDisabled {
			continue
		}
		until := k.CooldownUntil()
		if until.IsZero() {
			continue
		}
		if earliest.IsZero() || until.Before(earliest) {
			earliest = until
		}
	}
	return earliest
}

// Status returns the pool's keys as status snapshots for /admin/status.
func (p *Pool) Status() []health.KeyStatus {
	out := make([]health.KeyStatus, 0, len(p.keys))
	for _, k := range p.keys {
		out = append(out, k.Status())
	}
	return out
}

// Available reports whether at least one key can serve right now.
func (p *Pool) Available() bool {
	for _, k := range p.keys {
		if k.Available() {
			return true
		}
	}
	return false
}
