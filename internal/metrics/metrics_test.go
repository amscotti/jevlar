package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

const wantBuildInfo = `
# HELP jevlar_build_info Build information for the Jevlar gateway; the value is always 1.
# TYPE jevlar_build_info gauge
jevlar_build_info{go_version="go1.27.3",version="v0.0.1-test"} 1
`

func TestNew_RegistersBuildInfoWithNameAndLabels(t *testing.T) {
	t.Parallel()
	m := New(Options{Version: "v0.0.1-test", GoVersion: "go1.27.3"})
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(wantBuildInfo), "jevlar_build_info"); err != nil {
		t.Fatalf("registry exposition mismatch: %v", err)
	}
}

func TestNew_DoesNotUseTheGlobalRegistry(t *testing.T) {
	t.Parallel()
	before := globalFamilyNames(t)
	New(Options{Version: "v0.0.1-test", GoVersion: "go1.27.3"})
	after := globalFamilyNames(t)
	if strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("global registry changed: before %v, after %v", before, after)
	}
}

func TestHandler_ServesPrometheusTextWithBuildInfo(t *testing.T) {
	t.Parallel()
	m := New(Options{Version: "v0.0.1-test", GoVersion: "go1.27.3"})
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup; read error below is what matters
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content type = %q, want text/plain", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), `jevlar_build_info{go_version="go1.27.3",version="v0.0.1-test"} 1`) {
		t.Fatalf("body does not contain jevlar_build_info:\n%s", body)
	}
}

// globalFamilyNames snapshots the process-global registry's family names.
func globalFamilyNames(t *testing.T) []string {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather default: %v", err)
	}
	names := make([]string, 0, len(families))
	for _, f := range families {
		names = append(names, f.GetName())
	}
	return names
}

// TestNew_RegistersConfigReloadFailuresCounter: the counter exists under
// its exact name, starts at zero and increments.
func TestNew_RegistersConfigReloadFailuresCounter(t *testing.T) {
	t.Parallel()
	m := New(Options{Version: "v0.0.1-test", GoVersion: "go1.27.3"})
	if got := testutil.ToFloat64(m.ConfigReloadFailures()); got != 0 {
		t.Fatalf("counter starts at %v, want 0", got)
	}
	m.ConfigReloadFailures().Inc()
	want := `
# HELP jevlar_config_reload_failures_total Total number of failed configuration reloads (SIGHUP or /admin/reload); the previous runtime keeps serving.
# TYPE jevlar_config_reload_failures_total counter
jevlar_config_reload_failures_total 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want), "jevlar_config_reload_failures_total"); err != nil {
		t.Fatalf("registry exposition mismatch: %v", err)
	}
}

// TestRequestMetrics_HaveExactNamesAndLabels pins the documented metric
// surface: every counter/histogram Jevlar exposes carries the labels the
// plan promises, and recording through the accessors is observable.
func TestRequestMetrics_HaveExactNamesAndLabels(t *testing.T) {
	t.Parallel()
	m := New(Options{Version: "v0.0.1-test", GoVersion: "go1.27.3"})

	m.Request("app-prod", "typesafe", "jev-latest", "2xx")
	m.RequestDuration("typesafe", "jev-latest", 12.5)
	m.Attempt("typesafe", "success")
	m.UpstreamError("typesafe", "timeout")
	m.ResponseTooLarge()
	m.InflightAdd(1)

	cases := []struct {
		name   string
		metric string
		want   string
	}{
		{
			name:   "requests",
			metric: "jevlar_requests_total",
			want: `# HELP jevlar_requests_total Total client requests handled, by client, provider, resolved model alias and status class.
# TYPE jevlar_requests_total counter
jevlar_requests_total{client="app-prod",model="jev-latest",provider="typesafe",status="2xx"} 1
`,
		},
		{
			name:   "upstream errors",
			metric: "jevlar_upstream_errors_total",
			want: `# HELP jevlar_upstream_errors_total Upstream failures, by provider and reason (transport, timeout, too_large, status).
# TYPE jevlar_upstream_errors_total counter
jevlar_upstream_errors_total{provider="typesafe",reason="timeout"} 1
`,
		},
		{
			name:   "attempts",
			metric: "jevlar_attempts_total",
			want: `# HELP jevlar_attempts_total Upstream attempts, by provider and outcome (success, retryable, passthrough, error).
# TYPE jevlar_attempts_total counter
jevlar_attempts_total{provider="typesafe",result="success"} 1
`,
		},
		{
			name:   "response too large",
			metric: "jevlar_response_too_large_total",
			want: `# HELP jevlar_response_too_large_total Upstream responses rejected because they exceeded server.max_response_bytes.
# TYPE jevlar_response_too_large_total counter
jevlar_response_too_large_total 1
`,
		},
		{
			name:   "inflight",
			metric: "jevlar_requests_inflight",
			want: `# HELP jevlar_requests_inflight Requests currently admitted by Jevlar, bounded by server.max_inflight.
# TYPE jevlar_requests_inflight gauge
jevlar_requests_inflight 1
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(tc.want), tc.metric); err != nil {
				t.Fatalf("%s exposition mismatch: %v", tc.metric, err)
			}
		})
	}

	// The latency histogram exists with the documented labels and observes
	// the value it was given.
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "jevlar_request_duration_milliseconds" {
			continue
		}
		for _, metric := range f.GetMetric() {
			if len(metric.GetHistogram().GetBucket()) == 0 {
				t.Fatal("duration histogram has no buckets")
			}
			if got := metric.GetHistogram().GetSampleCount(); got != 1 {
				t.Fatalf("duration observations = %d, want 1", got)
			}
			return
		}
		t.Fatal("duration histogram has no metric series")
	}
	t.Fatal("jevlar_request_duration_milliseconds not registered")
}

// TestHealthMetrics_HaveExactNamesAndLabels pins the the routed stage metric
// surface: key state, breaker state, suspect-key counter and account
// cooldown all exist under their documented names with their labels.
func TestHealthMetrics_HaveExactNamesAndLabels(t *testing.T) {
	t.Parallel()
	m := New(Options{Version: "v0.0.1-test", GoVersion: "go1.27.3"})

	m.KeyState("typesafe", "primary", 1)
	m.BreakerState("typesafe", 2)
	m.KeySuspect()
	m.AccountCooldown("typesafe-main", 42)

	cases := []struct{ metric, want string }{
		{"jevlar_key_state", `# HELP jevlar_key_state Provider key state: 0 healthy, 1 cooldown, 2 disabled.
# TYPE jevlar_key_state gauge
jevlar_key_state{key="primary",provider="typesafe"} 1
`},
		{"jevlar_breaker_state", `# HELP jevlar_breaker_state Provider circuit breaker state: 0 closed, 1 half-open, 2 open.
# TYPE jevlar_breaker_state gauge
jevlar_breaker_state{provider="typesafe"} 2
`},
		{"jevlar_key_suspect_total", `# HELP jevlar_key_suspect_total Upstream 403 responses; the key was cooled but not disabled, because 403 also means quota or plan limits.
# TYPE jevlar_key_suspect_total counter
jevlar_key_suspect_total 1
`},
		{"jevlar_account_cooldown_seconds", `# HELP jevlar_account_cooldown_seconds Seconds remaining on an account-wide cooldown; 0 when the account is not cooling.
# TYPE jevlar_account_cooldown_seconds gauge
jevlar_account_cooldown_seconds{account="typesafe-main"} 42
`},
	}
	for _, tc := range cases {
		t.Run(tc.metric, func(t *testing.T) {
			t.Parallel()
			if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(tc.want), tc.metric); err != nil {
				t.Fatalf("%s exposition mismatch: %v", tc.metric, err)
			}
		})
	}
}

// TestUsageMetrics_HaveExactNamesAndLabels pins the the routed stage metric surface.
func TestUsageMetrics_HaveExactNamesAndLabels(t *testing.T) {
	t.Parallel()
	m := New(Options{Version: "v0.0.1-test", GoVersion: "go1.27.3"})
	m.Usage("app", "typesafe", "jev-latest", 312, 47, 0.25)
	m.UsageMissing()
	m.ClientLimited("app", "rpm")

	cases := []struct{ metric, want string }{
		{"jevlar_input_tokens_total", `# HELP jevlar_input_tokens_total Upstream-reported input tokens, by client, provider and model.
# TYPE jevlar_input_tokens_total counter
jevlar_input_tokens_total{client="app",model="jev-latest",provider="typesafe"} 312
`},
		{"jevlar_output_tokens_total", `# HELP jevlar_output_tokens_total Upstream-reported output tokens, by client, provider and model.
# TYPE jevlar_output_tokens_total counter
jevlar_output_tokens_total{client="app",model="jev-latest",provider="typesafe"} 47
`},
		{"jevlar_cost_total", `# HELP jevlar_cost_total Upstream-reported cost in the provider's currency, by client, provider and model.
# TYPE jevlar_cost_total counter
jevlar_cost_total{client="app",model="jev-latest",provider="typesafe"} 0.25
`},
		{"jevlar_usage_missing_total", `# HELP jevlar_usage_missing_total Successful responses that carried no usage block; tokens are never guessed.
# TYPE jevlar_usage_missing_total counter
jevlar_usage_missing_total 1
`},
		{"jevlar_client_limited_total", `# HELP jevlar_client_limited_total Requests refused because the client was over its rpm or tpm budget.
# TYPE jevlar_client_limited_total counter
jevlar_client_limited_total{client="app",reason="rpm"} 1
`},
	}
	for _, tc := range cases {
		t.Run(tc.metric, func(t *testing.T) {
			t.Parallel()
			if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(tc.want), tc.metric); err != nil {
				t.Fatalf("%s exposition mismatch: %v", tc.metric, err)
			}
		})
	}
}

// TestExposition_ParsesAsPrometheusText is the promtool substitute: it
// renders the private registry exactly as /metrics does and re-parses the
// text with the Prometheus text parser, failing on any malformed family,
// duplicate metric, or invalid label.
func TestExposition_ParsesAsPrometheusText(t *testing.T) {
	t.Parallel()
	m := New(Options{Version: "v0.0.1-test", GoVersion: "go1.27.3"})
	// Touch every family so the exposition is complete.
	m.Request("app", "typesafe", "jev-latest", "2xx")
	m.RequestDuration("typesafe", "jev-latest", 12)
	m.Attempt("typesafe", "success")
	m.UpstreamError("typesafe", "timeout")
	m.ResponseTooLarge()
	m.InflightAdd(1)
	m.KeyState("typesafe", "primary", 1)
	m.BreakerState("typesafe", 0)
	m.KeySuspect()
	m.AccountCooldown("acct", 3)
	m.Usage("app", "typesafe", "jev-latest", 10, 2, 0.01)
	m.UsageMissing()
	m.ClientLimited("app", "rpm")
	m.ConfigReloadFailures().Inc()

	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		t.Fatalf("prometheus text parser rejected the exposition: %v", err)
	}
	want := []string{
		"jevlar_build_info",
		"jevlar_requests_total",
		"jevlar_request_duration_milliseconds",
		"jevlar_attempts_total",
		"jevlar_upstream_errors_total",
		"jevlar_response_too_large_total",
		"jevlar_requests_inflight",
		"jevlar_key_state",
		"jevlar_breaker_state",
		"jevlar_key_suspect_total",
		"jevlar_account_cooldown_seconds",
		"jevlar_input_tokens_total",
		"jevlar_output_tokens_total",
		"jevlar_cost_total",
		"jevlar_usage_missing_total",
		"jevlar_client_limited_total",
		"jevlar_config_reload_failures_total",
	}
	for _, name := range want {
		if _, ok := families[name]; !ok {
			t.Fatalf("metric family %q missing from the exposition", name)
		}
	}
}

// TestPruneIdentities_RemovesStaleGauges pins that a reload which drops a
// provider, key or account removes the corresponding gauge series instead
// of leaving them at their last value forever.
func TestPruneIdentities_RemovesStaleGauges(t *testing.T) {
	t.Parallel()
	m := New(Options{Version: "v", GoVersion: "go"})
	m.KeyState("p1", "k1", 1)
	m.KeyState("p2", "k2", 2)
	m.BreakerState("p1", 2)
	m.BreakerState("p2", 2)
	m.AccountCooldown("acct1", 30)
	m.AccountCooldown("acct2", 30)

	m.PruneIdentities(map[string]map[string]bool{"p1": {"k1": true}}, []string{"p1"}, []string{"acct1"})

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	kept := map[string]bool{}
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			values := map[string]string{}
			for _, label := range metric.GetLabel() {
				values[label.GetName()] = label.GetValue()
			}
			for _, stale := range []string{"p2", "k2", "acct2"} {
				if values["provider"] == stale || values["key"] == stale || values["account"] == stale {
					t.Fatalf("stale series survived pruning: %v", values)
				}
			}
			if values["provider"] == "p1" && values["key"] == "k1" {
				kept["key"] = true
			}
			if values["provider"] == "p1" && values["key"] == "" {
				kept["breaker"] = true
			}
			if values["account"] == "acct1" {
				kept["account"] = true
			}
		}
	}
	if !kept["key"] || !kept["breaker"] || !kept["account"] {
		t.Fatalf("pruning removed live series: %v", kept)
	}
}
