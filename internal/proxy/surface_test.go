package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/amscotti/jevlar/internal/config"
	"github.com/amscotti/jevlar/internal/metrics"
	"github.com/amscotti/jevlar/internal/runtime"
	"github.com/amscotti/jevlar/internal/testutil"
)

// TestModels_UnionIsSortedAndDeduped pins GET /v1/models: aliases and
// provider model ids are unioned, deduplicated and sorted, and the same
// list is served in both the System One shape (which the official SDK
// parses) and the OpenAI shape (which other clients expect).
func TestModels_UnionIsSortedAndDeduped(t *testing.T) {
	t.Parallel()
	up := testutil.NewFakeUpstream(t)
	cfg := &config.Config{
		ConfigVersion: 1,
		Server: config.Server{
			Listen: "127.0.0.1:0", RequestTimeoutMs: 1000, MaxBodyBytes: 1024,
			MaxResponseBytes: 1024, MaxInflight: 4, ShutdownGraceMs: 100,
			ReadHeaderTimeoutMs: 100, LogLevel: "error",
		},
		Auth: config.Auth{Keys: []config.AuthKey{{ID: "test", Hash: testClientHash()}}},
		Providers: []config.Provider{{
			Name: "up", BaseURL: up.URL(), Path: "/v1/systemone", Auth: "none",
			Models: []string{"zeta", "alpha", "jev-latest"}, TimeoutMs: 1000, MaxConcurrency: 2,
		}},
		Models: map[string]config.Model{
			"jev-latest": {Targets: []config.ModelTarget{{Provider: "up", Model: "jev-latest"}}},
			"beta":       {Targets: []config.ModelTarget{{Provider: "up", Model: "alpha"}}},
		},
	}
	rt, err := runtime.Build(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	t.Cleanup(testutil.Wire(rt))
	h := New(func() *runtime.Runtime { return rt }, slog.New(slog.NewTextHandler(io.Discard, nil)),
		metrics.New(metrics.Options{Version: "t", GoVersion: "t"}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+testClientToken)
	h.Models(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	var got struct {
		Models []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			ReleaseDate string `json:"release_date"`
		} `json:"models"`
		Object string `json:"object"`
		Data   []struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(got.Models) != 4 || len(got.Data) != 4 {
		t.Fatalf("models=%d data=%d, want 4 each:\n%s", len(got.Models), len(got.Data), rec.Body.String())
	}
	wantOrder := []string{"alpha", "beta", "jev-latest", "zeta"}
	for i, want := range wantOrder {
		if got.Models[i].Name != want {
			t.Fatalf("models[%d] = %q, want %q (sorted, deduplicated)", i, got.Models[i].Name, want)
		}
		if got.Data[i].ID != want {
			t.Fatalf("data[%d].id = %q, want %q", i, got.Data[i].ID, want)
		}
		if got.Models[i].Description == "" {
			t.Fatalf("models[%d] has no description", i)
		}
	}
	// The alias entries are deduplicated against the provider's identical
	// model id: "jev-latest" appears exactly once.
	if got.Object != "list" || got.Data[0].Object != "model" {
		t.Fatalf("OpenAI shape fields missing: %+v", got)
	}
}

// TestModels_NilRuntimeIs503 pins the handler's fail-closed behaviour when
// no snapshot is published.
func TestModels_NilRuntimeIs503(t *testing.T) {
	t.Parallel()
	h := New(func() *runtime.Runtime { return nil }, nil,
		metrics.New(metrics.Options{Version: "t", GoVersion: "t"}))
	rec := httptest.NewRecorder()
	h.Models(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestServeSystemOne_UnknownModelFallsBackToAllProviders pins the
// documented fallback: an unconfigured model is tried on every provider
// with the requested model unchanged.
func TestServeSystemOne_UnknownModelFallsBackToAllProviders(t *testing.T) {
	t.Parallel()
	x := newHarness(t, nil)
	body := `{"model":"something-else","state":"x"}`
	rec := x.do(t, body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	got, _ := x.upstream.LastRequest()
	if string(got) != body {
		t.Fatalf("upstream body = %q, want the client body unchanged", got)
	}
	if rec.Header().Get("X-Jevlar-Model") != "unknown" {
		t.Fatalf("metric model label = %q, want unknown for cardinality safety",
			rec.Header().Get("X-Jevlar-Model"))
	}
}

// TestServeSystemOne_ProviderModelListBoundsTheMetricLabel pins that a
// configured provider model id is used as the label rather than "unknown".
func TestServeSystemOne_ProviderModelListBoundsTheMetricLabel(t *testing.T) {
	t.Parallel()
	x := newHarness(t, nil)
	rec := x.do(t, `{"model":"jev-latest","state":"x"}`, nil)
	if rec.Header().Get("X-Jevlar-Model") != "jev-latest" {
		t.Fatalf("label = %q", rec.Header().Get("X-Jevlar-Model"))
	}
}

// TestServeSystemOne_NoUsableProviderIs503 pins the exhaustion path when
// every provider needs a key it does not have.
func TestServeSystemOne_NoUsableProviderIs503(t *testing.T) {
	t.Parallel()
	x := newHarness(t, func(c *config.Config) {
		c.Providers[0].Auth = "bearer"
		c.Providers[0].APIKeys = nil
	})
	rec := x.do(t, validBody, nil)
	if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 503/404 when no provider is usable (body %s)", rec.Code, rec.Body.String())
	}
	if x.upstream.Requests() != 0 {
		t.Fatalf("upstream was contacted without a key")
	}
}

// TestServeSystemOne_InboundLimitIs503WithRetryAfter pins the global
// in-flight gate.
func TestServeSystemOne_InboundLimitIs503WithRetryAfter(t *testing.T) {
	t.Parallel()
	x := newHarness(t, nil)
	x.h.inflight.Store(1_000_000) // simulate saturation
	rec := x.do(t, validBody, nil)
	x.h.inflight.Store(0)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After on the saturation response")
	}
	if x.upstream.Requests() != 0 {
		t.Fatal("saturated gateway contacted the upstream")
	}
}

// TestParseRetryAfter_SecondsAndHTTPDate pins both Retry-After formats.
func TestParseRetryAfter_SecondsAndHTTPDate(t *testing.T) {
	t.Parallel()
	if got := parseRetryAfter("2"); got != 2*time.Second {
		t.Fatalf("seconds = %v, want 2s", got)
	}
	if got := parseRetryAfter("0"); got != 0 {
		t.Fatalf("zero = %v, want 0", got)
	}
	if got := parseRetryAfter("-5"); got != 0 {
		t.Fatalf("negative = %v, want 0", got)
	}
	if got := parseRetryAfter(""); got != 0 {
		t.Fatalf("empty = %v, want 0", got)
	}
	if got := parseRetryAfter("not-a-date"); got != 0 {
		t.Fatalf("garbage = %v, want 0", got)
	}
	future := time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(future); got < 1500*time.Millisecond || got > 3*time.Second {
		t.Fatalf("http-date = %v, want roughly 2-3s", got)
	}
	past := time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(past); got != 0 {
		t.Fatalf("past date = %v, want 0", got)
	}
}

// TestWriteError_RetryAfterIsRoundedUp pins the header format.
func TestWriteError_RetryAfterIsRoundedUp(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusTooManyRequests, TypeCapacityExhausted, "busy", "id-1", 1200)
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, want 2", got)
	}
	if !strings.Contains(rec.Body.String(), `"request_id":"id-1"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// TestServeSystemOne_AllRateLimitedIs429CapacityExhausted pins that a
// capacity problem is reported as such, with the upstream's Retry-After.
func TestServeSystemOne_AllRateLimitedIs429CapacityExhausted(t *testing.T) {
	t.Parallel()
	h, a, b := twoProviderHarness(t, 2)
	a.SetStatus(http.StatusTooManyRequests)
	a.SetHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"slow down"}`))
	})
	b.SetStatus(http.StatusTooManyRequests)

	rec := doAuthed(h, validBody)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Fatalf("Retry-After = %q, want 3 from the upstream", got)
	}
	if !strings.Contains(rec.Body.String(), TypeCapacityExhausted) {
		t.Fatalf("body = %s, want capacity_exhausted", rec.Body.String())
	}
}

// TestServeSystemOne_UnauthorizedUpstreamFailsOver pins that a rejected
// provider key is a failover reason, not a client error.
func TestServeSystemOne_UnauthorizedUpstreamFailsOver(t *testing.T) {
	t.Parallel()
	h, a, b := twoProviderHarness(t, 2)
	a.SetStatus(http.StatusUnauthorized)
	b.SetBody([]byte(`{"ok":true}`))

	rec := doAuthed(h, validBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover (body %s)", rec.Code, rec.Body.String())
	}
	if b.Requests() != 1 {
		t.Fatal("second provider was not tried after a 401")
	}
}

// TestServeSystemOne_NilRuntimeIs503 pins fail-closed behaviour before a
// snapshot exists.
func TestServeSystemOne_NilRuntimeIs503(t *testing.T) {
	t.Parallel()
	h := New(func() *runtime.Runtime { return nil }, nil,
		metrics.New(metrics.Options{Version: "t", GoVersion: "t"}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(validBody))
	req.Header.Set("Authorization", "Bearer "+testClientToken)
	h.ServeSystemOne(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestServeSystemOne_AllProviderSlotsBusyIs503Capacity pins the saturation
// mapping: when every provider is at its concurrency bound the client gets a
// retryable 503, not a 502 that looks like an upstream fault.
func TestServeSystemOne_AllProviderSlotsBusyIs503Capacity(t *testing.T) {
	t.Parallel()
	x := newHarness(t, nil)
	// Exhaust the provider's slot pool (max_concurrency = 8 in the harness).
	for i := 0; i < x.rt.Providers["up"].Slots.Cap(); i++ {
		if !x.rt.Providers["up"].Slots.TryAcquire() {
			t.Fatalf("could not fill slot %d", i)
		}
	}
	t.Cleanup(func() {
		for i := 0; i < x.rt.Providers["up"].Slots.Cap(); i++ {
			x.rt.Providers["up"].Slots.Release()
		}
	})

	rec := x.do(t, validBody, nil)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), TypeCapacityExhausted) {
		t.Fatalf("body = %s, want capacity_exhausted", rec.Body.String())
	}
	if x.upstream.Requests() != 0 {
		t.Fatal("saturated provider was contacted")
	}
}

// TestServeSystemOne_UpstreamCannotShadowCorrelationHeaders pins that an
// upstream echoing X-Request-Id does not duplicate Jevlar's value.
func TestServeSystemOne_UpstreamCannotShadowCorrelationHeaders(t *testing.T) {
	t.Parallel()
	x := newHarness(t, nil)
	x.upstream.SetHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "upstream-forged")
		w.Header().Set("X-Jevlar-Provider", "spoofed")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	rec := x.do(t, validBody, nil)

	if got := rec.Header().Values("X-Request-Id"); len(got) != 1 || got[0] == "upstream-forged" {
		t.Fatalf("X-Request-Id values = %v, want exactly Jevlar's own id", got)
	}
	if got := rec.Header().Get("X-Jevlar-Provider"); got != "up" {
		t.Fatalf("X-Jevlar-Provider = %q, want the real provider", got)
	}
}

// TestModels_ServesTheOfficialSDKShape pins the exact contract the TypeSafe
// Python SDK validates: a `models` array whose entries carry `name`,
// `description` and `release_date` as strings. The OpenAI-shaped `data`
// array is additive; the SDK ignores unknown fields.
func TestModels_ServesTheOfficialSDKShape(t *testing.T) {
	t.Parallel()
	x := newHarness(t, func(c *config.Config) {
		m := c.Models["jev-latest"]
		m.Description = "Latest general-purpose Jev decision model."
		m.ReleaseDate = "2026-09-15"
		c.Models["jev-latest"] = m
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+testClientToken)
	x.h.Models(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	var payload struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Models) == 0 {
		t.Fatal("no models in the payload")
	}
	for _, entry := range payload.Models {
		for _, field := range []string{"name", "description", "release_date"} {
			v, ok := entry[field]
			if !ok {
				t.Fatalf("model entry missing %q: %+v", field, entry)
			}
			if _, isString := v.(string); !isString {
				t.Fatalf("%q is not a string: %T", field, v)
			}
		}
	}
	first := payload.Models[0]
	if first["name"] != "jev-latest" {
		t.Fatalf("first model = %v, want the configured alias", first["name"])
	}
	if first["description"] != "Latest general-purpose Jev decision model." {
		t.Fatalf("configured description not honoured: %v", first["description"])
	}
	if first["release_date"] != "2026-09-15" {
		t.Fatalf("configured release_date not honoured: %v", first["release_date"])
	}
}

// TestResponses_CarryTheSDKRequestIDHeader pins that every response, error
// or success, exposes x-typesafe-request-id (the SDK reads it as
// error.request_id), and that a provider's own id is preserved separately.
func TestResponses_CarryTheSDKRequestIDHeader(t *testing.T) {
	t.Parallel()
	x := newHarness(t, nil)

	// Error path: Jevlar generates the response, so the id must be ours.
	rec := x.do(t, validBody, func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") })
	if got := rec.Header().Get("X-TypeSafe-Request-Id"); got == "" {
		t.Fatal("no x-typesafe-request-id on a Jevlar-generated error")
	}
	if got, want := rec.Header().Get("X-TypeSafe-Request-Id"), rec.Header().Get("X-Request-Id"); got != want {
		t.Fatalf("x-typesafe-request-id %q != x-request-id %q", got, want)
	}

	// Success path with a provider-supplied id: ours stays primary, the
	// provider's is preserved for support escalations.
	x.upstream.SetHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-typesafe-request-id", "req_provider_123")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	rec = x.do(t, validBody, nil)
	if got := rec.Header().Get("X-TypeSafe-Request-Id"); got != rec.Header().Get("X-Request-Id") {
		t.Fatalf("provider id overrode Jevlar's primary id: %q", got)
	}
	if got := rec.Header().Get("X-TypeSafe-Upstream-Request-Id"); got != "req_provider_123" {
		t.Fatalf("provider request id not preserved: %q", got)
	}
}

// TestModels_RequiresClientAuthentication pins that the model list, which
// names providers, is not disclosed to unauthenticated callers.
func TestModels_RequiresClientAuthentication(t *testing.T) {
	t.Parallel()
	x := newHarness(t, nil)

	rec := httptest.NewRecorder()
	x.h.Models(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /v1/models = %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "typesafe") || strings.Contains(rec.Body.String(), "up") {
		t.Fatalf("model list leaked provider detail to an unauthenticated caller: %s", rec.Body.String())
	}

	authed := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	authed.Header.Set("Authorization", "Bearer "+testClientToken)
	rec = httptest.NewRecorder()
	x.h.Models(rec, authed)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated /v1/models = %d, want 200", rec.Code)
	}
}
