package admin_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/amscotti/jevlar/internal/health"
	"github.com/amscotti/jevlar/internal/testutil"

	"github.com/amscotti/jevlar/internal/admin"
	"github.com/amscotti/jevlar/internal/config"
	"github.com/amscotti/jevlar/internal/runtime"
	"github.com/amscotti/jevlar/internal/usage"
)

// canaryOutboundKey and canaryToken are secrets the redaction test hunts
// for in response bodies; their presence anywhere is a failure.
const (
	canaryOutboundKey = "CANARY-outbound-key-value-9f1e"
	canaryToken       = "canary-inbound-client-token-42"
)

// canaryHash is the SHA-256 hex of canaryToken, as `jevlar keyhash`
// would configure it.
func canaryHash() string {
	sum := sha256.Sum256([]byte(canaryToken))
	return hex.EncodeToString(sum[:])
}

// statusDoc builds a complete valid configuration whose provider key is
// the canary literal value and whose inbound hash is the canary token's.
func statusDoc() string {
	return `
config_version = 1

[server]
listen = "127.0.0.1:18080"

[auth]
keys = [{ id = "app-prod", hash = "` + canaryHash() + `", rpm = 600, tpm = 6000000 }]

[[providers]]
name = "typesafe"
base_url = "https://api.typesafe.ai"
path = "/v1/systemone"
auth = "bearer"
account = "main"
models = ["jev-latest"]
timeout_ms = 8000
max_concurrency = 64
api_keys = [{ id = "primary", value = "` + canaryOutboundKey + `" }]

[accounts.main]
rpm = 1200
tpm = 15000000

[models."jev-latest"]
targets = [{ provider = "typesafe", model = "jev-latest" }]
policy = "priority"
max_attempts = 3

[breaker]
threshold = 5
cooldown_ms = 30000
`
}

// buildSnapshot parses doc and builds a runtime snapshot from it.
func buildSnapshot(t *testing.T, doc string) *runtime.Runtime {
	t.Helper()
	cfg, err := config.Parse([]byte(doc), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	rt, err := runtime.Build(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return rt
}

// newAdmin wires an Admin over a snapshot (the default reload fails).
func newAdmin(t *testing.T, opts admin.Options, doc func() string) *admin.Admin {
	t.Helper()
	if opts.Runtime == nil {
		opts.Runtime = func() *runtime.Runtime { return buildSnapshot(t, doc()) }
	}
	if opts.Reload == nil {
		opts.Reload = func() (string, error) {
			return "", errors.New("no reload wired")
		}
	}
	return admin.New(opts)
}

// call serves one request through the handler with the given remote
// address, token header and method, and returns the recorded response.
func call(t *testing.T, a *admin.Admin, method, path, remoteAddr, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Host = "127.0.0.1:8081"
	req.RemoteAddr = remoteAddr
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	return rec
}

func TestStatus_ReturnsDocumentedShape(t *testing.T) {
	t.Parallel()
	a := newAdmin(t, admin.Options{Version: "v0.1.0-test"}, statusDoc)
	rec := call(t, a, http.MethodGet, "/admin/status", "127.0.0.1:55555", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Version       string `json:"version"`
		UptimeSeconds int64  `json:"uptime_seconds"`
		ConfigHash    string `json:"config_hash"`
		Providers     []struct {
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
		} `json:"providers"`
		Models map[string][]struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"models"`
		Auth struct {
			KeyIDs []string `json:"key_ids"`
		} `json:"auth"`
		Breaker struct {
			Threshold  int `json:"threshold"`
			CooldownMs int `json:"cooldown_ms"`
		} `json:"breaker"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not the documented JSON shape: %v\n%s", err, rec.Body.String())
	}
	if body.Version != "v0.1.0-test" {
		t.Errorf("version = %q", body.Version)
	}
	if body.UptimeSeconds < 0 {
		t.Errorf("uptime = %d", body.UptimeSeconds)
	}
	if len(body.ConfigHash) != 64 {
		t.Errorf("config_hash = %q, want 64 hex chars", body.ConfigHash)
	}
	if len(body.Providers) != 1 {
		t.Fatalf("providers = %+v", body.Providers)
	}
	p := body.Providers[0]
	if p.Name != "typesafe" || p.BaseURL != "https://api.typesafe.ai" || p.Path != "/v1/systemone" ||
		p.Auth != "bearer" || p.Account != "main" || p.TimeoutMs != 8000 || p.MaxConcurrency != 64 ||
		p.KeyCount != 1 || len(p.KeyIDs) != 1 || p.KeyIDs[0] != "primary" {
		t.Errorf("provider summary = %+v", p)
	}
	if len(p.Models) != 1 || p.Models[0] != "jev-latest" {
		t.Errorf("provider models = %v", p.Models)
	}
	targets, ok := body.Models["jev-latest"]
	if !ok || len(targets) != 1 || targets[0].Provider != "typesafe" || targets[0].Model != "jev-latest" {
		t.Errorf("models = %+v", body.Models)
	}
	if len(body.Auth.KeyIDs) != 1 || body.Auth.KeyIDs[0] != "app-prod" {
		t.Errorf("auth = %+v", body.Auth)
	}
	if body.Breaker.Threshold != 5 || body.Breaker.CooldownMs != 30000 {
		t.Errorf("breaker = %+v", body.Breaker)
	}
}

// TestStatus_NeverLeaksKeyMaterial: the canary outbound key value, the
// canary inbound token and even its configured hash must be absent from
// the response body.
func TestStatus_NeverLeaksKeyMaterial(t *testing.T) {
	t.Parallel()
	a := newAdmin(t, admin.Options{Version: "t"}, statusDoc)
	rec := call(t, a, http.MethodGet, "/admin/status", "127.0.0.1:55555", "")
	body := rec.Body.String()
	for _, secret := range []string{canaryOutboundKey, canaryToken, canaryHash()} {
		if strings.Contains(body, secret) {
			t.Fatalf("status body leaks %q:\n%s", secret, body)
		}
	}
}

// TestAccess_LoopbackGuardWithoutToken: with no token configured,
// loopback remotes are served and non-loopback remotes get 403 — the
// guard is enforced in the handler, not only in configuration.
func TestAccess_LoopbackGuardWithoutToken(t *testing.T) {
	t.Parallel()
	a := newAdmin(t, admin.Options{Version: "t"}, statusDoc)
	for _, loopback := range []string{"127.0.0.1:1", "[::1]:1", "[::ffff:127.0.0.1]:1"} {
		if rec := call(t, a, http.MethodGet, "/admin/status", loopback, ""); rec.Code != http.StatusOK {
			t.Errorf("remote %q: status = %d, want 200", loopback, rec.Code)
		}
	}
	for _, other := range []string{"192.0.2.1:1", "10.0.0.1:1", "[2001:db8::1]:1", "example.com:1", "garbage"} {
		if rec := call(t, a, http.MethodGet, "/admin/status", other, ""); rec.Code != http.StatusForbidden {
			t.Errorf("remote %q: status = %d, want 403", other, rec.Code)
		}
	}
}

// TestAccess_BearerTokenWhenConfigured: with a token configured the
// loopback restriction is lifted and the token decides: missing or wrong
// tokens get 401, the right token is served from any address.
func TestAccess_BearerTokenWhenConfigured(t *testing.T) {
	t.Parallel()
	token := "0123456789abcdef0123456789abcdef" // 32 chars
	a := newAdmin(t, admin.Options{Version: "t", Token: token}, statusDoc)

	if rec := call(t, a, http.MethodGet, "/admin/status", "192.0.2.1:1", token); rec.Code != http.StatusOK {
		t.Errorf("correct token from non-loopback: status = %d, want 200", rec.Code)
	}
	if rec := call(t, a, http.MethodGet, "/admin/status", "192.0.2.1:1", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("missing token: status = %d, want 401", rec.Code)
	}
	if rec := call(t, a, http.MethodGet, "/admin/status", "192.0.2.1:1", "wrong-token-wrong-token-wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", rec.Code)
	}
	// A non-bearer Authorization header is not a token presentation.
	req := httptest.NewRequest(http.MethodGet, "/admin/status", nil)
	req.Host = "127.0.0.1:8081"
	req.RemoteAddr = "192.0.2.1:1"
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("basic auth header: status = %d, want 401", rec.Code)
	}
	// The guard applies to reload too.
	if rec := call(t, a, http.MethodPost, "/admin/reload", "10.0.0.1:1", token); rec.Code == http.StatusForbidden {
		t.Error("token-configured admin must not enforce loopback")
	}
}

// TestReload_SuccessAndFailureRendering: a successful reload answers 200
// with the new hash; a failing one answers 500 with one entry per
// configuration violation, each carrying its field path.
func TestReload_SuccessAndFailureRendering(t *testing.T) {
	t.Parallel()
	newHash := strings.Repeat("ab", 32)
	a := admin.New(admin.Options{
		Version: "t",
		Runtime: func() *runtime.Runtime { return buildSnapshot(t, statusDoc()) },
		Reload: func() (string, error) {
			return newHash, nil
		},
	})
	rec := call(t, a, http.MethodPost, "/admin/reload", "127.0.0.1:1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("reload = %d, body %s", rec.Code, rec.Body.String())
	}
	var ok map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil || ok["status"] != "ok" || ok["config_hash"] != newHash {
		t.Fatalf("reload body = %s (%v)", rec.Body.String(), err)
	}

	bad := admin.New(admin.Options{
		Version: "t",
		Runtime: func() *runtime.Runtime { return buildSnapshot(t, statusDoc()) },
		Reload: func() (string, error) {
			return "", config.ValidationErrors{
				{Field: "server.listen", Msg: "must be a host:port (x)"},
				{Field: "breaker.threshold", Msg: "must be at least 1"},
			}
		},
	})
	rec = call(t, bad, http.MethodPost, "/admin/reload", "127.0.0.1:1", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed reload = %d, body %s", rec.Code, rec.Body.String())
	}
	var fail struct {
		Status string `json:"status"`
		Errors []struct {
			Field   string `json:"field"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fail); err != nil {
		t.Fatalf("failure body is not JSON: %v\n%s", err, rec.Body.String())
	}
	if fail.Status != "error" || len(fail.Errors) != 2 ||
		fail.Errors[0].Field != "server.listen" || fail.Errors[1].Field != "breaker.threshold" {
		t.Fatalf("failure body = %+v", fail)
	}
}

// TestMethods_AreEnforced: status is GET-only and reload is POST-only.
func TestMethods_AreEnforced(t *testing.T) {
	t.Parallel()
	a := newAdmin(t, admin.Options{Version: "t"}, statusDoc)
	if rec := call(t, a, http.MethodPost, "/admin/status", "127.0.0.1:1", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rec.Code)
	}
	if rec := call(t, a, http.MethodGet, "/admin/reload", "127.0.0.1:1", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET reload = %d, want 405", rec.Code)
	}
}

// TestUnknownAdminPathIsRejectedWithTheGuard: the guard wraps the whole
// handler, so even unknown paths are not reachable without access.
func TestUnknownAdminPathIsRejectedWithTheGuard(t *testing.T) {
	t.Parallel()
	a := newAdmin(t, admin.Options{Version: "t"}, statusDoc)
	rec := call(t, a, http.MethodGet, "/admin/other", "10.0.0.1:1", "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("unknown path from non-loopback = %d, want 403", rec.Code)
	}
	rec = call(t, a, http.MethodGet, "/admin/other", "127.0.0.1:1", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown path from loopback = %d, want 404", rec.Code)
	}
}

// TestStatusWithoutRuntimeIsUnavailable: a nil snapshot (never published)
// answers 503 rather than panicking.
func TestStatusWithoutRuntimeIsUnavailable(t *testing.T) {
	t.Parallel()
	a := admin.New(admin.Options{
		Version: "t",
		Runtime: func() *runtime.Runtime { return nil },
		Reload:  func() (string, error) { return "", errors.New("x") },
	})
	rec := call(t, a, http.MethodGet, "/admin/status", "127.0.0.1:1", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status without runtime = %d, want 503", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "no runtime snapshot") {
		t.Errorf("body = %s", body)
	}
}

// TestStatus_ReportsKeyBreakerAndAccountState pins the the routed stage status
// surface: key state, breaker state and account cooling are visible to an
// operator, and no secret material appears.
func TestStatus_ReportsKeyBreakerAndAccountState(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		ConfigVersion: 1,
		Server: config.Server{
			Listen: "127.0.0.1:0", RequestTimeoutMs: 1000, MaxBodyBytes: 1024,
			MaxResponseBytes: 1024, MaxInflight: 8, ShutdownGraceMs: 100,
			ReadHeaderTimeoutMs: 100, LogLevel: "error",
		},
		Auth:     config.Auth{Keys: []config.AuthKey{{ID: "app", Hash: strings.Repeat("a", 64), RPM: 60}}},
		Accounts: map[string]config.Account{"acct": {RPM: 100}},
		Providers: []config.Provider{{
			Name: "p", BaseURL: "http://127.0.0.1:9", Path: "/v1/systemone",
			Auth: "bearer", Account: "acct", Models: []string{"m"},
			TimeoutMs: 1000, MaxConcurrency: 2,
			APIKeys: []config.APIKey{{ID: "k1", Resolved: "super-secret-value"}},
		}},
		Breaker: config.Breaker{Threshold: 5, CooldownMs: 1000},
	}
	rt, err := runtime.Build(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	reg := health.New()
	t.Cleanup(testutil.WireWithHealth(rt, reg))

	key := reg.Key(health.KeyRef{Provider: "p", ID: "k1"})
	key.CoolFor(time.Minute)
	reg.Account("acct").CoolFor(time.Minute)
	b := reg.Breaker("p")
	for i := 0; i < 5; i++ {
		b.RecordFailure()
	}

	a := admin.New(admin.Options{Version: "test", Runtime: func() *runtime.Runtime { return rt }})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/status", nil)
	req.Host = "127.0.0.1:8081"
	req.RemoteAddr = "127.0.0.1:1234"
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	var got admin.StatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Keys) != 1 || got.Keys[0].ID != "k1" || got.Keys[0].State != health.StateCooldown {
		t.Fatalf("keys = %+v, want k1 in cooldown", got.Keys)
	}
	if got.Keys[0].CooldownUntil == "" {
		t.Fatal("cooling key has no cooldown_until")
	}
	if len(got.Breakers) != 1 || got.Breakers[0].State == health.StateHealthy {
		t.Fatalf("breakers = %+v, want an open breaker", got.Breakers)
	}
	if len(got.Accounts) != 1 || !got.Accounts[0].Cooling {
		t.Fatalf("accounts = %+v, want acct cooling", got.Accounts)
	}
	if len(got.Clients) != 1 || got.Clients[0].ID != "app" {
		t.Fatalf("clients = %+v", got.Clients)
	}
	if strings.Contains(rec.Body.String(), "super-secret-value") {
		t.Fatal("status leaked key material")
	}
}

// TestUsage_RollsUpPerDimension pins /admin/usage: totals by client,
// provider, key and model, plus the missing-usage count.
func TestUsage_RollsUpPerDimension(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	tr.Record(usage.Key{Client: "app", Provider: "typesafe", APIKey: "primary", Model: "jev-latest"},
		usage.Usage{InputTokens: 312, OutputTokens: 47, Cost: 0.5, HasCost: true})
	tr.Record(usage.Key{Client: "app", Provider: "ollama", APIKey: "", Model: "nimble"},
		usage.Usage{InputTokens: 10})
	tr.RecordMiss()

	a := admin.New(admin.Options{
		Version: "test",
		Runtime: func() *runtime.Runtime { return nil },
		Usage:   tr.Snapshot,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/usage", nil)
	req.Host = "127.0.0.1:8081"
	req.RemoteAddr = "127.0.0.1:1234"
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var got admin.UsageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Client.Requests != 2 || got.Client.InputTokens != 322 || got.Client.OutputTokens != 47 {
		t.Fatalf("client totals = %+v", got.Client)
	}
	if got.Provider.By["typesafe"].InputTokens != 312 {
		t.Fatalf("provider rollup = %+v", got.Provider.By)
	}
	if got.Key.By["typesafe/primary"].Requests != 1 {
		t.Fatalf("key rollup = %+v", got.Key.By)
	}
	if got.Model.By["nimble"].InputTokens != 10 {
		t.Fatalf("model rollup = %+v", got.Model.By)
	}
	if got.Misses != 1 {
		t.Fatalf("misses = %d", got.Misses)
	}
}

// TestUsage_UnwiredIs503 pins the fail-closed admin surface.
func TestUsage_UnwiredIs503(t *testing.T) {
	t.Parallel()
	a := admin.New(admin.Options{Version: "test", Runtime: func() *runtime.Runtime { return nil }})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/usage", nil)
	req.Host = "127.0.0.1:8081"
	req.RemoteAddr = "127.0.0.1:1234"
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestUsage_WindowReadsDurableRollups pins /admin/usage?window=: it reads
// the rollup history rather than the in-memory totals, and rejects a bad
// or oversized window.
func TestUsage_WindowReadsDurableRollups(t *testing.T) {
	t.Parallel()
	var called time.Duration
	roll := usage.Rollup{
		Rows: []usage.RollupRow{
			{
				Client: "app", Provider: "typesafe", APIKey: "primary", Model: "jev-latest",
				Requests: 3, InputTokens: 300, OutputTokens: 30, Cost: 1.5,
			},
			{Client: "batch", Provider: "ollama", Model: "nimble", Requests: 1, InputTokens: 10},
		},
		UsageMissing: 2,
	}
	a := admin.New(admin.Options{
		Version: "test",
		Runtime: func() *runtime.Runtime { return nil },
		UsageHistory: func(since time.Duration) (usage.Rollup, error) {
			called = since
			return roll, nil
		},
	})

	fetch := func(query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/admin/usage"+query, nil)
		req.Host = "127.0.0.1:8081"
		req.RemoteAddr = "127.0.0.1:1234"
		a.Handler().ServeHTTP(rec, req)
		return rec
	}

	rec := fetch("?window=24h")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if called != 24*time.Hour {
		t.Fatalf("history called with %v", called)
	}
	var got admin.UsageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Client.Requests != 4 || got.Client.InputTokens != 310 || got.Client.Cost != 1.5 {
		t.Fatalf("client totals = %+v", got.Client)
	}
	if got.Key.By["typesafe/primary"].Requests != 3 {
		t.Fatalf("key rollup = %+v", got.Key.By)
	}
	if got.Misses != 2 {
		t.Fatalf("misses = %d", got.Misses)
	}

	for _, bad := range []string{"?window=nope", "?window=-1h", "?window=10000h"} {
		if rec := fetch(bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("window %q status = %d, want 400", bad, rec.Code)
		}
	}
}

// TestUsage_WindowWithoutRollupsIs503 pins the fail-closed path when
// durable rollups are disabled.
func TestUsage_WindowWithoutRollupsIs503(t *testing.T) {
	t.Parallel()
	a := admin.New(admin.Options{Version: "test", Runtime: func() *runtime.Runtime { return nil }})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/usage?window=24h", nil)
	req.Host = "127.0.0.1:8081"
	req.RemoteAddr = "127.0.0.1:1234"
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestAccess_TokenlessRejectsRebindingAndCrossSite pins the browser-facing
// defenses: a rebinding Host or a cross-site marker must be refused even
// though the TCP peer is loopback.
func TestAccess_TokenlessRejectsRebindingAndCrossSite(t *testing.T) {
	t.Parallel()
	a := newAdmin(t, admin.Options{}, statusDoc)
	cases := []struct {
		name   string
		host   string
		header map[string]string
		want   int
	}{
		{name: "loopback host", host: "127.0.0.1:8081", want: http.StatusOK},
		{name: "localhost host", host: "localhost:8081", want: http.StatusOK},
		{name: "rebound host", host: "attacker.example:8081", want: http.StatusForbidden},
		{
			name: "cross-site marker", host: "127.0.0.1:8081",
			header: map[string]string{"Sec-Fetch-Site": "cross-site"}, want: http.StatusForbidden,
		},
		{
			name: "cross-origin header", host: "127.0.0.1:8081",
			header: map[string]string{"Origin": "https://attacker.example"}, want: http.StatusForbidden,
		},
		{
			name: "same-origin browser", host: "127.0.0.1:8081",
			header: map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://127.0.0.1:8081"},
			want:   http.StatusOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/admin/status", nil)
			req.Host = tc.host
			req.RemoteAddr = "127.0.0.1:1234"
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			a.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
