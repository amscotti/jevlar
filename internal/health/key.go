package health

import (
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

// Backoff parameters for a rate-limited key without a Retry-After header.
const (
	// backoffBase is the first backoff step.
	backoffBase = time.Second
	// backoffMax caps the exponential backoff.
	backoffMax = 30 * time.Second
	// backoffJitter is the ± fraction applied to a computed backoff.
	backoffJitter = 0.2
)

// Key is the process-lifetime state of one provider key.
//
// Status is derived rather than stored: a key is disabled if an operator
// action (here: an upstream 401) disabled it, cooling while its cooldown
// deadline is in the future, and healthy otherwise. Deriving it means the
// three fields can never disagree.
type Key struct {
	provider string
	id       string

	inflight      atomic.Int64
	cooldownUntil atomic.Int64 // Unix nanoseconds; 0 = never cooled
	disabled      atomic.Bool
	reason        atomic.Pointer[string]
	consecutive   atomic.Int32

	// mu guards transitions so a disabled key cannot be un-disabled by a
	// concurrent cooldown.
	mu sync.Mutex

	now func() time.Time
}

func newKey(now func() time.Time) *Key {
	return &Key{now: now}
}

// Provider returns the key's provider name.
func (k *Key) Provider() string { return k.provider }

// ID returns the key's configured id.
func (k *Key) ID() string { return k.id }

// State reports the key's condition at the registry clock.
func (k *Key) State() State {
	if k.disabled.Load() {
		return StateDisabled
	}
	if until := k.cooldownUntil.Load(); until != 0 && k.now().UnixNano() < until {
		return StateCooldown
	}
	return StateHealthy
}

// Available reports whether the key may serve a request now.
func (k *Key) Available() bool { return k.State() == StateHealthy }

// CooldownUntil returns the cooldown deadline, or the zero time when the
// key is not cooling.
func (k *Key) CooldownUntil() time.Time {
	until := k.cooldownUntil.Load()
	if until == 0 {
		return time.Time{}
	}
	return time.Unix(0, until)
}

// DisabledReason returns the operator-facing reason a key is disabled, or
// "" when it is not disabled.
func (k *Key) DisabledReason() string {
	if p := k.reason.Load(); p != nil {
		return *p
	}
	return ""
}

// Inflight reports how many requests currently hold this key.
func (k *Key) Inflight() int64 { return k.inflight.Load() }

// Acquire increments the in-flight counter. Pair it with Release.
func (k *Key) Acquire() { k.inflight.Add(1) }

// Release decrements the in-flight counter, never below zero. An
// over-release is a programmer error; the compare-and-swap keeps a
// concurrent legitimate Acquire from being erased by the clamp.
func (k *Key) Release() {
	for {
		cur := k.inflight.Load()
		if cur <= 0 {
			return
		}
		if k.inflight.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// CoolFor puts the key into cooldown until at least now+d and extends an
// existing cooldown. A disabled key stays disabled: cooling never
// resurrects it. The consecutive-failure counter is deliberately left
// alone — ResetConsecutive clears it on success — so consecutive rate
// limits keep backing off further.
func (k *Key) CoolFor(d time.Duration) {
	if d <= 0 {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	until := k.now().Add(d).UnixNano()
	if cur := k.cooldownUntil.Load(); cur > until {
		until = cur
	}
	k.cooldownUntil.Store(until)
}

// Disable marks the key unusable until an operator re-enables it. Reloads
// preserve this state; only Enable clears it.
func (k *Key) Disable(reason string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.disabled.Store(true)
	r := reason
	k.reason.Store(&r)
}

// Enable clears a disabled state and any cooldown. It is the operator
// action the plan requires for a revoked-then-restored key.
func (k *Key) Enable() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.disabled.Store(false)
	k.reason.Store(nil)
	k.cooldownUntil.Store(0)
	k.consecutive.Store(0)
}

// NoteRateLimited cools the key for the upstream's Retry-After when it
// supplied one, and otherwise for a jittered exponential backoff that
// grows with consecutive rate limits and is capped at 30 s.
func (k *Key) NoteRateLimited(retryAfter time.Duration) {
	if retryAfter > 0 {
		k.CoolFor(retryAfter)
		return
	}
	n := k.consecutive.Add(1)
	d := backoffBase
	for i := int32(1); i < n && d < backoffMax; i++ {
		d *= 2
	}
	if d > backoffMax {
		d = backoffMax
	}
	k.CoolFor(withJitter(d))
}

// ResetConsecutive clears the backoff counter after a success.
func (k *Key) ResetConsecutive() { k.consecutive.Store(0) }

// withJitter applies ±backoffJitter to d using the global source. The
// randomness only spreads retries out; it is not secret material, so a
// non-cryptographic source is correct here.
func withJitter(d time.Duration) time.Duration {
	factor := 1 + (rand.Float64()*2-1)*backoffJitter //nolint:gosec // jitter is not security-sensitive
	return time.Duration(float64(d) * factor)
}
