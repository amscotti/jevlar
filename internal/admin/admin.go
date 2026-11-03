package admin

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/amscotti/jevlar/internal/config"
	"github.com/amscotti/jevlar/internal/health"
	"github.com/amscotti/jevlar/internal/runtime"
	"github.com/amscotti/jevlar/internal/usage"
)

// Options configures New.
type Options struct {
	// Version is the binary version reported in /admin/status.
	Version string
	// Token protects the admin plane when non-empty; requests must
	// present it as a bearer token. Empty means loopback-only access.
	Token string
	// Runtime returns the currently serving snapshot; it is read once
	// per request so a reload between two requests is always observed.
	Runtime func() *runtime.Runtime
	// Reload runs the reload pipeline and returns the new configuration
	// hash; on error the previous snapshot must still be serving.
	Reload func() (string, error)
	// Usage returns the current attribution totals, or nil when usage
	// accounting is not wired.
	Usage func() (map[usage.Key]usage.Counts, int64)
	// UsageHistory sums durable rollups for a window, or nil when durable
	// rollups are not enabled.
	UsageHistory func(since time.Duration) (usage.Rollup, error)
}

// Admin serves the operator plane. Create one with New and mount its
// Handler on the admin listener.
type Admin struct {
	version   string
	tokenHash [sha256.Size]byte
	hasToken  bool
	runtime   func() *runtime.Runtime
	reload    func() (string, error)
	usage     func() (map[usage.Key]usage.Counts, int64)
	history   func(since time.Duration) (usage.Rollup, error)
}

// New builds an Admin from opts.
func New(opts Options) *Admin {
	return &Admin{
		version:   opts.Version,
		tokenHash: sha256.Sum256([]byte(opts.Token)),
		hasToken:  opts.Token != "",
		runtime:   opts.Runtime,
		reload:    opts.Reload,
		usage:     opts.Usage,
		history:   opts.UsageHistory,
	}
}

// Handler returns the admin routes: /admin/status and /admin/reload,
// wrapped in the access guard. Unknown admin paths answer 404 through the
// same guard.
func (a *Admin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/status", a.handleStatus)
	mux.HandleFunc("/admin/reload", a.handleReload)
	mux.HandleFunc("/admin/usage", a.handleUsage)
	return a.guard(mux)
}

// guard applies the access policy: bearer-token authentication when a
// token is configured, otherwise a loopback-only remote address, a
// loopback Host header and a same-origin-only browser check. Without the
// token, a malicious web page could otherwise drive the admin plane: DNS
// rebinding makes the Host header attacker-controlled, and a cross-site
// form POST is a CORS "simple request" the browser sends without a
// preflight.
func (a *Admin) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.hasToken {
			digest := sha256.Sum256([]byte(bearerToken(r)))
			if subtle.ConstantTimeCompare(digest[:], a.tokenHash[:]) != 1 {
				writeError(w, http.StatusUnauthorized, "admin requires a bearer token")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if !isLoopbackRequest(r) {
			writeError(w, http.StatusForbidden, "admin without a token is loopback-only")
			return
		}
		if !isLoopbackHost(r.Host) {
			writeError(w, http.StatusForbidden, "admin without a token requires a loopback Host header")
			return
		}
		if isCrossSiteRequest(r) {
			writeError(w, http.StatusForbidden, "cross-site admin requests are refused")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackHost reports whether a Host header value (host or host:port)
// names this machine.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}

// isCrossSiteRequest reports whether a browser marked the request as
// cross-site. Non-browser clients send neither header and are allowed;
// a browser sends Sec-Fetch-Site on every request and Origin on
// cross-origin non-GET requests.
func isCrossSiteRequest(r *http.Request) bool {
	switch site := r.Header.Get("Sec-Fetch-Site"); site {
	case "", "same-origin", "none":
	default:
		return true
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !isLoopbackHost(u.Host) {
			return true
		}
	}
	return false
}

// bearerToken extracts the bearer token from the Authorization header;
// empty when the header is absent or not a bearer credential. The scheme
// is matched case-insensitively per RFC 7235.
func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	const schemeLen = len("Bearer ")
	if len(auth) <= schemeLen || !strings.EqualFold(auth[:schemeLen], "Bearer ") {
		return ""
	}
	return auth[schemeLen:]
}

// isLoopbackRequest reports whether the request's remote address is a
// loopback host. The port is split off first; an unparseable address is
// treated as non-loopback (fail closed).
func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}

// StatusResponse is the documented /admin/status payload; see the package
// documentation for the shape and its redaction rules.
type StatusResponse struct {
	Version       string                 `json:"version"`
	UptimeSeconds int64                  `json:"uptime_seconds"`
	ConfigHash    string                 `json:"config_hash"`
	Providers     []ProviderStatus       `json:"providers"`
	Models        map[string][]TargetRef `json:"models"`
	Auth          AuthStatus             `json:"auth"`
	Breaker       BreakerStatus          `json:"breaker"`
	Keys          []health.KeyStatus     `json:"keys"`
	Breakers      []health.BreakerStatus `json:"breakers"`
	Accounts      []health.AccountStatus `json:"accounts"`
	Clients       []health.ClientStatus  `json:"clients"`
}

// ProviderStatus summarises one provider without key material.
type ProviderStatus struct {
	Name           string   `json:"name"`
	BaseURL        string   `json:"base_url"`
	Path           string   `json:"path"`
	Auth           string   `json:"auth"`
	Account        string   `json:"account"`
	Models         []string `json:"models"`
	TimeoutMs      int64    `json:"timeout_ms"`
	MaxConcurrency int      `json:"max_concurrency"`
	KeyCount       int      `json:"key_count"`
	KeyIDs         []string `json:"key_ids"`
}

// TargetRef names one routing target.
type TargetRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// AuthStatus lists the configured inbound client key ids.
type AuthStatus struct {
	KeyIDs []string `json:"key_ids"`
}

// BreakerStatus reports the circuit-breaker configuration.
type BreakerStatus struct {
	Threshold  int `json:"threshold"`
	CooldownMs int `json:"cooldown_ms"`
}

// handleStatus renders the serving snapshot's summary.
func (a *Admin) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET, HEAD")
		return
	}
	rt := a.runtime()
	if rt == nil {
		writeError(w, http.StatusServiceUnavailable, "no runtime snapshot")
		return
	}
	resp := StatusResponse{
		Version:       a.version,
		UptimeSeconds: int64(time.Since(rt.StartedAt) / time.Second),
		ConfigHash:    rt.ConfigHash,
		Models:        make(map[string][]TargetRef, len(rt.Models)),
		Auth:          AuthStatus{KeyIDs: make([]string, 0, len(rt.Config.Auth.Keys))},
	}
	for _, p := range rt.Config.Providers {
		view := rt.Providers[p.Name]
		if view == nil {
			continue
		}
		keyIDs := make([]string, 0, len(view.Keys))
		for _, k := range view.Keys {
			keyIDs = append(keyIDs, k.ID)
		}
		models := append([]string(nil), p.Models...)
		if models == nil {
			models = []string{}
		}
		resp.Providers = append(resp.Providers, ProviderStatus{
			Name:           view.Name,
			BaseURL:        view.BaseURL.String(),
			Path:           view.Path,
			Auth:           view.Auth,
			Account:        view.Account,
			Models:         models,
			TimeoutMs:      view.Timeout.Milliseconds(),
			MaxConcurrency: view.MaxConc,
			KeyCount:       len(view.Keys),
			KeyIDs:         keyIDs,
		})
	}
	if resp.Providers == nil {
		resp.Providers = []ProviderStatus{}
	}
	resp.Keys = []health.KeyStatus{}
	resp.Breakers = []health.BreakerStatus{}
	resp.Accounts = []health.AccountStatus{}
	resp.Clients = []health.ClientStatus{}
	if reg := rt.Health; reg != nil {
		for _, p := range rt.Config.Providers {
			if view := rt.Providers[p.Name]; view != nil && view.Pool != nil {
				resp.Keys = append(resp.Keys, view.Pool.Status()...)
			}
			if b := reg.Breaker(p.Name); b != nil {
				resp.Breakers = append(resp.Breakers, b.Status(p.Name))
			}
		}
		for _, name := range sortedKeys(rt.Config.Accounts) {
			if acc := reg.Account(name); acc != nil {
				resp.Accounts = append(resp.Accounts, acc.Status())
			}
		}
		for _, k := range rt.Config.Auth.Keys {
			if c := reg.Client(k.ID); c != nil {
				resp.Clients = append(resp.Clients, c.Status())
			}
		}
	}
	for alias, targets := range rt.Models {
		refs := make([]TargetRef, 0, len(targets))
		for _, t := range targets {
			refs = append(refs, TargetRef{Provider: t.Provider, Model: t.Model})
		}
		resp.Models[alias] = refs
	}
	for _, k := range rt.Config.Auth.Keys {
		resp.Auth.KeyIDs = append(resp.Auth.KeyIDs, k.ID)
	}
	resp.Breaker = BreakerStatus{
		Threshold:  rt.Config.Breaker.Threshold,
		CooldownMs: rt.Config.Breaker.CooldownMs,
	}
	writeJSON(w, http.StatusOK, resp)
}

// UsageResponse is the documented /admin/usage payload: totals per
// client, provider and key, plus the count of responses that carried no
// usage block.
type UsageResponse struct {
	Client   Totals `json:"client"`
	Provider Totals `json:"provider"`
	Key      Totals `json:"key"`
	Model    Totals `json:"model"`
	Misses   int64  `json:"usage_missing"`
}

// Totals is one rollup dimension.
type Totals struct {
	Requests     int64              `json:"requests"`
	InputTokens  int64              `json:"input_tokens"`
	OutputTokens int64              `json:"output_tokens"`
	Cost         float64            `json:"cost"`
	By           map[string]Summary `json:"by"`
}

// Summary is one entry inside a rollup dimension.
type Summary struct {
	Requests     int64   `json:"requests"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	Cost         float64 `json:"cost"`
}

// handleUsage rolls the attribution table up by client, provider and key.
func (a *Admin) handleUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET, HEAD")
		return
	}
	// A window query reads the durable rollups; the default reads the live
	// in-memory totals since boot.
	if windowParam := r.URL.Query().Get("window"); windowParam != "" {
		a.handleUsageWindow(w, windowParam)
		return
	}
	if a.usage == nil {
		writeError(w, http.StatusServiceUnavailable, "usage accounting is not enabled")
		return
	}
	rows, misses := a.usage()
	resp := UsageResponse{
		Client: newTotals(), Provider: newTotals(), Key: newTotals(), Model: newTotals(),
		Misses: misses,
	}
	for k, c := range rows {
		resp.Client.add(k.Client, c)
		resp.Provider.add(k.Provider, c)
		resp.Model.add(k.Model, c)
		if k.APIKey != "" {
			resp.Key.add(k.Provider+"/"+k.APIKey, c)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleUsageWindow renders a durable-rollup window.
func (a *Admin) handleUsageWindow(w http.ResponseWriter, windowParam string) {
	if a.history == nil {
		writeError(w, http.StatusServiceUnavailable,
			"durable usage rollups are not enabled; set usage.enabled = true")
		return
	}
	window, err := time.ParseDuration(windowParam)
	if err != nil || window <= 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("window must be a positive duration, got %q", windowParam))
		return
	}
	if window > maxUsageWindow {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("window must be at most %s", maxUsageWindow))
		return
	}
	roll, err := a.history(window)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := UsageResponse{
		Client: newTotals(), Provider: newTotals(), Key: newTotals(), Model: newTotals(),
		Misses: roll.UsageMissing,
	}
	for _, row := range roll.Rows {
		c := usage.Counts{
			Requests: row.Requests, InputTokens: row.InputTokens,
			OutputTokens: row.OutputTokens, Cost: row.Cost,
		}
		resp.Client.add(row.Client, c)
		resp.Provider.add(row.Provider, c)
		resp.Model.add(row.Model, c)
		if row.APIKey != "" {
			resp.Key.add(row.Provider+"/"+row.APIKey, c)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// maxUsageWindow bounds a rollup read so one request cannot scan years of
// files.
const maxUsageWindow = 90 * 24 * time.Hour

func newTotals() Totals { return Totals{By: map[string]Summary{}} }

func (t *Totals) add(name string, c usage.Counts) {
	t.Requests += c.Requests
	t.InputTokens += c.InputTokens
	t.OutputTokens += c.OutputTokens
	t.Cost += c.Cost
	s := t.By[name]
	s.Requests += c.Requests
	s.InputTokens += c.InputTokens
	s.OutputTokens += c.OutputTokens
	s.Cost += c.Cost
	t.By[name] = s
}

// handleReload runs the reload pipeline and renders its result.
func (a *Admin) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	hash, err := a.reload()
	if err != nil {
		resp := map[string]any{"status": "error"}
		var verrs config.ValidationErrors
		if errors.As(err, &verrs) {
			errs := make([]map[string]string, 0, len(verrs))
			for _, ce := range verrs {
				errs = append(errs, map[string]string{"field": ce.Field, "message": ce.Msg})
			}
			resp["errors"] = errs
		} else {
			resp["errors"] = []map[string]string{{"field": "", "message": err.Error()}}
		}
		writeJSON(w, http.StatusInternalServerError, resp)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "config_hash": hash})
}

// sortedKeys returns a map's keys in sorted order, so status output is
// deterministic across requests.
func sortedKeys[M ~map[string]V, V any](m M) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// methodNotAllowed answers 405 with the Allow header RFC 9110 requires.
func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "method not allowed; use "+allow)
}

// writeJSON renders v as a JSON response body.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

// writeError renders the operator-plane error envelope, the same shape
// a failed reload reports.
func writeError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]any{
		"status": "error",
		"errors": []map[string]string{{"field": "", "message": message}},
	})
}
