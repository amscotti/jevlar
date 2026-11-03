package proxy

import (
	"crypto/sha256"
	"encoding/hex"
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

const testClientToken = "test-client-token"

// testClientHash is the SHA-256 of testClientToken, formatted for config.
func testClientHash() string {
	sum := sha256.Sum256([]byte(testClientToken))
	return hex.EncodeToString(sum[:])
}

// harness bundles a handler with the snapshot and upstreams it serves.
type harness struct {
	h        *Handler
	rt       *runtime.Runtime
	cfg      *config.Config
	upstream *testutil.FakeUpstream
}

// newHarness builds a one-provider configuration pointing at a fake
// upstream, with request/response caps applied from opts.
func newHarness(t *testing.T, tune func(*config.Config)) *harness {
	t.Helper()
	up := testutil.NewFakeUpstream(t)
	cfg := &config.Config{
		ConfigVersion: 1,
		Server: config.Server{
			Listen:              "127.0.0.1:0",
			RequestTimeoutMs:    30_000,
			MaxBodyBytes:        4096,
			MaxResponseBytes:    4096,
			MaxInflight:         64,
			ShutdownGraceMs:     1000,
			ReadHeaderTimeoutMs: 5000,
			LogLevel:            "error",
		},
		Auth: config.Auth{Keys: []config.AuthKey{{
			ID: "test", Hash: testClientHash(), RPM: 600,
		}}},
		Providers: []config.Provider{{
			Name: "up", BaseURL: up.URL(), Path: "/v1/systemone",
			Auth: "none", Models: []string{"jev-latest"},
			TimeoutMs: 5000, MaxConcurrency: 8,
		}},
		Models: map[string]config.Model{
			"jev-latest": {
				Targets:     []config.ModelTarget{{Provider: "up", Model: "jev-latest"}},
				Policy:      "priority",
				MaxAttempts: 2,
			},
		},
	}
	if tune != nil {
		tune(cfg)
	}
	rt, err := runtime.Build(cfg)
	if err != nil {
		t.Fatalf("build runtime: %v", err)
	}
	cleanup := testutil.Wire(rt)
	t.Cleanup(cleanup)
	m := metrics.New(metrics.Options{Version: "test", GoVersion: "go-test"})
	h := New(func() *runtime.Runtime { return rt }, slog.New(slog.NewTextHandler(io.Discard, nil)), m)
	return &harness{h: h, rt: rt, cfg: cfg, upstream: up}
}

func (x *harness) do(t *testing.T, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testClientToken)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	x.h.ServeSystemOne(rec, req)
	return rec
}

const validBody = `{"model":"jev-latest","state":"a customer cannot connect stripe","questions":{"urgent":{"type":"noul","instructions":"urgent?"}}}`

// TestServeSystemOne_ForwardsBodyByteIdentically pins the pass-through
// contract: with no rewrite configured the upstream receives exactly the
// bytes the client sent, and the response body is exactly what the
// upstream returned.
func TestServeSystemOne_ForwardsBodyByteIdentically(t *testing.T) {
	t.Parallel()
	x := newHarness(t, nil)
	want := sha256.Sum256([]byte(validBody))
	x.upstream.SetBody([]byte(`{"model":"jev-latest","answers":{"urgent":{"noul":0.9}}}`))

	rec := x.do(t, validBody, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Body-Sha256"); got != hex.EncodeToString(want[:]) {
		t.Fatalf("upstream body hash = %s, want %s", got, hex.EncodeToString(want[:]))
	}
	if got := rec.Body.String(); got != `{"model":"jev-latest","answers":{"urgent":{"noul":0.9}}}` {
		t.Fatalf("response body = %q", got)
	}
}

// TestServeSystemOne_ClientAuthorizationNeverReachesUpstream pins that the
// inbound credential is replaced by provider authentication (or removed
// entirely for auth = "none").
func TestServeSystemOne_ClientAuthorizationNeverReachesUpstream(t *testing.T) {
	t.Parallel()
	x := newHarness(t, nil)
	rec := x.do(t, validBody, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	_, header := x.upstream.LastRequest()
	if got := header.Get("Authorization"); got != "" {
		t.Fatalf("upstream saw Authorization %q; the client credential must never be forwarded", got)
	}
}

// TestServeSystemOne_BearerProviderGetsConfiguredKey pins that a bearer
// provider receives its own key, not the client's token.
func TestServeSystemOne_BearerProviderGetsConfiguredKey(t *testing.T) {
	t.Parallel()
	x := newHarness(t, func(c *config.Config) {
		c.Providers[0].Auth = "bearer"
		c.Providers[0].APIKeys = []config.APIKey{{ID: "k1", Resolved: "provider-secret"}}
	})
	rec := x.do(t, validBody, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	_, header := x.upstream.LastRequest()
	if got := header.Get("Authorization"); got != "Bearer provider-secret" {
		t.Fatalf("upstream Authorization = %q, want the provider key", got)
	}
}

// TestServeSystemOne_RejectsUnknownClient pins the 401-before-upstream
// rule: an unknown token never reaches a provider.
func TestServeSystemOne_RejectsUnknownClient(t *testing.T) {
	t.Parallel()
	x := newHarness(t, nil)
	rec := x.do(t, validBody, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer nope")
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if x.upstream.Requests() != 0 {
		t.Fatalf("upstream received %d requests; an unauthenticated request must not be forwarded", x.upstream.Requests())
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	if env.Error.Type != TypeUnauthorized || env.Error.RequestID == "" {
		t.Fatalf("error envelope = %+v", env.Error)
	}
}

// TestServeSystemOne_RejectsOversizedBody pins 413 without upstream
// contact, for both the Content-Length short circuit and the streaming
// (chunked) path.
func TestServeSystemOne_RejectsOversizedBody(t *testing.T) {
	t.Parallel()
	x := newHarness(t, func(c *config.Config) { c.Server.MaxBodyBytes = 64 })

	big := strings.Repeat("a", 200)
	body := `{"model":"jev-latest","state":"` + big + `"}`

	rec := x.do(t, body, nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared length: status = %d, want 413", rec.Code)
	}

	// Unknown length: the cap must still hold while streaming.
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(body))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+testClientToken)
	rec = httptest.NewRecorder()
	x.h.ServeSystemOne(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("unknown length: status = %d, want 413", rec.Code)
	}
	if x.upstream.Requests() != 0 {
		t.Fatalf("upstream received %d requests for oversized bodies", x.upstream.Requests())
	}
}

// TestServeSystemOne_MissingModelIs400 pins the request-shape rules.
func TestServeSystemOne_MissingModelIs400(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{"no model key", `{"state":"x"}`},
		{"nested model only", `{"questions":{"model":"x"}}`},
		{"model not a string", `{"model":42}`},
		{"duplicate model keys", `{"model":"a","model":"b"}`},
		{"not an object", `["model"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			x := newHarness(t, nil)
			rec := x.do(t, tc.body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			if x.upstream.Requests() != 0 {
				t.Fatalf("upstream received %d requests for a malformed body", x.upstream.Requests())
			}
		})
	}
}

// TestServeSystemOne_UpstreamTimeoutIs504 pins the timeout mapping and that
// the client is not left waiting.
func TestServeSystemOne_UpstreamTimeoutIs504(t *testing.T) {
	t.Parallel()
	x := newHarness(t, func(c *config.Config) {
		c.Providers[0].TimeoutMs = 50
		c.Models["jev-latest"] = config.Model{
			Targets:     []config.ModelTarget{{Provider: "up", Model: "jev-latest"}},
			Policy:      "priority",
			MaxAttempts: 1,
		}
	})
	x.upstream.SetDelay(2 * time.Second)
	rec := x.do(t, validBody, nil)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (body %s)", rec.Code, rec.Body.String())
	}
}
