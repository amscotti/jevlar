package runtime

// Slots is a non-blocking counting semaphore: it admits at most Cap()
// concurrent holders and never queues. It is a process-lifetime handle
// owned by internal/app and borrowed by every Runtime snapshot, so a
// reload does not reset occupancy while requests are in flight.
//
// The zero value is not usable; construct with NewSlots.
type Slots struct {
	ch chan struct{}
}

// NewSlots returns a Slots admitting n concurrent holders. n must be at
// least 1; callers validate max_concurrency before reaching this.
func NewSlots(n int) *Slots {
	if n < 1 {
		n = 1
	}
	return &Slots{ch: make(chan struct{}, n)}
}

// TryAcquire takes one slot without blocking. It reports false when every
// slot is taken; the caller must then try another target and must not
// call Release.
func (s *Slots) TryAcquire() bool {
	select {
	case s.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release returns one slot. Releasing without a matching successful
// TryAcquire panics on a closed channel only if over-released past Cap;
// callers pair it with the acquisition.
func (s *Slots) Release() {
	select {
	case <-s.ch:
	default:
		// Over-release is a programmer error, but a panic in the request
		// path would take the process down; the counter simply does not
		// go negative.
	}
}

// Cap reports the number of slots.
func (s *Slots) Cap() int { return cap(s.ch) }

// InUse reports how many slots are currently held.
func (s *Slots) InUse() int { return len(s.ch) }
