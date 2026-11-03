package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amscotti/jevlar/internal/config"
	"github.com/amscotti/jevlar/internal/health"
	"github.com/amscotti/jevlar/internal/metrics"
	"github.com/amscotti/jevlar/internal/runtime"
	"github.com/amscotti/jevlar/internal/testutil"
)

// authRecorder records the Authorization header of every upstream request
// so a test can see which key served which attempt.
type authRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (a *authRecorder) add(v string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, v)
}

func (a *authRecorder) all() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

// keyedHarness is a one-provider, two-key setup whose upstream records the
// key used and can be scripted per request.
type keyedHarness struct {
	h        *Handler
	rt       *runtime.Runtime
	cfg      *config.Config
	reg      *health.Registry
	upstream *testutil.FakeUpstream
	auth     *authRecorder
}

func newKeyedHarness(t *testing.T, script func(w http.ResponseWriter, r *http.Request)) *keyedHarness {
	return newKeyedHarnessAccount(t, script, true)
}

// newKeyedHarnessAccount builds the harness with or without an account
// grouping. Without one, a 429 is per-key and rotation applies; with one,
// a 429 cools the whole account by design.
func newKeyedHarnessAccount(t *testing.T, script func(w http.ResponseWriter, r *http.Request), withAccount bool) *keyedHarness {
	t.Helper()
	up := testutil.NewFakeUpstream(t)
	rec := &authRecorder{}
	up.SetHandler(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.Header.Get("Authorization"))
		if script != nil {
			script(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	cfg := &config.Config{
		ConfigVersion: 1,
		Server: config.Server{
			Listen: "127.0.0.1:0", RequestTimeoutMs: 10_000, MaxBodyBytes: 4096,
			MaxResponseBytes: 4096, MaxInflight: 64, ShutdownGraceMs: 1000,
			ReadHeaderTimeoutMs: 5000, LogLevel: "error",
		},
		Auth: config.Auth{Keys: []config.AuthKey{{ID: "test", Hash: testClientHash()}}},
		Accounts: map[string]config.Account{
			"acct": {RPM: 10_000, TPM: 10_000_000},
		},
		Providers: []config.Provider{{
			Name: "p", BaseURL: up.URL(), Path: "/v1/systemone", Auth: "bearer",
			Account: accountName(withAccount), Models: []string{"jev-latest"},
			TimeoutMs: 5000, MaxConcurrency: 8,
			APIKeys: []config.APIKey{
				{ID: "a", Resolved: "secret-a"},
				{ID: "b", Resolved: "secret-b"},
			},
		}},
		Models: map[string]config.Model{
			"jev-latest": {
				Targets:     []config.ModelTarget{{Provider: "p", Model: "jev-latest"}},
				Policy:      "priority",
				MaxAttempts: 3,
			},
		},
		Breaker: config.Breaker{Threshold: 5, CooldownMs: 30_000},
	}
	rt, err := runtime.Build(cfg)
	if err != nil {
		t.Fatalf("build runtime: %v", err)
	}
	reg := health.New()
	t.Cleanup(testutil.WireWithHealth(rt, reg))
	// Deterministic selection: always draw the first candidate, so the test
	// controls which key is tried first.
	rt.Providers["p"].Pool.SetRand(func(int) int { return 0 })
	return &keyedHarness{h: newHandler(rt), rt: rt, cfg: cfg, reg: reg, upstream: up, auth: rec}
}

// accountName returns the harness account name, or "" for an ungrouped
// provider.
func accountName(withAccount bool) string {
	if withAccount {
		return "acct"
	}
	return ""
}

func newHandler(rt *runtime.Runtime) *Handler {
	m := metrics.New(metrics.Options{Version: "test", GoVersion: "go-test"})
	return New(func() *runtime.Runtime { return rt },
		slog.New(slog.NewTextHandler(io.Discard, nil)), m)
}

func (x *keyedHarness) do(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(validBody))
	req.Header.Set("Authorization", "Bearer "+testClientToken)
	rec := httptest.NewRecorder()
	x.h.ServeSystemOne(rec, req)
	return rec
}

// TestKeyRotation_429CoolsOnlyTheOffendingKey pins the headline the routed stage
// behaviour: after a 429 on key A, the next request uses key B.
func TestKeyRotation_429CoolsOnlyTheOffendingKey(t *testing.T) {
	t.Parallel()
	// No account grouping: limits are per key, so a 429 on one key rotates
	// to the next. (With an account, the account cools instead — see
	// TestAccountCooldown_PropagatesToOtherKeys.)
	x := newKeyedHarnessAccount(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "secret-a") {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"slow down"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}, false)

	first := x.do(t)
	if first.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200 via the other key (body %s)", first.Code, first.Body.String())
	}
	used := x.auth.all()
	if len(used) != 2 {
		t.Fatalf("upstream saw %d attempts, want 2 (429 then retry): %v", len(used), used)
	}
	if !strings.Contains(used[0], "secret-a") || !strings.Contains(used[1], "secret-b") {
		t.Fatalf("key rotation order = %v, want a then b", used)
	}
	if got := x.reg.Key(health.KeyRef{Provider: "p", ID: "a"}).State(); got != health.StateCooldown {
		t.Fatalf("key a state = %s, want cooldown", got)
	}
}

// TestKeyRotation_CooledKeyIsNotSelectedBeforeItsDeadline pins that the
// key stays out of rotation for the Retry-After window and returns after.
func TestKeyRotation_CooledKeyIsNotSelectedBeforeItsDeadline(t *testing.T) {
	t.Parallel()
	x := newKeyedHarnessAccount(t, nil, false)
	keyA := x.reg.Key(health.KeyRef{Provider: "p", ID: "a"})
	keyA.CoolFor(2 * time.Second)

	for i := 0; i < 5; i++ {
		x.upstream.SetHandler(func(w http.ResponseWriter, r *http.Request) {
			x.auth.add(r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(`{"ok":true}`))
		})
		if rec := x.do(t); rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d", i, rec.Code)
		}
	}
	for _, used := range x.auth.all() {
		if strings.Contains(used, "secret-a") {
			t.Fatalf("cooled key was selected: %v", used)
		}
	}
}

// TestAccountCooldown_PropagatesToOtherKeys pins that a 429 on key A also
// cools the account, so key B is not used to sidestep the account limit.
func TestAccountCooldown_PropagatesToOtherKeys(t *testing.T) {
	t.Parallel()
	up := testutil.NewFakeUpstream(t)
	up.SetHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	})

	cfg := &config.Config{
		ConfigVersion: 1,
		Server: config.Server{
			Listen: "127.0.0.1:0", RequestTimeoutMs: 5000, MaxBodyBytes: 4096,
			MaxResponseBytes: 4096, MaxInflight: 32, ShutdownGraceMs: 100,
			ReadHeaderTimeoutMs: 1000, LogLevel: "error",
		},
		Auth:     config.Auth{Keys: []config.AuthKey{{ID: "test", Hash: testClientHash()}}},
		Accounts: map[string]config.Account{"acct": {RPM: 10_000}},
		Providers: []config.Provider{{
			Name: "p", BaseURL: up.URL(), Path: "/v1/systemone", Auth: "bearer",
			Account: "acct", Models: []string{"jev-latest"}, TimeoutMs: 2000, MaxConcurrency: 4,
			APIKeys: []config.APIKey{{ID: "a", Resolved: "secret-a"}, {ID: "b", Resolved: "secret-b"}},
		}},
		Models: map[string]config.Model{
			"jev-latest": {
				Targets: []config.ModelTarget{{Provider: "p", Model: "jev-latest"}},
				Policy:  "priority", MaxAttempts: 2,
			},
		},
	}
	rt, err := runtime.Build(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	reg := health.New()
	t.Cleanup(testutil.WireWithHealth(rt, reg))
	rt.Providers["p"].Pool.SetRand(func(int) int { return 0 })
	h := newHandler(rt)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(validBody))
	req.Header.Set("Authorization", "Bearer "+testClientToken)
	h.ServeSystemOne(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body %s)", rec.Code, rec.Body.String())
	}
	if got := up.Requests(); got != 1 {
		t.Fatalf("upstream attempts = %d, want 1: the account cooldown must stop the retry on key b", got)
	}
	if cooling, _ := reg.Account("acct").Cooling(); !cooling {
		t.Fatal("account was not cooled by a 429")
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("no Retry-After on the capacity response")
	}
}

// TestAllKeysCoolingReturns429WithEarliestExpiry pins the exhaustion path.
func TestAllKeysCoolingReturns429WithEarliestExpiry(t *testing.T) {
	t.Parallel()
	x := newKeyedHarnessAccount(t, nil, false)
	// Cool the account too, so the loop reaches key selection.
	for _, id := range []string{"a", "b"} {
		x.reg.Key(health.KeyRef{Provider: "p", ID: id}).CoolFor(time.Minute)
	}

	rec := x.do(t)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), TypeCapacityExhausted) {
		t.Fatalf("body = %s, want capacity_exhausted", rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("no Retry-After when every key is cooling")
	}
	if x.upstream.Requests() != 0 {
		t.Fatal("upstream contacted although every key was cooling")
	}
}

// TestUnauthorizedDisablesKeyAcrossReloads pins the reload guarantee end to
// end: a 401 disables the key, a new snapshot built from the same registry
// still refuses to use it.
func TestUnauthorizedDisablesKeyAcrossReloads(t *testing.T) {
	t.Parallel()
	x := newKeyedHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "secret-a") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	if rec := x.do(t); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 via key b (body %s)", rec.Code, rec.Body.String())
	}
	keyA := x.reg.Key(health.KeyRef{Provider: "p", ID: "a"})
	if keyA.State() != health.StateDisabled {
		t.Fatalf("key a state = %s, want disabled after a 401", keyA.State())
	}

	// Simulate a reload: a brand-new snapshot built from the same config,
	// wired against the same registry.
	reloaded, err := runtime.Build(x.cfg)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	t.Cleanup(testutil.WireWithHealth(reloaded, x.reg))
	h2 := newHandler(reloaded)

	x.auth.mu.Lock()
	x.auth.seen = nil
	x.auth.mu.Unlock()
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(validBody))
		req.Header.Set("Authorization", "Bearer "+testClientToken)
		rec := httptest.NewRecorder()
		h2.ServeSystemOne(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("post-reload request %d status = %d", i, rec.Code)
		}
	}
	for _, used := range x.auth.all() {
		if strings.Contains(used, "secret-a") {
			t.Fatalf("reload resurrected a 401-disabled key: %v", used)
		}
	}
	if got := x.reg.Key(health.KeyRef{Provider: "p", ID: "a"}).State(); got != health.StateDisabled {
		t.Fatalf("key a state after reload = %s, want disabled", got)
	}
}

// TestForbiddenCoolsButDoesNotDisable pins the 403 policy: cool, count,
// keep the key for later.
func TestForbiddenCoolsButDoesNotDisable(t *testing.T) {
	t.Parallel()
	x := newKeyedHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	_ = x.do(t)

	key := x.reg.Key(health.KeyRef{Provider: "p", ID: "a"})
	if key.State() == health.StateDisabled {
		t.Fatal("a 403 disabled the key; 403 also means quota, so it must only cool")
	}
	if key.State() != health.StateCooldown {
		t.Fatalf("key state = %s, want cooldown after a 403", key.State())
	}
}

// TestBreaker_OpensAfterFiveFailuresAndSkipsTheProvider pins the breaker
// through the proxy: after the threshold the provider is skipped without
// an upstream attempt.
func TestBreaker_OpensAfterFiveFailuresAndSkipsTheProvider(t *testing.T) {
	t.Parallel()
	x := newKeyedHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	for i := 0; i < 5; i++ {
		rec := x.do(t)
		if rec.Code < 500 {
			t.Fatalf("failure %d status = %d, want 5xx", i+1, rec.Code)
		}
	}
	breaker := x.reg.Breaker("p")
	if !breaker.Open() {
		t.Fatal("breaker did not open after five consecutive 500s")
	}

	before := x.upstream.Requests()
	rec := x.do(t)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 while the breaker is open (body %s)", rec.Code, rec.Body.String())
	}
	if x.upstream.Requests() != before {
		t.Fatalf("open breaker still contacted the upstream (%d -> %d)", before, x.upstream.Requests())
	}
	if !strings.Contains(rec.Body.String(), TypeCapacityExhausted) {
		t.Fatalf("body = %s, want capacity_exhausted", rec.Body.String())
	}
}
