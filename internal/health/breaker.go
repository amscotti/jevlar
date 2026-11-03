package health

import (
	"sync"
	"time"
)

// probeTimeout bounds how long a half-open probe may hold the breaker in
// its probing state before another probe is allowed. Without it, a probe
// whose caller never records an outcome would wedge the breaker open.
const probeTimeout = 30 * time.Second

// Breaker is a consecutive-failure circuit breaker for one provider.
//
// closed → threshold consecutive failures → open → cooldown → half-open
// (exactly one probe) → success closes, failure re-opens.
type Breaker struct {
	mu        sync.Mutex
	threshold int
	cooldown  time.Duration
	consec    int
	openUntil time.Time
	probing   bool
	probedAt  time.Time
	now       func() time.Time
}

func newBreaker(threshold int, cooldown time.Duration, now func() time.Time) *Breaker {
	b := &Breaker{now: now}
	b.configure(threshold, cooldown)
	return b
}

// configure applies reloaded settings without discarding the current
// state: a reload can change the threshold of a breaker that is open, and
// the breaker stays open. Disabling the breaker (threshold <= 0) clears
// its state, so the admin surface cannot report a block that Allow no
// longer applies.
func (b *Breaker) configure(threshold int, cooldown time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.threshold = threshold
	b.cooldown = cooldown
	if threshold <= 0 {
		b.consec = 0
		b.openUntil = time.Time{}
		b.probing = false
	}
}

// State reports the breaker's condition at the registry clock.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.threshold <= 0 {
		return StateHealthy
	}
	now := b.now()
	if b.openUntil.IsZero() || !now.Before(b.openUntil) {
		if !b.openUntil.IsZero() {
			return StateCooldown // half-open: cooldown elapsed, probing
		}
		return StateHealthy
	}
	return StateCooldown
}

// Open reports whether the breaker is currently blocking traffic.
func (b *Breaker) Open() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.threshold <= 0 {
		return false
	}
	return b.now().Before(b.openUntil)
}

// Allow reports whether one request may go to this provider. In the
// half-open window exactly one caller is admitted; the rest are refused
// until that probe records an outcome or its probe timeout elapses.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.threshold <= 0 {
		return true // breaker disabled by configuration
	}
	now := b.now()
	if now.Before(b.openUntil) {
		return false
	}
	if b.openUntil.IsZero() {
		return true // closed
	}
	// Half-open: one probe at a time.
	if b.probing {
		if now.Sub(b.probedAt) < probeTimeout {
			return false
		}
		// The previous probe never reported; let another try.
	}
	b.probing = true
	b.probedAt = now
	return true
}

// ReleaseProbe abandons a half-open probe that never produced a verdict
// (for example because the provider's concurrency slots were exhausted, so
// no upstream call was made). Without it the probe flag would block every
// other request until the probe timeout.
func (b *Breaker) ReleaseProbe() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
}

// RecordSuccess closes the breaker and clears the failure count. A success
// recorded while the breaker is open can only come from a request admitted
// before it opened; it proves nothing about the current outage and must
// not close the breaker mid-cooldown.
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.threshold <= 0 {
		b.consec = 0
		b.openUntil = time.Time{}
		b.probing = false
		return
	}
	if b.now().Before(b.openUntil) {
		return
	}
	b.consec = 0
	b.openUntil = time.Time{}
	b.probing = false
}

// RecordFailure increments the consecutive-failure count and opens the
// breaker when it reaches the threshold. A failed half-open probe re-opens
// the breaker for another full cooldown immediately: the single probe is
// the evidence the outage continues, regardless of the threshold.
func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	wasProbe := b.probing
	b.probing = false
	if b.threshold <= 0 {
		return
	}
	if wasProbe {
		b.consec = 0
		b.openUntil = b.now().Add(b.cooldown)
		return
	}
	b.consec++
	if b.consec >= b.threshold {
		b.openUntil = b.now().Add(b.cooldown)
		b.consec = 0
	}
}

// ConsecutiveFailures reports the current consecutive-failure count.
func (b *Breaker) ConsecutiveFailures() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.consec
}
