package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amscotti/jevlar/internal/config"
	"github.com/amscotti/jevlar/internal/health"
	"github.com/amscotti/jevlar/internal/metrics"
	"github.com/amscotti/jevlar/internal/runtime"
	"github.com/amscotti/jevlar/internal/testutil"
)

// routingHarness builds an alias with the given targets and policy.
type routingHarness struct {
	h        *Handler
	rt       *runtime.Runtime
	reg      *health.Registry
	upstream map[string]*testutil.FakeUpstream
}

type targetSpec struct {
	provider string
	model    string
	delay    time.Duration
	status   int
}

func newRoutingHarness(t *testing.T, specs []targetSpec, policy string, maxAttempts int) *routingHarness {
	t.Helper()
	h := &routingHarness{upstream: make(map[string]*testutil.FakeUpstream, len(specs))}
	cfg := &config.Config{
		ConfigVersion: 1,
		Server: config.Server{
			Listen: "127.0.0.1:0", RequestTimeoutMs: 10_000, MaxBodyBytes: 4096,
			MaxResponseBytes: 4096, MaxInflight: 64, ShutdownGraceMs: 1000,
			ReadHeaderTimeoutMs: 5000, LogLevel: "error",
		},
		Auth:    config.Auth{Keys: []config.AuthKey{{ID: "test", Hash: testClientHash()}}},
		Breaker: config.Breaker{Threshold: 5, CooldownMs: 30_000},
		Models: map[string]config.Model{
			"jev-latest": {
				Policy:       policy,
				MaxAttempts:  maxAttempts,
				EwmaAlpha:    0.2,
				HysteresisMs: 15,
				ExploreEvery: 100,
			},
		},
	}
	for _, spec := range specs {
		up := testutil.NewFakeUpstream(t)
		if spec.delay > 0 {
			up.SetDelay(spec.delay)
		}
		if spec.status != 0 {
			up.SetStatus(spec.status)
		}
		h.upstream[spec.provider] = up
		cfg.Providers = append(cfg.Providers, config.Provider{
			Name: spec.provider, BaseURL: up.URL(), Path: "/v1/systemone",
			Auth: "none", Models: []string{spec.model},
			TimeoutMs: 5000, MaxConcurrency: 8,
		})
		cfg.Models["jev-latest"] = config.Model{
			Targets: append(cfg.Models["jev-latest"].Targets,
				config.ModelTarget{Provider: spec.provider, Model: spec.model}),
			Policy:       policy,
			MaxAttempts:  maxAttempts,
			EwmaAlpha:    0.2,
			HysteresisMs: 15,
			ExploreEvery: 100,
		}
	}
	rt, err := runtime.Build(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	h.reg = health.New()
	t.Cleanup(testutil.WireWithHealth(rt, h.reg))
	h.rt = rt
	h.h = New(func() *runtime.Runtime { return rt },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		metrics.New(metrics.Options{Version: "t", GoVersion: "t"}))
	return h
}

func (x *routingHarness) do(t *testing.T, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testClientToken)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	x.h.ServeSystemOne(rec, req)
	return rec
}

// TestRewrite_EndToEnd_PreservesEveryOtherByte pins the plan's gate: a
// request for the alias sent to a target that names a different model
// carries the target's model, and replacing that value restores the
// original body hash.
func TestRewrite_EndToEnd_PreservesEveryOtherByte(t *testing.T) {
	t.Parallel()
	x := newRoutingHarness(t, []targetSpec{
		{provider: "a", model: "jev-latest", status: http.StatusInternalServerError},
		{provider: "b", model: "nimble"},
	}, "priority", 2)

	body := validBody
	rec := x.do(t, body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	got, _ := x.upstream["b"].LastRequest()
	if !strings.Contains(string(got), `"model":"nimble"`) {
		t.Fatalf("target b received %s, want model nimble", got)
	}
	restored, err := restoreModel(got, "jev-latest")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if string(restored) != body {
		t.Fatalf("restored body differs from the original\n got: %s\nwant: %s", restored, body)
	}
	// The first target must have received the unmodified alias.
	first, _ := x.upstream["a"].LastRequest()
	if string(first) != body {
		t.Fatalf("first target received a rewritten body: %s", first)
	}
}

// TestXProvider_PinsToATarget pins that X-Provider selects one target.
func TestXProvider_PinsToATarget(t *testing.T) {
	t.Parallel()
	x := newRoutingHarness(t, []targetSpec{
		{provider: "a", model: "jev-latest"},
		{provider: "b", model: "nimble"},
	}, "priority", 2)

	rec := x.do(t, validBody, map[string]string{"X-Provider": "b"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if x.upstream["a"].Requests() != 0 {
		t.Fatal("pinned request reached the unpinned provider")
	}
	if x.upstream["b"].Requests() != 1 {
		t.Fatalf("pinned provider got %d requests", x.upstream["b"].Requests())
	}
	got, _ := x.upstream["b"].LastRequest()
	if !strings.Contains(string(got), `"model":"nimble"`) {
		t.Fatalf("pinned target model was not applied: %s", got)
	}
}

// TestXProvider_UnknownProviderIs400 pins validation: a pin must name a
// target of this alias, so it cannot bypass routing.
func TestXProvider_UnknownProviderIs400(t *testing.T) {
	t.Parallel()
	x := newRoutingHarness(t, []targetSpec{{provider: "a", model: "jev-latest"}}, "priority", 2)

	rec := x.do(t, validBody, map[string]string{"X-Provider": "nope"})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), TypeInvalidRequest) {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if x.upstream["a"].Requests() != 0 {
		t.Fatal("invalid pin still contacted an upstream")
	}
}

// TestXProvider_UnhealthyProviderIs503 pins that pinning to a breaker-open
// provider fails with capacity rather than silently using another target.
func TestXProvider_UnhealthyProviderIs503(t *testing.T) {
	t.Parallel()
	x := newRoutingHarness(t, []targetSpec{
		{provider: "a", model: "jev-latest"},
		{provider: "b", model: "nimble"},
	}, "priority", 2)
	breaker := x.reg.Breaker("b")
	for i := 0; i < 5; i++ {
		breaker.RecordFailure()
	}

	rec := x.do(t, validBody, map[string]string{"X-Provider": "b"})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
	if x.upstream["a"].Requests() != 0 {
		t.Fatal("a pinned request fell over to another provider")
	}
}

// TestFastest_SendsMostTrafficToTheFasterTarget pins the EWMA policy: with
// a clear latency gap, the fast provider takes the overwhelming majority.
func TestFastest_SendsMostTrafficToTheFasterTarget(t *testing.T) {
	t.Parallel()
	x := newRoutingHarness(t, []targetSpec{
		{provider: "slow", model: "jev-latest", delay: 60 * time.Millisecond},
		{provider: "fast", model: "nimble", delay: 5 * time.Millisecond},
	}, "fastest", 1)

	for i := 0; i < 100; i++ {
		if rec := x.do(t, validBody, nil); rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d", i, rec.Code)
		}
	}
	fast := x.upstream["fast"].Requests()
	if fast < 90 {
		t.Fatalf("fast provider served %d of 100, want at least 90", fast)
	}
}

// TestFastest_DoesNotFlapInsideTheHysteresisBand pins the band: providers
// within 15 ms of each other keep a stable leader.
func TestFastest_DoesNotFlapInsideTheHysteresisBand(t *testing.T) {
	t.Parallel()
	x := newRoutingHarness(t, []targetSpec{
		{provider: "a", model: "jev-latest", delay: 20 * time.Millisecond},
		{provider: "b", model: "nimble", delay: 22 * time.Millisecond},
	}, "fastest", 1)

	// Warm up so both have estimates inside the band.
	for i := 0; i < 10; i++ {
		x.do(t, validBody, nil)
	}
	aBefore, bBefore := x.upstream["a"].Requests(), x.upstream["b"].Requests()

	for i := 0; i < 80; i++ {
		x.do(t, validBody, nil)
	}
	aAfter, bAfter := x.upstream["a"].Requests(), x.upstream["b"].Requests()

	// Exploration aside, the leader should keep serving: one provider
	// should receive the large majority rather than the split flapping.
	majority := max(aAfter-aBefore, bAfter-bBefore)
	if majority < 70 {
		t.Fatalf("traffic split inside the hysteresis band: a=%d b=%d", aAfter-aBefore, bAfter-bBefore)
	}
}

// TestBudget_ThreeTargetChainNeverExceedsTheRequestDeadline pins the
// deadline arithmetic: three slow targets inside a 300 ms budget cannot
// hold the client for longer than the budget (plus a small margin).
func TestBudget_ThreeTargetChainNeverExceedsTheRequestDeadline(t *testing.T) {
	t.Parallel()
	h := newRoutingHarness(t, []targetSpec{
		{provider: "a", model: "jev-latest", delay: 2 * time.Second},
		{provider: "b", model: "nimble", delay: 2 * time.Second},
		{provider: "c", model: "tev1", delay: 2 * time.Second},
	}, "priority", 3)
	h.rt.Config.Server.RequestTimeoutMs = 300

	started := time.Now()
	rec := h.do(t, validBody, nil)
	elapsed := time.Since(started)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (body %s)", rec.Code, rec.Body.String())
	}
	if elapsed > 600*time.Millisecond {
		t.Fatalf("request took %v for a 300ms budget", elapsed)
	}
}

// TestFastest_ExplorationSendsSomeTrafficToTheNonLeader pins deterministic
// exploration: with explore_every = 4, the non-leader is tried regularly.
func TestFastest_ExplorationSendsSomeTrafficToTheNonLeader(t *testing.T) {
	t.Parallel()
	h := newRoutingHarness(t, []targetSpec{
		{provider: "slow", model: "jev-latest", delay: 40 * time.Millisecond},
		{provider: "fast", model: "nimble", delay: 2 * time.Millisecond},
	}, "fastest", 1)

	for i := 0; i < 40; i++ {
		h.do(t, validBody, nil)
	}
	if h.upstream["slow"].Requests() == 0 {
		t.Fatal("exploration never reached the slower provider")
	}
}

// TestFastest_ErrorsDoNotAttractTraffic pins that a provider answering
// quickly with failures cannot win the latency ranking: only responses
// that prove the provider works are sampled.
func TestFastest_ErrorsDoNotAttractTraffic(t *testing.T) {
	t.Parallel()
	if rankedResponse(http.StatusInternalServerError) {
		t.Fatal("a 500 was counted as a ranking sample")
	}
	if rankedResponse(http.StatusTooManyRequests) {
		t.Fatal("a 429 was counted as a ranking sample")
	}
	if rankedResponse(http.StatusUnauthorized) {
		t.Fatal("a 401 was counted as a ranking sample")
	}
	for _, s := range []int{
		http.StatusOK, http.StatusCreated, http.StatusBadRequest,
		http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity,
	} {
		if !rankedResponse(s) {
			t.Fatalf("status %d should count as a ranking sample", s)
		}
	}
}

// TestBudget_RevisitsTargetsWhenBudgetRemains pins that max_attempts is a
// real budget: with one target and three attempts, a transient failure is
// retried rather than failing the request.
func TestBudget_RevisitsTargetsWhenBudgetRemains(t *testing.T) {
	t.Parallel()
	x := newRoutingHarness(t, []targetSpec{{provider: "a", model: "jev-latest"}}, "priority", 3)
	var n int32
	x.upstream["a"].SetHandler(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&n, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	rec := x.do(t, validBody, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the third attempt (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Jevlar-Attempts"); got != "3" {
		t.Fatalf("X-Jevlar-Attempts = %q, want 3", got)
	}
}

// TestBudget_AllUnusableTerminates pins termination: when every target is
// skipped (breakers open) the request ends promptly instead of spinning.
func TestBudget_AllUnusableTerminates(t *testing.T) {
	t.Parallel()
	x := newRoutingHarness(t, []targetSpec{
		{provider: "a", model: "jev-latest"},
		{provider: "b", model: "nimble"},
	}, "priority", 8)
	for _, p := range []string{"a", "b"} {
		b := x.reg.Breaker(p)
		for i := 0; i < 5; i++ {
			b.RecordFailure()
		}
	}

	start := time.Now()
	rec := x.do(t, validBody, nil)
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("request took %v with every breaker open", elapsed)
	}
}
