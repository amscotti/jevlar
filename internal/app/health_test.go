package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/amscotti/jevlar/internal/config"
	"github.com/amscotti/jevlar/internal/health"
	"github.com/amscotti/jevlar/internal/metrics"
	"github.com/amscotti/jevlar/internal/runtime"
)

func newTestServer(now func() time.Time) *server {
	reg := health.New()
	if now != nil {
		reg.SetClock(now)
	}
	return &server{
		metrics: metrics.New(metrics.Options{Version: "test", GoVersion: "go1"}),
		health:  reg,
	}
}

func buildAndAttach(t *testing.T, s *server, cfg *config.Config) *runtime.Runtime {
	t.Helper()
	rt, err := runtime.Build(cfg)
	if err != nil {
		t.Fatalf("runtime.Build: %v", err)
	}
	s.attachHealth(cfg, rt)
	s.rt.Store(rt)
	return rt
}

func callReadyz(s *server) (int, string) {
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	s.handleReadyz(rec, req)
	return rec.Code, rec.Body.String()
}

func TestReadyz_AuthNoneAndXGoogAPIKey(t *testing.T) {
	t.Parallel()

	t.Run("nil snapshot is not ready", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(nil)
		code, body := callReadyz(s)
		if code != http.StatusServiceUnavailable || !strings.Contains(body, `"not_ready"`) {
			t.Fatalf("/readyz = %d %q, want 503 not_ready", code, body)
		}
	})

	t.Run("auth none breaker closed and open", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(nil)
		cfg := &config.Config{
			ConfigVersion: 1,
			Server:        config.Server{Listen: "127.0.0.1:0", MaxResponseBytes: 1024},
			Providers: []config.Provider{{
				Name: "local", BaseURL: "http://127.0.0.1:9", Path: "/v1/systemone",
				Auth: config.AuthNone, TimeoutMs: 1000, MaxConcurrency: 4,
			}},
			Breaker: config.Breaker{Threshold: 2, CooldownMs: 30_000},
		}
		buildAndAttach(t, s, cfg)

		if code, body := callReadyz(s); code != http.StatusOK || !strings.Contains(body, `"ready"`) {
			t.Fatalf("breaker closed: /readyz = %d %q, want 200 ready", code, body)
		}

		br := s.health.Breaker("local")
		br.RecordFailure()
		br.RecordFailure()
		if code, body := callReadyz(s); code != http.StatusServiceUnavailable || !strings.Contains(body, `"not_ready"`) {
			t.Fatalf("breaker open: /readyz = %d %q, want 503 not_ready", code, body)
		}
	})

	t.Run("auth x-goog-api-key healthy, cooling and disabled", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(nil)
		cfg := &config.Config{
			ConfigVersion: 1,
			Server:        config.Server{Listen: "127.0.0.1:0", MaxResponseBytes: 1024},
			Accounts:      map[string]config.Account{"gemini-main": {RPM: 60, TPM: 1_000_000}},
			Providers: []config.Provider{{
				Name: "gemini", BaseURL: "https://generativelanguage.googleapis.com", Path: "/v1beta/decisions",
				Auth: config.AuthXGoogAPIKey, WireFormat: config.WireGeminiDecisions, Account: "gemini-main",
				TimeoutMs: 1000, MaxConcurrency: 4,
				APIKeys: []config.APIKey{{ID: "k1", Resolved: "gm-secret"}},
			}},
			Breaker: config.Breaker{Threshold: 2, CooldownMs: 30_000},
		}
		buildAndAttach(t, s, cfg)

		if code, body := callReadyz(s); code != http.StatusOK || !strings.Contains(body, `"ready"`) {
			t.Fatalf("healthy key: /readyz = %d %q, want 200 ready", code, body)
		}

		k := s.health.Key(health.KeyRef{Provider: "gemini", ID: "k1"})
		k.CoolFor(time.Minute)
		if code, body := callReadyz(s); code != http.StatusServiceUnavailable || !strings.Contains(body, `"not_ready"`) {
			t.Fatalf("cooling key: /readyz = %d %q, want 503 not_ready", code, body)
		}

		k.Enable()
		if code, body := callReadyz(s); code != http.StatusOK || !strings.Contains(body, `"ready"`) {
			t.Fatalf("re-enabled key: /readyz = %d %q, want 200 ready", code, body)
		}

		k.Disable("API_KEY_INVALID")
		if code, body := callReadyz(s); code != http.StatusServiceUnavailable || !strings.Contains(body, `"not_ready"`) {
			t.Fatalf("disabled key: /readyz = %d %q, want 503 not_ready", code, body)
		}

		// Re-attaching with a cooling/disabled key and cooling account syncs gauges cleanly.
		s.health.Account("gemini-main").CoolFor(time.Minute)
		buildAndAttach(t, s, cfg)
		k.Enable()
		k.CoolFor(time.Minute)
		buildAndAttach(t, s, cfg)
	})
}

func TestSyncBreakerGauge_AllStates(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	s := newTestServer(func() time.Time { return now })
	cfg := &config.Config{
		ConfigVersion: 1,
		Server:        config.Server{Listen: "127.0.0.1:0", MaxResponseBytes: 1024},
		Providers: []config.Provider{{
			Name: "p", BaseURL: "https://api.example", Path: "/v1/systemone",
			Auth: config.AuthNone, TimeoutMs: 1000, MaxConcurrency: 2,
		}},
		Breaker: config.Breaker{Threshold: 2, CooldownMs: 5_000},
	}
	buildAndAttach(t, s, cfg)

	readMetric := func() string {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		rec := httptest.NewRecorder()
		s.metrics.Handler().ServeHTTP(rec, req)
		return rec.Body.String()
	}

	br := s.health.Breaker("p")
	// Closed -> 0
	s.syncBreakerGauge("p", br)
	if body := readMetric(); !strings.Contains(body, `jevlar_breaker_state{provider="p"} 0`) {
		t.Fatalf("closed gauge missing:\n%s", body)
	}

	// Open -> 2
	br.RecordFailure()
	br.RecordFailure()
	s.syncBreakerGauge("p", br)
	if body := readMetric(); !strings.Contains(body, `jevlar_breaker_state{provider="p"} 2`) {
		t.Fatalf("open gauge missing:\n%s", body)
	}

	// Half-open (cooldown elapsed) -> 1
	now = now.Add(6 * time.Second)
	s.syncBreakerGauge("p", br)
	if body := readMetric(); !strings.Contains(body, `jevlar_breaker_state{provider="p"} 1`) {
		t.Fatalf("half-open gauge missing:\n%s", body)
	}
}
