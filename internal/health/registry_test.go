package health

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestRegistry() (*Registry, *fakeClock) {
	c := newFakeClock()
	r := New()
	r.SetClock(c.Now)
	return r, c
}

func reconcileOneKey(t *testing.T, r *Registry, provider, id string) *Key {
	t.Helper()
	r.Reconcile(Desired{
		Keys:      []KeyRef{{Provider: provider, ID: id}},
		Providers: []ProviderRef{{Name: provider, BreakerThreshold: 5, BreakerCooldown: 30 * time.Second}},
	})
	return r.Key(KeyRef{Provider: provider, ID: id})
}

// TestRegistry_ReconcileRetainsStateForTheSameIdentity pins the core
// reload guarantee: state belongs to (provider, key id) and survives a
// reconcile that still contains it.
func TestRegistry_ReconcileRetainsStateForTheSameIdentity(t *testing.T) {
	t.Parallel()
	r, clock := newTestRegistry()
	k := reconcileOneKey(t, r, "p", "a")
	k.Disable("revoked")
	k.Acquire()

	// A reload with the same key keeps both the disabled state and the
	// in-flight count.
	again := reconcileOneKey(t, r, "p", "a")
	if again != k {
		t.Fatal("reconcile replaced the key state instead of retaining it")
	}
	if again.State() != StateDisabled {
		t.Fatalf("state = %s, want disabled after reload", again.State())
	}
	if again.Inflight() != 1 {
		t.Fatalf("inflight = %d, want 1", again.Inflight())
	}
	_ = clock
}

// TestRegistry_ReconcileDropsRemovedAndCreatesNew pins retirement and
// creation of identities.
func TestRegistry_ReconcileDropsRemovedAndCreatesNew(t *testing.T) {
	t.Parallel()
	r, _ := newTestRegistry()
	old := reconcileOneKey(t, r, "p", "a")
	old.Disable("revoked")

	r.Reconcile(Desired{Keys: []KeyRef{{Provider: "p", ID: "b"}}})
	if r.Key(KeyRef{Provider: "p", ID: "a"}) != nil {
		t.Fatal("removed key survived reconcile")
	}
	fresh := r.Key(KeyRef{Provider: "p", ID: "b"})
	if fresh == nil || fresh.State() != StateHealthy {
		t.Fatal("new key did not start healthy")
	}
}

// TestKey_CooldownExpires pins the cooldown lifecycle.
func TestKey_CooldownExpires(t *testing.T) {
	t.Parallel()
	r, clock := newTestRegistry()
	k := reconcileOneKey(t, r, "p", "a")

	k.CoolFor(2 * time.Second)
	if k.State() != StateCooldown || k.Available() {
		t.Fatalf("state = %s, available = %v; want cooldown", k.State(), k.Available())
	}
	clock.Advance(1999 * time.Millisecond)
	if k.Available() {
		t.Fatal("key became available 1ms early")
	}
	clock.Advance(2 * time.Millisecond)
	if !k.Available() {
		t.Fatal("key did not return after its cooldown expired")
	}
}

// TestKey_CoolForExtendsButNeverShortens pins that a second, shorter
// cooldown cannot shorten the first.
func TestKey_CoolForExtendsButNeverShortens(t *testing.T) {
	t.Parallel()
	r, clock := newTestRegistry()
	k := reconcileOneKey(t, r, "p", "a")

	k.CoolFor(10 * time.Second)
	k.CoolFor(time.Second)
	if got := k.CooldownUntil().Sub(clock.Now()); got != 10*time.Second {
		t.Fatalf("cooldown = %v, want 10s", got)
	}
}

// TestKey_DisabledOutlivesCooldownAndReload pins that disable is sticky
// and cooling never resurrects a disabled key.
func TestKey_DisabledOutlivesCooldownAndReload(t *testing.T) {
	t.Parallel()
	r, clock := newTestRegistry()
	k := reconcileOneKey(t, r, "p", "a")
	k.Disable("revoked")
	k.CoolFor(2 * time.Second)

	clock.Advance(time.Minute)
	again := reconcileOneKey(t, r, "p", "a")
	if again.State() != StateDisabled {
		t.Fatalf("state after cooldown + reload = %s, want disabled", again.State())
	}
	if got := again.DisabledReason(); got == "" {
		t.Fatal("disabled reason was lost")
	}
	again.Enable()
	if again.State() != StateHealthy {
		t.Fatalf("state after Enable = %s, want healthy", again.State())
	}
}

// TestKey_BackoffGrowsAndIsCapped pins the no-Retry-After backoff: it
// grows with consecutive rate limits, never retries immediately, and caps
// at 30 seconds (plus jitter).
func TestKey_BackoffGrowsAndIsCapped(t *testing.T) {
	t.Parallel()
	r, clock := newTestRegistry()
	k := reconcileOneKey(t, r, "p", "a")

	seen := make([]time.Duration, 0, 8)
	for i := 0; i < 8; i++ {
		before := clock.Now()
		k.NoteRateLimited(0)
		seen = append(seen, k.CooldownUntil().Sub(before))
		clock.Advance(k.CooldownUntil().Sub(clock.Now()) + time.Millisecond)
	}
	for i, d := range seen {
		if d <= 0 {
			t.Fatalf("backoff %d is not positive: %v", i, d)
		}
		if d > backoffMax*2 {
			t.Fatalf("backoff %d exceeds the cap: %v", i, d)
		}
	}
	if seen[0] >= seen[len(seen)-1] {
		t.Fatalf("backoff did not grow: first %v last %v", seen[0], seen[len(seen)-1])
	}
}

// TestKey_RetryAfterIsHonouredExactly pins that a server-provided
// Retry-After is used verbatim rather than being replaced by backoff.
func TestKey_RetryAfterIsHonouredExactly(t *testing.T) {
	t.Parallel()
	r, clock := newTestRegistry()
	k := reconcileOneKey(t, r, "p", "a")

	k.NoteRateLimited(7 * time.Second)
	if got := k.CooldownUntil().Sub(clock.Now()); got != 7*time.Second {
		t.Fatalf("cooldown = %v, want exactly 7s", got)
	}
}

// TestKey_InflightNeverGoesNegative pins over-release safety.
func TestKey_InflightNeverGoesNegative(t *testing.T) {
	t.Parallel()
	r, _ := newTestRegistry()
	k := reconcileOneKey(t, r, "p", "a")
	k.Release()
	k.Release()
	if got := k.Inflight(); got != 0 {
		t.Fatalf("inflight = %d, want 0", got)
	}
}

// TestKey_InflightReturnsToZeroUnderMixedFailures is the leak test: a
// thousand mixed successes, cooldowns and disables leave the counter at
// zero.
func TestKey_InflightReturnsToZeroUnderMixedFailures(t *testing.T) {
	t.Parallel()
	r, clock := newTestRegistry()
	k := reconcileOneKey(t, r, "p", "a")
	for i := 0; i < 1000; i++ {
		k.Acquire()
		switch i % 3 {
		case 0:
			k.NoteRateLimited(0)
		case 1:
			k.CoolFor(time.Second)
		case 2:
			k.ResetConsecutive()
		}
		k.Release()
		clock.Advance(time.Millisecond)
	}
	if got := k.Inflight(); got != 0 {
		t.Fatalf("inflight leaked: %d", got)
	}
}

// TestBreaker_OpensAfterConsecutiveFailuresAndProbesOnce pins the breaker
// state machine, including the single half-open probe under concurrency.
func TestBreaker_OpensAfterConsecutiveFailuresAndProbesOnce(t *testing.T) {
	t.Parallel()
	r, clock := newTestRegistry()
	r.Reconcile(Desired{Providers: []ProviderRef{{Name: "p", BreakerThreshold: 5, BreakerCooldown: 10 * time.Second}}})
	b := r.Breaker("p")

	for i := 0; i < 4; i++ {
		b.RecordFailure()
		if !b.Allow() {
			t.Fatalf("breaker opened after %d failures, threshold is 5", i+1)
		}
	}
	b.RecordFailure()
	if b.Allow() {
		t.Fatal("breaker did not open after the fifth consecutive failure")
	}
	if !b.Open() {
		t.Fatal("Open() = false while the breaker should be open")
	}

	// Cooldown elapses: exactly one probe is admitted.
	clock.Advance(10 * time.Second)
	if b.Open() {
		t.Fatal("breaker stayed open past its cooldown")
	}
	admitted := 0
	for i := 0; i < 100; i++ {
		if b.Allow() {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("half-open admitted %d probes, want exactly 1", admitted)
	}

	// The probe succeeds: the breaker closes and admits everything again.
	b.RecordSuccess()
	if !b.Allow() {
		t.Fatal("breaker did not close after a successful probe")
	}
	if !b.Allow() {
		t.Fatal("closed breaker admitted only one request")
	}
}

// TestBreaker_FailedProbeReopens pins that a failed probe re-opens the
// breaker for another cooldown, at every threshold: the probe is the
// evidence, not the counter.
func TestBreaker_FailedProbeReopens(t *testing.T) {
	t.Parallel()
	for _, threshold := range []int{1, 5} {
		t.Run(fmt.Sprintf("threshold=%d", threshold), func(t *testing.T) {
			t.Parallel()
			r, clock := newTestRegistry()
			r.Reconcile(Desired{Providers: []ProviderRef{{Name: "p", BreakerThreshold: threshold, BreakerCooldown: 5 * time.Second}}})
			b := r.Breaker("p")
			for i := 0; i < threshold; i++ {
				b.RecordFailure()
			}
			clock.Advance(5 * time.Second)
			if !b.Allow() {
				t.Fatal("probe not admitted after cooldown")
			}
			b.RecordFailure()
			if b.Allow() {
				t.Fatal("failed probe did not re-open the breaker")
			}
			if !b.Open() {
				t.Fatal("Open() = false after a failed probe")
			}
			clock.Advance(4999 * time.Millisecond)
			if b.Allow() {
				t.Fatal("breaker re-opened early")
			}
			clock.Advance(2 * time.Millisecond)
			if !b.Allow() {
				t.Fatal("breaker did not admit a probe after the second cooldown")
			}
		})
	}
}

// TestBreaker_StragglerSuccessDoesNotCloseOpenBreaker pins that a success
// from a request admitted before the breaker opened cannot close it while
// the cooldown is still running.
func TestBreaker_StragglerSuccessDoesNotCloseOpenBreaker(t *testing.T) {
	t.Parallel()
	r, clock := newTestRegistry()
	r.Reconcile(Desired{Providers: []ProviderRef{{Name: "p", BreakerThreshold: 1, BreakerCooldown: 10 * time.Second}}})
	b := r.Breaker("p")
	b.RecordFailure()
	if b.Allow() {
		t.Fatal("breaker is not open")
	}
	b.RecordSuccess() // straggler admitted before the open
	if !b.Open() {
		t.Fatal("a straggler success closed the breaker mid-cooldown")
	}
	clock.Advance(10 * time.Second)
	if !b.Allow() {
		t.Fatal("breaker did not admit the probe after cooldown")
	}
	b.RecordSuccess()
	if b.Open() {
		t.Fatal("successful probe did not close the breaker")
	}
}

// TestBreaker_DisabledAfterOpenReportsHealthy pins that a reload which
// disables the breaker clears the state the admin surface reports.
func TestBreaker_DisabledAfterOpenReportsHealthy(t *testing.T) {
	t.Parallel()
	r, _ := newTestRegistry()
	r.Reconcile(Desired{Providers: []ProviderRef{{Name: "p", BreakerThreshold: 1, BreakerCooldown: time.Minute}}})
	b := r.Breaker("p")
	b.RecordFailure()
	if !b.Open() {
		t.Fatal("breaker is not open")
	}
	r.Reconcile(Desired{Providers: []ProviderRef{{Name: "p", BreakerThreshold: 0, BreakerCooldown: time.Minute}}})
	if b.Open() {
		t.Fatal("Open() = true after the breaker was disabled")
	}
	if got := b.State(); got != StateHealthy {
		t.Fatalf("State() = %q after disable, want healthy", got)
	}
	if !b.Allow() {
		t.Fatal("disabled breaker refused traffic")
	}
}

// TestBreaker_DisabledThresholdAlwaysAllows pins threshold <= 0.
func TestBreaker_DisabledThresholdAlwaysAllows(t *testing.T) {
	t.Parallel()
	r, _ := newTestRegistry()
	r.Reconcile(Desired{Providers: []ProviderRef{{Name: "p"}}})
	b := r.Breaker("p")
	for i := 0; i < 100; i++ {
		b.RecordFailure()
	}
	if !b.Allow() {
		t.Fatal("a breaker with threshold 0 must never block")
	}
}

// TestAccount_TakeRequestRefillsOverAMinute pins the rpm bucket.
func TestAccount_TakeRequestRefillsOverAMinute(t *testing.T) {
	t.Parallel()
	r, clock := newTestRegistry()
	r.Reconcile(Desired{Accounts: []AccountRef{{Name: "a", RPM: 60}}})
	acc := r.Account("a")

	for i := 0; i < 60; i++ {
		if ok, _ := acc.TakeRequest(); !ok {
			t.Fatalf("request %d refused inside the minute budget", i+1)
		}
	}
	ok, wait := acc.TakeRequest()
	if ok {
		t.Fatal("61st request admitted with rpm = 60")
	}
	if wait <= 0 || wait > time.Minute {
		t.Fatalf("wait = %v, want a positive value inside a minute", wait)
	}
	clock.Advance(wait)
	if ok, _ := acc.TakeRequest(); !ok {
		t.Fatal("request refused after waiting the reported duration")
	}
}

// TestAccount_CoolingPropagatesToTheWholeAccount pins that one 429 cools
// every key in the account (the caller checks Cooling before selecting).
func TestAccount_CoolingPropagatesToTheWholeAccount(t *testing.T) {
	t.Parallel()
	r, clock := newTestRegistry()
	r.Reconcile(Desired{Accounts: []AccountRef{{Name: "a", RPM: 100}}})
	acc := r.Account("a")
	acc.CoolFor(3 * time.Second)

	if cooling, _ := acc.Cooling(); !cooling {
		t.Fatal("account is not cooling after CoolFor")
	}
	clock.Advance(3*time.Second + time.Millisecond)
	if cooling, _ := acc.Cooling(); cooling {
		t.Fatal("account still cooling after the deadline")
	}
}

// TestClient_TpmLimitIsPostHoc pins the client token bucket and its
// limited marker.
func TestClient_TpmLimitIsPostHoc(t *testing.T) {
	t.Parallel()
	r, clock := newTestRegistry()
	r.Reconcile(Desired{Clients: []ClientRef{{ID: "c", TPM: 100}}})
	c := r.Client("c")

	if ok, _ := c.TakeTokens(60); !ok {
		t.Fatal("first 60 tokens refused")
	}
	if ok, _ := c.TakeTokens(60); ok {
		t.Fatal("over-budget take succeeded")
	}
	ok, wait := c.TakeTokens(60)
	if ok || wait <= 0 {
		t.Fatal("expected refusal with a positive wait")
	}
	c.LimitFor(clock.Now().Add(time.Minute))
	if limited, _ := c.LimitedUntil(); !limited {
		t.Fatal("client not marked limited")
	}
	clock.Advance(time.Minute + time.Millisecond)
	if limited, _ := c.LimitedUntil(); limited {
		t.Fatal("client still limited after the deadline")
	}
}

// TestRegistry_AccessorsAndStatusSurface pins the accessors and the status
// snapshots /admin/status renders, including the "not configured" nil
// paths.
func TestRegistry_AccessorsAndStatusSurface(t *testing.T) {
	t.Parallel()
	r, clock := newTestRegistry()
	r.Reconcile(Desired{
		Keys:      []KeyRef{{Provider: "p", ID: "k1"}},
		Providers: []ProviderRef{{Name: "p", BreakerThreshold: 1, BreakerCooldown: time.Second}},
		Accounts:  []AccountRef{{Name: "acct", RPM: 10, TPM: 100}},
		Clients:   []ClientRef{{ID: "c1", RPM: 10, TPM: 100}},
	})

	k := r.Key(KeyRef{Provider: "p", ID: "k1"})
	if k.Provider() != "p" || k.ID() != "k1" {
		t.Fatalf("key identity = %s/%s", k.Provider(), k.ID())
	}
	if k.DisabledReason() != "" {
		t.Fatal("healthy key reports a disabled reason")
	}
	if !k.CooldownUntil().IsZero() {
		t.Fatal("healthy key reports a cooldown")
	}

	// Status snapshots.
	if st := k.Status(); st.State != StateHealthy || st.ID != "k1" {
		t.Fatalf("key status = %+v", st)
	}
	if st := r.Breaker("p").Status("p"); st.State != StateHealthy || st.Provider != "p" {
		t.Fatalf("breaker status = %+v", st)
	}
	if st := r.Account("acct").Status(); st.Name != "acct" || st.Cooling {
		t.Fatalf("account status = %+v", st)
	}
	if st := r.Client("c1").Status(); st.ID != "c1" || st.Limited {
		t.Fatalf("client status = %+v", st)
	}
	if r.Account("") != nil {
		t.Fatal("empty account name resolved to state")
	}
	if r.Account("nope") != nil || r.Client("nope") != nil || r.Breaker("nope") != nil {
		t.Fatal("unknown identity resolved to state")
	}

	// Breaker transition, then status reports it.
	b := r.Breaker("p")
	b.RecordFailure()
	if !b.Open() {
		t.Fatal("threshold-1 breaker did not open")
	}
	if st := b.Status("p"); st.State == StateHealthy {
		t.Fatalf("breaker status after open = %+v", st)
	}
	if b.ConsecutiveFailures() != 0 {
		t.Fatalf("consecutive = %d after opening, want 0", b.ConsecutiveFailures())
	}
	clock.Advance(2 * time.Second)
	if st := b.Status("p"); st.State != StateCooldown {
		t.Fatalf("half-open status = %+v", st)
	}

	// Account and client limits.
	acc := r.Account("acct")
	if ok, _ := acc.TakeRequest(); !ok {
		t.Fatal("account refused a request inside its budget")
	}
	if acc.Name() != "acct" {
		t.Fatalf("account name = %q", acc.Name())
	}
	if acc.TokensLeft() == 0 {
		t.Fatal("account reports no tokens inside budget")
	}
	if ok, _ := acc.TakeTokens(50); !ok {
		t.Fatal("account refused tokens inside budget")
	}
	acc.CoolFor(time.Second)
	if !acc.CooldownUntil().After(clock.Now()) {
		t.Fatal("account cooldown deadline not set")
	}

	c := r.Client("c1")
	if c.ID() != "c1" {
		t.Fatalf("client id = %q", c.ID())
	}
	if ok, _ := c.TakeRequest(); !ok {
		t.Fatal("client refused a request inside its budget")
	}
	if c.TokensLeft() == 0 {
		t.Fatal("client reports no tokens inside budget")
	}
	c.LimitFor(clock.Now().Add(time.Minute))
	if st := c.Status(); !st.Limited || st.LimitedUntil == "" {
		t.Fatalf("client status after limit = %+v", st)
	}
	if !r.Now().Equal(clock.Now()) {
		t.Fatal("Now() diverged from the injected clock")
	}

	// KeysFor preserves configuration order and skips unknown ids.
	got := r.KeysFor("p", []string{"k1", "missing"})
	if len(got) != 1 || got[0].ID() != "k1" {
		t.Fatalf("KeysFor = %+v", got)
	}
}

// TestRegistry_ConfigureRetainsBudgets pins that a reload with unchanged
// limits does not refill a bucket, and that changed limits apply.
func TestRegistry_ConfigureRetainsBudgets(t *testing.T) {
	t.Parallel()
	r, _ := newTestRegistry()
	desired := Desired{
		Accounts: []AccountRef{{Name: "a", RPM: 10}},
		Clients:  []ClientRef{{ID: "c", RPM: 10}},
	}
	r.Reconcile(desired)
	acc := r.Account("a")
	for i := 0; i < 10; i++ {
		if ok, _ := acc.TakeRequest(); !ok {
			t.Fatalf("request %d refused", i+1)
		}
	}
	if ok, _ := acc.TakeRequest(); ok {
		t.Fatal("budget exceeded before reconcile")
	}
	r.Reconcile(desired) // same limits: no refill
	if ok, _ := acc.TakeRequest(); ok {
		t.Fatal("reconcile with unchanged limits refilled the bucket")
	}
	desired.Accounts[0].RPM = 100
	desired.Clients[0].RPM = 100
	r.Reconcile(desired)
	if ok, _ := acc.TakeRequest(); !ok {
		t.Fatal("raised limit did not apply")
	}
}

// TestBreaker_ReleaseProbeAbandonsAHalfOpenProbe pins that a probe which
// never produced a verdict (slots exhausted) does not wedge the breaker.
func TestBreaker_ReleaseProbeAbandonsAHalfOpenProbe(t *testing.T) {
	t.Parallel()
	r, clock := newTestRegistry()
	r.Reconcile(Desired{Providers: []ProviderRef{{Name: "p", BreakerThreshold: 1, BreakerCooldown: time.Second}}})
	b := r.Breaker("p")
	b.RecordFailure()
	clock.Advance(2 * time.Second)

	if !b.Allow() {
		t.Fatal("first probe not admitted")
	}
	if b.Allow() {
		t.Fatal("second probe admitted while the first was in flight")
	}
	b.ReleaseProbe()
	if !b.Allow() {
		t.Fatal("released probe still blocks the breaker")
	}
}

// TestRouteOrder_PrefersTheFasterProviderAndIgnoresSmallGaps pins the
// fastest policy directly: unmeasured providers are explored first, then
// the leader changes only when the gap exceeds the band.
func TestRouteOrder_PrefersTheFasterProviderAndIgnoresSmallGaps(t *testing.T) {
	t.Parallel()
	r, _ := newTestRegistry()
	band := 15 * time.Millisecond
	candidates := []string{"slow", "fast"}

	// With no samples, every candidate is explored before any repeats.
	first := r.RouteOrder("m", candidates, band, 100, 0.2)
	second := r.RouteOrder("m", candidates, band, 100, 0.2)
	if first[0] == second[0] {
		t.Fatalf("unmeasured providers were not explored round-robin: %v then %v", first, second)
	}

	r.ObserveRouteLatency("m", "slow", 60*time.Millisecond)
	r.ObserveRouteLatency("m", "fast", 5*time.Millisecond)
	order := r.RouteOrder("m", candidates, band, 100, 0.2)
	if order[0] != "fast" {
		t.Fatalf("order = %v, want fast first", order)
	}

	// A gap inside the band must not move the leader.
	r2, _ := newTestRegistry()
	r2.ObserveRouteLatency("m", "a", 20*time.Millisecond)
	r2.ObserveRouteLatency("m", "b", 22*time.Millisecond)
	order = r2.RouteOrder("m", []string{"a", "b"}, band, 0, 0.2)
	leader := order[0]
	for i := 0; i < 20; i++ {
		if got := r2.RouteOrder("m", []string{"a", "b"}, band, 0, 0.2)[0]; got != leader {
			t.Fatalf("leader flapped inside the hysteresis band: %s then %s", leader, got)
		}
	}

	// Exploration rotates the non-leader to the front.
	order = r2.RouteOrder("m", []string{"a", "b"}, band, 1, 0.2)
	if order[0] == leader {
		t.Fatalf("explore_every = 1 did not explore: %v", order)
	}
}

// TestRouteEWMA_TracksSamples pins the estimator surface and its guards.
func TestRouteEWMA_TracksSamples(t *testing.T) {
	t.Parallel()
	r, _ := newTestRegistry()
	if _, ok := r.RouteEWMA("m", "p"); ok {
		t.Fatal("unknown series reported a sample")
	}
	r.ObserveRouteLatency("m", "p", 100*time.Millisecond)
	if d, ok := r.RouteEWMA("m", "p"); !ok || d != 100*time.Millisecond {
		t.Fatalf("first sample = %v/%v", d, ok)
	}
	r.SetRouteAlpha(0.5)
	r.ObserveRouteLatency("m", "p", 200*time.Millisecond)
	if d, _ := r.RouteEWMA("m", "p"); d != 150*time.Millisecond {
		t.Fatalf("ewma = %v, want 150ms", d)
	}
	// Degenerate samples are ignored.
	r.ObserveRouteLatency("", "p", time.Second)
	r.ObserveRouteLatency("m", "", time.Second)
	r.ObserveRouteLatency("m", "p", 0)
	if d, _ := r.RouteEWMA("m", "p"); d != 150*time.Millisecond {
		t.Fatalf("degenerate sample changed the estimate: %v", d)
	}
}

// TestRouteOrder_UsesTheConfiguredAlpha pins that ewma_alpha from the
// configuration actually changes the estimator: a higher alpha weights the
// newest sample more heavily.
func newTestRoutes(alpha float64) *routes {
	r := newRoutes()
	if alpha > 0 {
		r.setAlpha(alpha)
	}
	return r
}

func TestRouteOrder_UsesTheConfiguredAlpha(t *testing.T) {
	t.Parallel()
	slow := newTestRoutes(0.9) // very reactive
	slow.observe("m", "p", 100*time.Millisecond)
	slow.observe("m", "p", 200*time.Millisecond)
	if d, _ := slow.ewma("m", "p"); d != 190*time.Millisecond {
		t.Fatalf("alpha 0.9 ewma = %v, want 190ms", d)
	}

	calm := newTestRoutes(0.1)
	calm.observe("m", "p", 100*time.Millisecond)
	calm.observe("m", "p", 200*time.Millisecond)
	if d, _ := calm.ewma("m", "p"); d != 110*time.Millisecond {
		t.Fatalf("alpha 0.1 ewma = %v, want 110ms", d)
	}

	// The per-alias value supplied to RouteOrder overrides the default.
	r := newTestRoutes(0)
	r.observe("m", "p", 100*time.Millisecond)
	r.order("m", []string{"p", "q"}, time.Millisecond, 0, 0.5)
	r.observe("m", "p", 200*time.Millisecond)
	if d, _ := r.ewma("m", "p"); d != 150*time.Millisecond {
		t.Fatalf("per-alias alpha ewma = %v, want 150ms", d)
	}
}
