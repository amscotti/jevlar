package keyring

import (
	"testing"
	"time"

	"github.com/amscotti/jevlar/internal/health"
)

// newPool builds a pool of n keys with a deterministic "random" source.
func newPool(t *testing.T, n int, rnd func(int) int) (*Pool, *health.Registry) {
	t.Helper()
	reg := health.New()
	refs := make([]health.KeyRef, 0, n)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := string(rune('a' + i))
		refs = append(refs, health.KeyRef{Provider: "p", ID: id})
		ids = append(ids, id)
	}
	reg.Reconcile(health.Desired{Keys: refs})
	p := NewPool("p", reg.KeysFor("p", ids))
	if rnd != nil {
		p.SetRand(rnd)
	}
	return p, reg
}

// TestPool_AcquireReleasesAndCountsInflight pins the acquire/release
// contract the proxy relies on.
func TestPool_AcquireReleasesAndCountsInflight(t *testing.T) {
	t.Parallel()
	p, _ := newPool(t, 2, func(int) int { return 0 })
	k, ok := p.Acquire()
	if !ok || k == nil {
		t.Fatal("acquire failed with healthy keys")
	}
	if k.Inflight() != 1 {
		t.Fatalf("inflight = %d, want 1", k.Inflight())
	}
	p.Release(k)
	if k.Inflight() != 0 {
		t.Fatalf("inflight after release = %d, want 0", k.Inflight())
	}
}

// TestPool_SkipsCoolingKeys pins rotation: a cooled key is not selected
// while a healthy one exists, even when the "random" draw lands on it.
func TestPool_SkipsCoolingKeys(t *testing.T) {
	t.Parallel()
	p, _ := newPool(t, 2, func(int) int { return 0 }) // always draws index 0
	first := p.Keys()[0]
	first.CoolFor(time.Minute)

	k, ok := p.Acquire()
	if !ok {
		t.Fatal("acquire failed although the second key is healthy")
	}
	if k.ID() == first.ID() {
		t.Fatalf("selected cooling key %q", k.ID())
	}
	p.Release(k)
}

// TestPool_AllUnavailableReportsFalse pins exhaustion.
func TestPool_AllUnavailableReportsFalse(t *testing.T) {
	t.Parallel()
	p, _ := newPool(t, 3, func(int) int { return 1 })
	for _, k := range p.Keys() {
		k.CoolFor(time.Minute)
	}
	if _, ok := p.Acquire(); ok {
		t.Fatal("acquired a key although every key is cooling")
	}
	if p.Available() {
		t.Fatal("Available() = true with every key cooling")
	}
}

// TestPool_EarliestCooldownIsTheRetryAfterValue pins the exhaustion hint.
func TestPool_EarliestCooldownIsTheRetryAfterValue(t *testing.T) {
	t.Parallel()
	p, reg := newPool(t, 3, func(int) int { return 0 })
	keys := p.Keys()
	keys[0].CoolFor(9 * time.Second)
	keys[1].CoolFor(3 * time.Second)
	keys[2].CoolFor(20 * time.Second)

	earliest := p.EarliestCooldown()
	want := reg.Now().Add(3 * time.Second)
	if delta := earliest.Sub(want); delta < -time.Millisecond || delta > time.Millisecond {
		t.Fatalf("earliest = %v, want about %v", earliest, want)
	}
}

// TestPool_DisabledKeyIsNeverSelectedEvenWhenRandomDrawsIt pins that the
// fallback scan respects disable.
func TestPool_DisabledKeyIsNeverSelectedEvenWhenRandomDrawsIt(t *testing.T) {
	t.Parallel()
	p, _ := newPool(t, 2, func(int) int { return 0 })
	p.Keys()[0].Disable("revoked")
	for i := 0; i < 50; i++ {
		k, ok := p.Acquire()
		if !ok {
			t.Fatal("acquire failed although one key is healthy")
		}
		if k.State() == health.StateDisabled {
			t.Fatal("selected a disabled key")
		}
		p.Release(k)
	}
}

// TestPool_StatusReportsEveryKey pins the admin surface.
func TestPool_StatusReportsEveryKey(t *testing.T) {
	t.Parallel()
	p, _ := newPool(t, 2, nil)
	p.Keys()[1].Disable("revoked")
	status := p.Status()
	if len(status) != 2 {
		t.Fatalf("status has %d entries, want 2", len(status))
	}
	if status[1].State != health.StateDisabled || status[1].Reason == "" {
		t.Fatalf("disabled key status = %+v", status[1])
	}
}

// TestPool_EmptyReportsNothing pins the zero-key pool (auth = none).
func TestPool_EmptyReportsNothing(t *testing.T) {
	t.Parallel()
	p := NewPool("p", nil)
	if _, ok := p.Acquire(); ok {
		t.Fatal("empty pool acquired a key")
	}
	if p.Len() != 0 || p.Available() {
		t.Fatal("empty pool reports capacity")
	}
	if !p.EarliestCooldown().IsZero() {
		t.Fatal("empty pool reported a cooldown")
	}
}

// TestPool_ProviderName pins the identity accessor.
func TestPool_ProviderName(t *testing.T) {
	t.Parallel()
	p := NewPool("upstream-a", nil)
	if p.Provider() != "upstream-a" {
		t.Fatalf("Provider() = %q", p.Provider())
	}
}

// TestPool_SingleKeyAndPoolAvailability pins the one-key fast path and the
// Available projection.
func TestPool_SingleKeyAndPoolAvailability(t *testing.T) {
	t.Parallel()
	p, _ := newPool(t, 1, func(int) int { return 0 })
	if !p.Available() {
		t.Fatal("single healthy key reported unavailable")
	}
	k, ok := p.Acquire()
	if !ok {
		t.Fatal("single healthy key not acquired")
	}
	p.Release(k)
	k.CoolFor(time.Minute)
	if p.Available() {
		t.Fatal("single cooling key reported available")
	}
	if _, ok := p.Acquire(); ok {
		t.Fatal("acquired a cooling single key")
	}
}

// TestPool_AcquireWithoutRandomSourceUsesTheDefault pins the constructor
// default so a nil SetRand is never a nil call.
func TestPool_AcquireWithoutRandomSourceUsesTheDefault(t *testing.T) {
	t.Parallel()
	p, _ := newPool(t, 3, nil)
	for i := 0; i < 20; i++ {
		k, ok := p.Acquire()
		if !ok {
			t.Fatal("acquire failed with three healthy keys")
		}
		p.Release(k)
	}
}
