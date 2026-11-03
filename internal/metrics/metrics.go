// Package metrics owns Jevlar's Prometheus registry and metric definitions.
//
// The registry is private to this package — never client_golang's global
// registry — so no library import can silently register metrics into
// Jevlar's /metrics output. Packages that need a metric ask this package to
// define and register it on the private registry; the package imports no
// other Jevlar package.
package metrics

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics owns Jevlar's private Prometheus registry and the handlers that
// expose it. Create exactly one per process at boot.
type Metrics struct {
	registry *prometheus.Registry

	configReloadFailures prometheus.Counter

	requests      *prometheus.CounterVec
	requestMillis *prometheus.HistogramVec
	attempts      *prometheus.CounterVec
	upstreamError *prometheus.CounterVec
	tooLarge      prometheus.Counter
	inflight      prometheus.Gauge
	keyState      *prometheus.GaugeVec
	breakerState  *prometheus.GaugeVec
	keySuspect    prometheus.Counter
	accountCool   *prometheus.GaugeVec
	inputTokens   *prometheus.CounterVec
	outputTokens  *prometheus.CounterVec
	cost          *prometheus.CounterVec
	usageMissing  prometheus.Counter
	clientLimited *prometheus.CounterVec

	// mu guards the series indexes below; they let a reload delete gauge
	// series for identities the new configuration no longer contains.
	mu            sync.Mutex
	keySeries     map[[2]string]struct{}
	breakerSeries map[string]struct{}
	accountSeries map[string]struct{}
}

// Options configures New.
type Options struct {
	// Version is the gateway version reported by jevlar_build_info; the
	// cmd injects it at build time.
	Version string
	// GoVersion is the Go toolchain version reported by jevlar_build_info.
	GoVersion string
}

// New creates a Metrics with a private registry and registers
// jevlar_build_info (a constant-1 gauge carrying the "version" and
// "go_version" labels) and jevlar_config_reload_failures_total (a counter
// of failed configuration reloads). Registration cannot fail at runtime
// short of programmer error, so construction never returns an error.
func New(opts Options) *Metrics {
	registry := prometheus.NewRegistry()
	buildInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "jevlar_build_info",
			Help: "Build information for the Jevlar gateway; the value is always 1.",
		},
		[]string{"version", "go_version"},
	)
	buildInfo.WithLabelValues(opts.Version, opts.GoVersion).Set(1)
	registry.MustRegister(buildInfo)
	reloadFailures := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "jevlar_config_reload_failures_total",
		Help: "Total number of failed configuration reloads (SIGHUP or /admin/reload); the previous runtime keeps serving.",
	})
	registry.MustRegister(reloadFailures)

	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jevlar_requests_total",
		Help: "Total client requests handled, by client, provider, resolved model alias and status class.",
	}, []string{"client", "provider", "model", "status"})
	requestMillis := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "jevlar_request_duration_milliseconds",
		Help:    "End-to-end Jevlar handling time in milliseconds, by provider and model alias.",
		Buckets: []float64{1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000},
	}, []string{"provider", "model"})
	attempts := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jevlar_attempts_total",
		Help: "Upstream attempts, by provider and outcome (success, retryable, passthrough, error).",
	}, []string{"provider", "result"})
	upstreamError := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jevlar_upstream_errors_total",
		Help: "Upstream failures, by provider and reason (transport, timeout, too_large, status).",
	}, []string{"provider", "reason"})
	tooLarge := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "jevlar_response_too_large_total",
		Help: "Upstream responses rejected because they exceeded server.max_response_bytes.",
	})
	inflight := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "jevlar_requests_inflight",
		Help: "Requests currently admitted by Jevlar, bounded by server.max_inflight.",
	})
	keyState := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "jevlar_key_state",
		Help: "Provider key state: 0 healthy, 1 cooldown, 2 disabled.",
	}, []string{"provider", "key"})
	breakerState := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "jevlar_breaker_state",
		Help: "Provider circuit breaker state: 0 closed, 1 half-open, 2 open.",
	}, []string{"provider"})
	keySuspect := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "jevlar_key_suspect_total",
		Help: "Upstream 403 responses; the key was cooled but not disabled, because 403 also means quota or plan limits.",
	})
	accountCool := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "jevlar_account_cooldown_seconds",
		Help: "Seconds remaining on an account-wide cooldown; 0 when the account is not cooling.",
	}, []string{"account"})
	inputTokens := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jevlar_input_tokens_total",
		Help: "Upstream-reported input tokens, by client, provider and model.",
	}, []string{"client", "provider", "model"})
	outputTokens := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jevlar_output_tokens_total",
		Help: "Upstream-reported output tokens, by client, provider and model.",
	}, []string{"client", "provider", "model"})
	cost := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jevlar_cost_total",
		Help: "Upstream-reported cost in the provider's currency, by client, provider and model.",
	}, []string{"client", "provider", "model"})
	usageMissing := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "jevlar_usage_missing_total",
		Help: "Successful responses that carried no usage block; tokens are never guessed.",
	})
	clientLimited := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jevlar_client_limited_total",
		Help: "Requests refused because the client was over its rpm or tpm budget.",
	}, []string{"client", "reason"})
	registry.MustRegister(requests, requestMillis, attempts, upstreamError, tooLarge, inflight,
		keyState, breakerState, keySuspect, accountCool,
		inputTokens, outputTokens, cost, usageMissing, clientLimited)
	return &Metrics{
		registry:             registry,
		configReloadFailures: reloadFailures,
		requests:             requests,
		requestMillis:        requestMillis,
		attempts:             attempts,
		upstreamError:        upstreamError,
		tooLarge:             tooLarge,
		inflight:             inflight,
		keyState:             keyState,
		breakerState:         breakerState,
		keySuspect:           keySuspect,
		accountCool:          accountCool,
		inputTokens:          inputTokens,
		outputTokens:         outputTokens,
		cost:                 cost,
		usageMissing:         usageMissing,
		clientLimited:        clientLimited,
		keySeries:            map[[2]string]struct{}{},
		breakerSeries:        map[string]struct{}{},
		accountSeries:        map[string]struct{}{},
	}
}

// Handler returns an http.Handler serving the private registry in the
// Prometheus text exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Registry returns the private registry so tests and tooling can gather
// the exposition without going through HTTP.
func (m *Metrics) Registry() *prometheus.Registry {
	return m.registry
}

// ConfigReloadFailures returns the jevlar_config_reload_failures_total
// counter; internal/app increments it every time a reload (SIGHUP or
// /admin/reload) fails while the previous runtime keeps serving.
func (m *Metrics) ConfigReloadFailures() prometheus.Counter {
	return m.configReloadFailures
}

// Request records one handled request. status is the HTTP status class
// (for example "2xx", "429") rather than the exact code, keeping label
// cardinality bounded.
func (m *Metrics) Request(client, provider, model, status string) {
	m.requests.WithLabelValues(client, provider, model, status).Inc()
}

// RequestDuration records the end-to-end handling time of one request.
func (m *Metrics) RequestDuration(provider, model string, millis float64) {
	m.requestMillis.WithLabelValues(provider, model).Observe(millis)
}

// Attempt records one upstream attempt and its outcome.
func (m *Metrics) Attempt(provider, result string) {
	m.attempts.WithLabelValues(provider, result).Inc()
}

// UpstreamError records one upstream failure reason.
func (m *Metrics) UpstreamError(provider, reason string) {
	m.upstreamError.WithLabelValues(provider, reason).Inc()
}

// ResponseTooLarge records one over-cap upstream response.
func (m *Metrics) ResponseTooLarge() { m.tooLarge.Inc() }

// InflightAdd adjusts the admitted-request gauge.
func (m *Metrics) InflightAdd(delta float64) { m.inflight.Add(delta) }

// KeyState records a provider key's state (0 healthy, 1 cooldown,
// 2 disabled).
func (m *Metrics) KeyState(provider, key string, state float64) {
	m.mu.Lock()
	m.keySeries[[2]string{provider, key}] = struct{}{}
	m.mu.Unlock()
	m.keyState.WithLabelValues(provider, key).Set(state)
}

// BreakerState records a provider breaker's state (0 closed, 1 half-open,
// 2 open).
func (m *Metrics) BreakerState(provider string, state float64) {
	m.mu.Lock()
	m.breakerSeries[provider] = struct{}{}
	m.mu.Unlock()
	m.breakerState.WithLabelValues(provider).Set(state)
}

// KeySuspect records an upstream 403.
func (m *Metrics) KeySuspect() { m.keySuspect.Inc() }

// Usage records one response's reported tokens and cost.
func (m *Metrics) Usage(client, provider, model string, input, output int64, cost float64) {
	m.inputTokens.WithLabelValues(client, provider, model).Add(float64(input))
	m.outputTokens.WithLabelValues(client, provider, model).Add(float64(output))
	if cost > 0 {
		m.cost.WithLabelValues(client, provider, model).Add(cost)
	}
}

// UsageMissing records a successful response with no usage block.
func (m *Metrics) UsageMissing() { m.usageMissing.Inc() }

// ClientLimited records a request refused for a client budget, with reason
// "rpm" or "tpm".
func (m *Metrics) ClientLimited(client, reason string) {
	m.clientLimited.WithLabelValues(client, reason).Inc()
}

// AccountCooldown records the seconds remaining on an account cooldown.
func (m *Metrics) AccountCooldown(account string, seconds float64) {
	m.mu.Lock()
	m.accountSeries[account] = struct{}{}
	m.mu.Unlock()
	m.accountCool.WithLabelValues(account).Set(seconds)
}

// PruneIdentities removes gauge series for provider keys, providers and
// accounts that the current configuration no longer contains. Counters are
// process-lifetime totals and are deliberately kept. Call it after a
// successful reload, once the new snapshot's gauges are published.
func (m *Metrics) PruneIdentities(keysByProvider map[string]map[string]bool, providers, accounts []string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	keepProviders := make(map[string]bool, len(providers))
	for _, p := range providers {
		keepProviders[p] = true
	}
	for p := range m.breakerSeries {
		if keepProviders[p] {
			continue
		}
		m.breakerState.DeleteLabelValues(p)
		delete(m.breakerSeries, p)
	}
	for series := range m.keySeries {
		provider, id := series[0], series[1]
		if ids := keysByProvider[provider]; ids != nil && ids[id] {
			continue
		}
		m.keyState.DeleteLabelValues(provider, id)
		delete(m.keySeries, series)
	}
	keepAccounts := make(map[string]bool, len(accounts))
	for _, a := range accounts {
		keepAccounts[a] = true
	}
	for a := range m.accountSeries {
		if keepAccounts[a] {
			continue
		}
		m.accountCool.DeleteLabelValues(a)
		delete(m.accountSeries, a)
	}
}
