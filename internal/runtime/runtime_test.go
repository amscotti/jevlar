package runtime_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amscotti/jevlar/internal/config"
	"github.com/amscotti/jevlar/internal/runtime"
)

// baseDoc is a complete valid configuration with two providers, an
// account, inbound keys and one model alias.
const baseDoc = `
config_version = 1

[server]
listen = "127.0.0.1:18080"
admin_listen = "127.0.0.1:18081"

[auth]
keys = [{ id = "app-prod", hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", rpm = 600, tpm = 6000000 }]

[accounts."typesafe-main"]
rpm = 1200
tpm = 15000000

[[providers]]
name = "typesafe"
base_url = "https://api.typesafe.ai"
path = "/v1/systemone"
auth = "bearer"
account = "typesafe-main"
models = ["jev-latest"]
timeout_ms = 8000
max_concurrency = 64
api_keys = [{ id = "primary", value = "${TYPESAFE_API_KEY}" }]

[[providers]]
name = "loopback"
base_url = "http://127.0.0.1:9"
path = "/v1/systemone"
auth = "none"

[models."jev-latest"]
targets = [
  { provider = "typesafe", model = "jev-latest" },
  { provider = "loopback", model = "nimble" },
]
policy = "priority"
max_attempts = 3

[breaker]
threshold = 5
cooldown_ms = 30000
`

// build parses doc, validates it and builds a Runtime.
func build(t *testing.T, doc string) (*runtime.Runtime, *config.Config) {
	t.Helper()
	cfg, err := config.Parse([]byte(doc), fakeEnv())
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
	return rt, cfg
}

// fakeEnv is the environment baseDoc needs.
func fakeEnv() config.Lookup {
	return func(name string) (string, bool) {
		vars := map[string]string{"TYPESAFE_API_KEY": "ts-secret"}
		v, ok := vars[name]
		return v, ok
	}
}

// TestBuild_ProducesProviderViewsAndModelTable: names map to views, URLs
// are parsed, timeouts converted, keys resolved, and the alias maps to
// its ordered targets.
func TestBuild_ProducesProviderViewsAndModelTable(t *testing.T) {
	t.Parallel()
	rt, _ := build(t, baseDoc)

	if len(rt.Providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(rt.Providers))
	}
	ts := rt.Providers["typesafe"]
	if ts == nil {
		t.Fatal("typesafe view missing")
	}
	if ts.BaseURL.String() != "https://api.typesafe.ai" {
		t.Errorf("base URL = %q", ts.BaseURL.String())
	}
	if ts.Timeout != 8*time.Second {
		t.Errorf("timeout = %v, want 8s", ts.Timeout)
	}
	if ts.MaxConc != 64 {
		t.Errorf("max concurrency = %d, want 64", ts.MaxConc)
	}
	if ts.Account != "typesafe-main" || ts.Auth != "bearer" || ts.Path != "/v1/systemone" {
		t.Errorf("provider view = %+v", ts)
	}
	if len(ts.Keys) != 1 || ts.Keys[0].ID != "primary" || ts.Keys[0].Value != "ts-secret" {
		t.Errorf("keys = %+v, want the resolved primary key", ts.Keys)
	}

	targets := rt.Models["jev-latest"]
	if len(targets) != 2 || targets[0] != (runtime.Target{Provider: "typesafe", Model: "jev-latest"}) || targets[1] != (runtime.Target{Provider: "loopback", Model: "nimble"}) {
		t.Errorf("targets = %+v, want the ordered pair", targets)
	}
	if rt.StartedAt.IsZero() {
		t.Error("StartedAt is zero")
	}
}

// TestBuild_BadBaseURLFailsClosed: Build never panics on a value that
// skipped validation; it fails with an anchored error.
func TestBuild_BadBaseURLFailsClosed(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte(baseDoc), fakeEnv())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg.Providers[0].BaseURL = "://not a url at all"
	if _, err := runtime.Build(cfg); err == nil {
		t.Fatal("Build accepted a malformed base_url, want an error")
	}
}

// TestConfigHash_IsStableAcrossCommentsAndFormatting: the hash covers the
// resolved configuration, not the file; added comments and whitespace
// changes leave it identical.
func TestConfigHash_IsStableAcrossCommentsAndFormatting(t *testing.T) {
	t.Parallel()
	base, _ := build(t, baseDoc)

	commented := strings.Replace(baseDoc, "config_version = 1", "# a comment about nothing\nconfig_version = 1 # trailing\n", 1)
	commented = strings.Replace(commented, "[breaker]", "\n# another comment\n[breaker]", 1)
	other, _ := build(t, commented)
	if base.ConfigHash != other.ConfigHash {
		t.Fatalf("comment changed the hash:\n%s\n%s", base.ConfigHash, other.ConfigHash)
	}
}

// TestConfigHash_ChangesWhenValuesChange: any resolved value change —
// timeouts, quotas, provider lists, model targets — changes the hash.
func TestConfigHash_ChangesWhenValuesChange(t *testing.T) {
	t.Parallel()
	base, _ := build(t, baseDoc)
	cases := map[string]string{
		"provider timeout":    strings.Replace(baseDoc, "timeout_ms = 8000", "timeout_ms = 9000", 1),
		"model max attempts":  strings.Replace(baseDoc, "max_attempts = 3", "max_attempts = 2", 1),
		"breaker threshold":   strings.Replace(baseDoc, "threshold = 5", "threshold = 7", 1),
		"auth key quota":      strings.Replace(baseDoc, "rpm = 600", "rpm = 601", 1),
		"account quota":       strings.Replace(baseDoc, "rpm = 1200", "rpm = 1201", 1),
		"removed account ref": strings.Replace(baseDoc, "account = \"typesafe-main\"", "account = \"\"", 1),
		"routing policy":      strings.Replace(baseDoc, "policy = \"priority\"", "policy = \"fastest\"", 1),
		"log level":           strings.Replace(baseDoc, "listen = \"127.0.0.1:18080\"", "listen = \"127.0.0.1:18080\"\nlog_level = \"warn\"", 1),
		"breaker cooldown":    strings.Replace(baseDoc, "cooldown_ms = 30000", "cooldown_ms = 45000", 1),
	}
	for name, doc := range cases {
		other, _ := build(t, doc)
		if base.ConfigHash == other.ConfigHash {
			t.Errorf("%s: hash unchanged after the value changed", name)
		}
	}

	// Swapping an inline value for a value_file is a structural key
	// change (different secret source) even though neither secret enters
	// the hash.
	secret := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(secret, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileSwap := strings.Replace(baseDoc, "value = \"${TYPESAFE_API_KEY}\"", "value_file = \""+secret+"\"", 1)
	other, _ := build(t, fileSwap)
	if base.ConfigHash == other.ConfigHash {
		t.Error("hash unchanged after swapping an inline value for a value_file")
	}
	if got := other.Providers["typesafe"].Keys[0].Value; got != "from-file" {
		t.Errorf("file-backed key value = %q, want from-file", got)
	}
}

// TestConfigHash_OmitsSecretMaterial: rotating a key's secret (env value
// or file contents) does not change the hash — the hash is published by
// /admin/status and must not become a verifier of key material — while
// the resolved snapshot does pick the new secret up.
func TestConfigHash_OmitsSecretMaterial(t *testing.T) {
	t.Parallel()
	cfgA, err := config.Parse([]byte(baseDoc), fakeEnv())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rtA, err := runtime.Build(cfgA)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	cfgB, err := config.Parse([]byte(baseDoc), func(string) (string, bool) { return "rotated-secret", true })
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rtB, err := runtime.Build(cfgB)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if rtA.ConfigHash != rtB.ConfigHash {
		t.Error("hash changed when only the secret value rotated; secrets must not enter the hash")
	}
	if got := rtB.Providers["typesafe"].Keys[0].Value; got != "rotated-secret" {
		t.Errorf("rotated key value = %q, want rotated-secret", got)
	}
	if got := rtA.Providers["typesafe"].Keys[0].Value; got != "ts-secret" {
		t.Errorf("original key value = %q, want ts-secret", got)
	}
}

// TestConfigHash_DefaultsResolveToTheSameHash: an omitted field and its
// explicit default hash identically — the canonical form is the resolved
// configuration. (baseDoc pins log_level indirectly: it omits it.)
func TestConfigHash_DefaultsResolveToTheSameHash(t *testing.T) {
	t.Parallel()
	base, _ := build(t, baseDoc)
	if base.Config.Server.LogLevel != config.DefaultLogLevel {
		t.Fatalf("sanity: base log level = %q, want the default", base.Config.Server.LogLevel)
	}
	explicit := strings.Replace(baseDoc, "[server]\nlisten", "[server]\nlog_level = \"info\"\nlisten", 1)
	other, _ := build(t, explicit)
	if other.ConfigHash != base.ConfigHash {
		t.Errorf("explicit default (log_level = info) hashed differently: %s vs %s", other.ConfigHash, base.ConfigHash)
	}
}

// TestConfigHash_CoversModelMetadata pins that the fields GET /v1/models
// publishes are part of the identity of a configuration: changing a
// description is a real change, not formatting.
func TestConfigHash_CoversModelMetadata(t *testing.T) {
	t.Parallel()
	base := func() *config.Config {
		return &config.Config{
			ConfigVersion: 1,
			Server:        config.Server{Listen: "127.0.0.1:0"},
			Models: map[string]config.Model{
				"m": {
					Targets:     []config.ModelTarget{{Provider: "p", Model: "m"}},
					Description: "first",
					ReleaseDate: "2026-09-15",
				},
			},
		}
	}
	a := runtime.ConfigHash(base())
	changedDesc := base()
	m := changedDesc.Models["m"]
	m.Description = "second"
	changedDesc.Models["m"] = m
	if runtime.ConfigHash(changedDesc) == a {
		t.Fatal("changing description did not change the config hash")
	}
	changedDate := base()
	m = changedDate.Models["m"]
	m.ReleaseDate = "2026-10-01"
	changedDate.Models["m"] = m
	if runtime.ConfigHash(changedDate) == a {
		t.Fatal("changing release_date did not change the config hash")
	}
}
