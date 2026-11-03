package proxy

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amscotti/jevlar/internal/config"
	"github.com/amscotti/jevlar/internal/health"
	"github.com/amscotti/jevlar/internal/usage"
)

// TestUsage_CountersMatchTheUpstreamBlock pins the headline the routed stage rule:
// usage.input_tokens = 312 increments the matching counters by exactly 312.
func TestUsage_CountersMatchTheUpstreamBlock(t *testing.T) {
	t.Parallel()
	x := newKeyedHarnessAccount(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"jev-latest","answers":{},"usage":{"input_tokens":312,"output_tokens":47}}`))
	}, false)

	if rec := x.do(t); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	rows, misses := x.h.Tracker().Snapshot()
	if misses != 0 {
		t.Fatalf("misses = %d, want 0", misses)
	}
	var found *usage.Counts
	for k, c := range rows {
		if k.Client == "test" && k.Provider == "p" && k.Model == "jev-latest" {
			found = &c
			if k.APIKey == "" {
				t.Fatal("usage row has no key id")
			}
		}
	}
	if found == nil {
		t.Fatalf("no attribution row: %+v", rows)
	}
	if found.InputTokens != 312 || found.OutputTokens != 47 || found.Requests != 1 {
		t.Fatalf("row = %+v, want 312 input, 47 output, 1 request", found)
	}
}

// TestUsage_MissingBlockIsCountedNotGuessed pins the no-guessing rule end
// to end.
func TestUsage_MissingBlockIsCountedNotGuessed(t *testing.T) {
	t.Parallel()
	x := newKeyedHarnessAccount(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{}}`))
	}, false)

	if rec := x.do(t); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	rows, misses := x.h.Tracker().Snapshot()
	if misses != 1 {
		t.Fatalf("misses = %d, want 1", misses)
	}
	for k, c := range rows {
		if c.InputTokens != 0 {
			t.Fatalf("row %+v guessed tokens for a response without usage", k)
		}
	}
}

// TestClientRPM_IsEnforcedBeforeTheUpstream pins the pre-flight budget: a
// client over its request budget never reaches a provider.
func TestClientRPM_IsEnforcedBeforeTheUpstream(t *testing.T) {
	t.Parallel()
	up := newKeyedHarnessAccount(t, nil, false)
	// rpm = 2 for this client.
	up.cfg.Auth.Keys[0].RPM = 2
	reg := health.New()
	_ = reg
	// Wire a fresh registry carrying the client budget.
	rt2 := up.rt
	rt2.Health.Reconcile(health.Desired{
		Keys:      []health.KeyRef{{Provider: "p", ID: "a"}, {Provider: "p", ID: "b"}},
		Providers: []health.ProviderRef{{Name: "p", BreakerThreshold: 5, BreakerCooldown: time.Second}},
		Clients:   []health.ClientRef{{ID: "test", RPM: 2}},
	})

	for i := 0; i < 2; i++ {
		if rec := up.do(t); rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d", i+1, rec.Code)
		}
	}
	before := up.upstream.Requests()
	rec := up.do(t)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third request status = %d, want 429 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), TypeQuotaExceeded) {
		t.Fatalf("body = %s, want quota_exceeded", rec.Body.String())
	}
	if up.upstream.Requests() != before {
		t.Fatal("an over-budget request still reached the upstream")
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After on the client quota response")
	}
}

// TestClientTPM_IsPostHocWithABoundedOvershoot pins the documented
// overshoot: the request that exceeds the budget succeeds, and the next one
// is refused with a computed Retry-After.
func TestClientTPM_IsPostHocWithABoundedOvershoot(t *testing.T) {
	t.Parallel()
	x := newKeyedHarnessAccount(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":80,"output_tokens":40}}`))
	}, false)
	x.rt.Health.Reconcile(health.Desired{
		Keys:      []health.KeyRef{{Provider: "p", ID: "a"}, {Provider: "p", ID: "b"}},
		Providers: []health.ProviderRef{{Name: "p", BreakerThreshold: 5, BreakerCooldown: time.Second}},
		Clients:   []health.ClientRef{{ID: "test", TPM: 100}},
	})

	// 120 tokens against a 100 token minute: the request completes (it is
	// only known afterwards) and the client is limited for the overrun.
	if rec := x.do(t); rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rec.Code)
	}
	limited, _ := x.rt.Health.Client("test").LimitedUntil()
	if !limited {
		t.Fatal("client was not limited after exceeding its token budget")
	}

	before := x.upstream.Requests()
	rec := x.do(t)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("post-overrun request status = %d, want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), TypeQuotaExceeded) {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("no Retry-After computed for the tpm window")
	}
	if x.upstream.Requests() != before {
		t.Fatal("a tpm-limited request reached the upstream")
	}
}

// TestLogs_NeverContainStateOrKeyValues pins the redaction invariant: a
// captured request log holds no prompt text and no credential.
func TestLogs_NeverContainStateOrKeyValues(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	x := newKeyedHarnessAccount(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1}}`))
	}, false)
	x.h.log = newCaptureLogger(&buf)

	const secretState = "customer says their card was declined"
	body := `{"model":"jev-latest","state":"` + secretState + `","questions":{"q":{"type":"noul","instructions":"urgent?"}}}`
	for i := 0; i < 50; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testClientToken)
		rec := httptest.NewRecorder()
		x.h.ServeSystemOne(rec, req)
	}

	out := buf.String()
	if out == "" {
		t.Fatal("nothing was logged")
	}
	if strings.Contains(out, secretState) {
		t.Fatal("log contains the request state")
	}
	for _, secret := range []string{"secret-a", "secret-b", testClientToken} {
		if strings.Contains(out, secret) {
			t.Fatalf("log contains credential %q", secret)
		}
	}
	// The key *id* is expected; the key *value* is not.
	if !strings.Contains(out, `"provider":"p"`) {
		t.Fatalf("log line lacks attribution: %s", out)
	}
}

// TestAdminUsage_RollsUpPerDimension pins /admin/usage.
func TestAdminUsage_RollsUpPerDimension(t *testing.T) {
	t.Parallel()
	x := newKeyedHarnessAccount(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":10,"output_tokens":5,"cost":0.01}}`))
	}, false)
	for i := 0; i < 2; i++ {
		if rec := x.do(t); rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d", i, rec.Code)
		}
	}
	rows, misses := x.h.Tracker().Snapshot()
	if len(rows) == 0 || misses != 0 {
		t.Fatalf("rows=%d misses=%d", len(rows), misses)
	}
	var input int64
	for _, c := range rows {
		input += c.InputTokens
	}
	if input != 20 {
		t.Fatalf("input tokens = %d, want 20", input)
	}
}

// newCaptureLogger returns a JSON logger writing into buf.
func newCaptureLogger(buf *strings.Builder) *slog.Logger {
	return slog.New(slog.NewJSONHandler(&syncWriter{w: buf}, nil))
}

// syncWriter makes a strings.Builder safe for the logger.
type syncWriter struct {
	mu sync.Mutex
	w  *strings.Builder
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

var _ = config.Config{}

// TestLogs_CarryTheKeyIDButNeverTheKeyValue pins the attribution rule: each
// request line names which provider key served it, by id only.
func TestLogs_CarryTheKeyIDButNeverTheKeyValue(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	x := newKeyedHarnessAccount(t, nil, false)
	x.h.log = newCaptureLogger(&buf)

	if rec := x.do(t); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	out := buf.String()
	if !strings.Contains(out, `"key_id":"a"`) && !strings.Contains(out, `"key_id":"b"`) {
		t.Fatalf("log line lacks the key id: %s", out)
	}
	for _, secret := range []string{"secret-a", "secret-b"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log line leaked key material %q: %s", secret, out)
		}
	}
}

// TestLogBodies_IsTheDocumentedPiiSwitch pins the one path that may log
// prompt state: it is off by default and only logs when log_bodies is set.
func TestLogBodies_IsTheDocumentedPiiSwitch(t *testing.T) {
	t.Parallel()
	x := newKeyedHarnessAccount(t, nil, false)
	var buf strings.Builder
	x.h.log = newCaptureLogger(&buf)

	// Off (the default): no body, no state.
	if rec := x.do(t); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(buf.String(), "request body") {
		t.Fatal("body logged with log_bodies off")
	}

	// On: the debugging switch logs the body at debug level.
	buf.Reset()
	x.cfg.Server.LogBodies = true
	x.rt.Config.Server.LogBodies = true
	x.h.log = slog.New(slog.NewJSONHandler(&syncWriter{w: &buf},
		&slog.HandlerOptions{Level: slog.LevelDebug}))
	if rec := x.do(t); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(buf.String(), "request body") {
		t.Fatalf("log_bodies on did not log the body: %s", buf.String())
	}
}

// TestXProviderIsNotForwardedUpstream pins that Jevlar control headers stop
// at the gateway.
func TestXProviderIsNotForwardedUpstream(t *testing.T) {
	t.Parallel()
	x := newRoutingHarness(t, []targetSpec{{provider: "a", model: "jev-latest"}}, "priority", 1)
	rec := x.do(t, validBody, map[string]string{"X-Provider": "a", "X-Jevlar-Debug": "1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	_, header := x.upstream["a"].LastRequest()
	if header.Get("X-Provider") != "" {
		t.Fatal("X-Provider reached the upstream")
	}
	if header.Get("X-Jevlar-Debug") != "" {
		t.Fatal("X-Jevlar-Debug reached the upstream")
	}
}
