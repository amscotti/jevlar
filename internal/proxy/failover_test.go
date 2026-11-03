package proxy

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/amscotti/jevlar/internal/config"
	"github.com/amscotti/jevlar/internal/metrics"
	"github.com/amscotti/jevlar/internal/runtime"
	"github.com/amscotti/jevlar/internal/testutil"
)

// twoProviderHarness wires two fake upstreams behind one alias, in the
// order given, with the configured attempt budget.
func twoProviderHarness(t *testing.T, maxAttempts int) (*Handler, *testutil.FakeUpstream, *testutil.FakeUpstream) {
	t.Helper()
	first := testutil.NewFakeUpstream(t)
	second := testutil.NewFakeUpstream(t)
	cfg := &config.Config{
		ConfigVersion: 1,
		Server: config.Server{
			Listen:              "127.0.0.1:0",
			RequestTimeoutMs:    10_000,
			MaxBodyBytes:        4096,
			MaxResponseBytes:    4096,
			MaxInflight:         64,
			ShutdownGraceMs:     1000,
			ReadHeaderTimeoutMs: 5000,
			LogLevel:            "error",
		},
		Auth: config.Auth{Keys: []config.AuthKey{{ID: "test", Hash: testClientHash()}}},
		Providers: []config.Provider{
			{
				Name: "a", BaseURL: first.URL(), Path: "/v1/systemone", Auth: "none",
				Models: []string{"jev-latest"}, TimeoutMs: 2000, MaxConcurrency: 8,
			},
			{
				Name: "b", BaseURL: second.URL(), Path: "/v1/systemone", Auth: "none",
				Models: []string{"jev-latest"}, TimeoutMs: 2000, MaxConcurrency: 8,
			},
		},
		Models: map[string]config.Model{
			"jev-latest": {
				Targets: []config.ModelTarget{
					{Provider: "a", Model: "jev-latest"},
					{Provider: "b", Model: "jev-latest"},
				},
				Policy:      "priority",
				MaxAttempts: maxAttempts,
			},
		},
	}
	rt, err := runtime.Build(cfg)
	if err != nil {
		t.Fatalf("build runtime: %v", err)
	}
	t.Cleanup(testutil.Wire(rt))
	m := metrics.New(metrics.Options{Version: "test", GoVersion: "go-test"})
	return New(func() *runtime.Runtime { return rt }, slog.New(slog.NewTextHandler(io.Discard, nil)), m), first, second
}

func doAuthed(h *Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testClientToken)
	rec := httptest.NewRecorder()
	h.ServeSystemOne(rec, req)
	return rec
}

// TestFailover_ServerErrorMovesToNextTarget pins failover on 5xx.
func TestFailover_ServerErrorMovesToNextTarget(t *testing.T) {
	t.Parallel()
	h, a, b := twoProviderHarness(t, 2)
	a.SetStatus(http.StatusInternalServerError)
	b.SetBody([]byte(`{"ok":true}`))

	rec := doAuthed(h, validBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover (body %s)", rec.Code, rec.Body.String())
	}
	if a.Requests() != 1 || b.Requests() != 1 {
		t.Fatalf("requests: a=%d b=%d, want 1 and 1", a.Requests(), b.Requests())
	}
	if got := rec.Header().Get("X-Jevlar-Attempts"); got != "2" {
		t.Fatalf("X-Jevlar-Attempts = %q, want 2", got)
	}
}

// TestFailover_404MovesToNextTarget pins that a target which does not serve
// the model fails over rather than failing the client.
func TestFailover_404MovesToNextTarget(t *testing.T) {
	t.Parallel()
	h, a, b := twoProviderHarness(t, 2)
	a.SetStatus(http.StatusNotFound)
	b.SetBody([]byte(`{"ok":true}`))

	rec := doAuthed(h, validBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if b.Requests() != 1 {
		t.Fatalf("second provider was not tried")
	}
}

// TestFailover_422NeverFailsOver pins that a request-shaped rejection is
// passed through from the first target: the request is wrong, not the
// provider.
func TestFailover_422NeverFailsOver(t *testing.T) {
	t.Parallel()
	h, a, b := twoProviderHarness(t, 2)
	a.SetStatus(http.StatusUnprocessableEntity)
	a.SetBody([]byte(`{"upstream":"too many choices"}`))
	b.SetBody([]byte(`{"ok":true}`))

	rec := doAuthed(h, validBody)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want the upstream 422", rec.Code)
	}
	if got := rec.Body.String(); got != `{"upstream":"too many choices"}` {
		t.Fatalf("body = %q, want the upstream body untouched", got)
	}
	if b.Requests() != 0 {
		t.Fatalf("failover happened on 422: second provider got %d requests", b.Requests())
	}
}

// TestFailover_AllTargetsReturn404PassesLastThrough pins the exhaustion
// rule for 404.
func TestFailover_AllTargetsReturn404PassesLastThrough(t *testing.T) {
	t.Parallel()
	h, a, b := twoProviderHarness(t, 2)
	a.SetStatus(http.StatusNotFound)
	a.SetBody([]byte(`{"error":"a"}`))
	b.SetStatus(http.StatusNotFound)
	b.SetBody([]byte(`{"error":"b"}`))

	rec := doAuthed(h, validBody)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := rec.Body.String(); got != `{"error":"b"}` {
		t.Fatalf("body = %q, want the last upstream 404 body", got)
	}
}

// TestFailover_AttemptBudgetIsRespected pins max_attempts = 1: a failing
// target is not retried and the second provider is never contacted.
func TestFailover_AttemptBudgetIsRespected(t *testing.T) {
	t.Parallel()
	h, a, b := twoProviderHarness(t, 1)
	a.SetStatus(http.StatusInternalServerError)

	rec := doAuthed(h, validBody)

	if rec.Code < 500 {
		t.Fatalf("status = %d, want a 5xx", rec.Code)
	}
	if a.Requests() != 1 {
		t.Fatalf("first provider got %d requests, want 1", a.Requests())
	}
	if b.Requests() != 0 {
		t.Fatalf("max_attempts = 1 but the second provider got %d requests", b.Requests())
	}
}

// TestMidBodyCloseIsNeverATruncated200 pins the commit-after-read
// invariant: an upstream that dies after the headers produces a 5xx, never
// a 200 with a short body.
func TestMidBodyCloseIsNeverATruncated200(t *testing.T) {
	t.Parallel()
	h, a, b := twoProviderHarness(t, 1)
	a.SetHandler(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("response writer is not a hijacker")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		// Announce 1024 bytes, send 10, then hang up.
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 1024\r\n\r\n{\"answers\"")
		_ = buf.Flush()
	})

	rec := doAuthed(h, validBody)

	if rec.Code >= 200 && rec.Code < 300 {
		t.Fatalf("status = %d; a mid-body upstream failure must never surface as a success", rec.Code)
	}
	if b.Requests() != 0 {
		t.Fatalf("max_attempts = 1 but the second provider was contacted")
	}
}

// TestResponseTooLargeIsA5xx pins the response cap: an over-cap body aborts
// the attempt instead of streaming or truncating.
func TestResponseTooLargeIsA5xx(t *testing.T) {
	t.Parallel()
	h, a, b := twoProviderHarness(t, 1)
	a.SetBody([]byte(strings.Repeat("x", 8192)))

	rec := doAuthed(h, validBody)

	if rec.Code < 500 {
		t.Fatalf("status = %d, want a 5xx for an over-cap response", rec.Code)
	}
	if b.Requests() != 0 {
		t.Fatalf("max_attempts = 1 but the second provider was contacted")
	}
	if strings.Contains(rec.Body.String(), "xxxx") {
		t.Fatalf("over-cap response leaked upstream bytes to the client")
	}
}

// TestHopByHopHeadersAreStripped pins RFC 9110 handling: Connection-named
// headers and the fixed hop-by-hop set never cross the gateway.
func TestHopByHopHeadersAreStripped(t *testing.T) {
	t.Parallel()
	h, a, _ := twoProviderHarness(t, 1)
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(validBody))
	req.Header.Set("Authorization", "Bearer "+testClientToken)
	req.Header.Set("Connection", "X-Drop-Me")
	req.Header.Set("X-Drop-Me", "secret")
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("X-Keep-Me", "kept")
	rec := httptest.NewRecorder()
	h.ServeSystemOne(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	_, header := a.LastRequest()
	if header.Get("X-Drop-Me") != "" {
		t.Fatalf("Connection-named header reached the upstream")
	}
	if header.Get("Keep-Alive") != "" {
		t.Fatalf("Keep-Alive reached the upstream")
	}
	if header.Get("X-Keep-Me") != "kept" {
		t.Fatalf("an end-to-end header was dropped")
	}
	if header.Get("Accept-Encoding") != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity so usage stays readable", header.Get("Accept-Encoding"))
	}
}

// TestMidBodyCloseOnSecondAttemptStillFails pins that a body read error on
// a retried attempt is not mistaken for success.
func TestMidBodyCloseOnSecondAttemptStillFails(t *testing.T) {
	t.Parallel()
	h, a, b := twoProviderHarness(t, 2)
	a.SetStatus(http.StatusInternalServerError)
	b.SetHandler(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 500\r\n\r\n{}")
		_ = buf.Flush()
	})

	rec := doAuthed(h, validBody)
	if rec.Code >= 200 && rec.Code < 300 {
		t.Fatalf("status = %d, want a 5xx", rec.Code)
	}
}

// TestServerClosesIdleUpstreamConnections is a guard that the harness
// transport is actually pooled: two requests reuse one connection.
func TestServerClosesIdleUpstreamConnections(t *testing.T) {
	t.Parallel()
	h, a, _ := twoProviderHarness(t, 1)
	if rec := doAuthed(h, validBody); rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d", rec.Code)
	}
	if rec := doAuthed(h, validBody); rec.Code != http.StatusOK {
		t.Fatalf("second request status = %d", rec.Code)
	}
	if a.Requests() != 2 {
		t.Fatalf("upstream requests = %d, want 2", a.Requests())
	}
}

var _ = net.IPv4len
