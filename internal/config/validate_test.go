package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// validationErrors extracts the ValidationErrors list from a Validate
// result; nil for a valid configuration.
func validationErrors(t *testing.T, err error) []ConfigError {
	t.Helper()
	if err == nil {
		return nil
	}
	var verrs ValidationErrors
	if !errors.As(err, &verrs) {
		t.Fatalf("Validate() error %v (%T) is not ValidationErrors", err, err)
	}
	return verrs
}

// validDoc is a complete, valid configuration used as the base for
// validation tests: mutations of it isolate one rule at a time.
const validDoc = `
config_version = 1

[server]
listen = "127.0.0.1:18080"

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
`

// validConfig parses validDoc with the environment its references need and
// returns the resulting Config.
func validConfig(t *testing.T) *Config {
	t.Helper()
	cfg, err := Parse([]byte(validDoc), fakeEnv(map[string]string{"TYPESAFE_API_KEY": "ts-secret"}))
	if err != nil {
		t.Fatalf("parse validDoc: %v", err)
	}
	return cfg
}

func TestValidate_MinimalConfigHasNoErrors(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(minimalDoc), fakeEnv(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("got %v, want nil", err)
	}
}

func TestValidate_CompleteConfigHasNoErrors(t *testing.T) {
	t.Parallel()
	if err := validConfig(t).Validate(); err != nil {
		t.Fatalf("got %v, want nil", err)
	}
}

func TestValidate_ConfigVersionMustBeOne(t *testing.T) {
	t.Parallel()
	for _, version := range []int{0, 2, -1} {
		cfg := validConfig(t)
		cfg.ConfigVersion = version
		errs := validationErrors(t, cfg.Validate())
		want := fmt.Sprintf("unsupported version %d (supported: 1)", version)
		if len(errs) == 0 || errs[0].Field != "config_version" || errs[0].Msg != want {
			t.Fatalf("version %d: got %v, want [config_version: %s]", version, errs, want)
		}
	}
}

func TestValidate_AtLeastOneProviderIsRequired(t *testing.T) {
	t.Parallel()
	cfg := validConfig(t)
	cfg.Providers = nil
	cfg.Models = nil
	errs := validationErrors(t, cfg.Validate())
	if len(errs) != 1 || errs[0].Field != "providers" || errs[0].Msg != "at least one provider is required" {
		t.Fatalf("got %v, want [providers: at least one provider is required]", errs)
	}
}

// TestValidate_ProviderNameIsRequired: empty names are rejected at their
// index before the uniqueness check, so two empty names yield two
// name-required errors rather than a duplicate error.
func TestValidate_ProviderNameIsRequired(t *testing.T) {
	t.Parallel()
	cfg := validConfig(t)
	cfg.Providers = []Provider{
		{Name: "", BaseURL: "https://a.example", Path: "/p", Auth: "none", TimeoutMs: 1000, MaxConcurrency: 1},
		{Name: "", BaseURL: "https://b.example", Path: "/p", Auth: "none", TimeoutMs: 1000, MaxConcurrency: 1},
		{Name: "a", BaseURL: "https://c.example", Path: "/p", Auth: "none", TimeoutMs: 1000, MaxConcurrency: 1},
	}
	errs := validationErrors(t, cfg.Validate())
	var nameErrs []ConfigError
	for _, e := range errs {
		if strings.HasSuffix(e.Field, ".name") {
			nameErrs = append(nameErrs, e)
		}
	}
	if len(nameErrs) != 2 {
		t.Fatalf("got %v, want exactly the two name-required errors", errs)
	}
	for i, want := range []int{0, 1} {
		if nameErrs[i].Field != fmt.Sprintf("providers[%d].name", want) || nameErrs[i].Msg != "name is required" {
			t.Fatalf("name error %d = %v, want providers[%d].name: name is required", i, nameErrs[i], want)
		}
	}
}

func TestValidate_ProviderNamesMustBeUnique(t *testing.T) {
	t.Parallel()
	cfg := validConfig(t)
	cfg.Providers = []Provider{
		{Name: "x", BaseURL: "https://a.example", Path: "/p", Auth: "none", TimeoutMs: 1000, MaxConcurrency: 1},
		{Name: "y", BaseURL: "https://b.example", Path: "/p", Auth: "none", TimeoutMs: 1000, MaxConcurrency: 1},
		{Name: "x", BaseURL: "https://c.example", Path: "/p", Auth: "none", TimeoutMs: 1000, MaxConcurrency: 1},
	}
	cfg.Models = nil
	errs := validationErrors(t, cfg.Validate())
	if len(errs) != 1 {
		t.Fatalf("got %v, want exactly the duplicate error", errs)
	}
	want := `duplicate "x"`
	if errs[0].Field != "providers[2].name" || errs[0].Msg != want {
		t.Fatalf("got %v, want [providers[2].name: %s]", errs, want)
	}
}

// TestValidate_ShutdownGraceBounds: 0 keeps its "use the default"
// meaning, 600000 (ten minutes) is the largest accepted value, and both
// sides outside the range are rejected with the same message.
func TestValidate_ShutdownGraceBounds(t *testing.T) {
	t.Parallel()
	for _, grace := range []int{-1, -10_000, 600_001} {
		cfg := validConfig(t)
		cfg.Server.ShutdownGraceMs = grace
		errs := validationErrors(t, cfg.Validate())
		if len(errs) != 1 || errs[0].Field != "server.shutdown_grace_ms" || errs[0].Msg != "must be between 0 and 600000" {
			t.Fatalf("grace %d: got %v, want [server.shutdown_grace_ms: must be between 0 and 600000]", grace, errs)
		}
	}
	for _, grace := range []int{0, 1, 10_000, 600_000} {
		cfg := validConfig(t)
		cfg.Server.ShutdownGraceMs = grace
		if err := cfg.Validate(); err != nil {
			t.Errorf("grace %d: got %v, want nil", grace, err)
		}
	}
	// Grace 0 keeps its "use the default" meaning at parse time.
	doc := strings.Replace(minimalDoc, "[server]", "[server]\nshutdown_grace_ms = 0", 1)
	cfg, err := Parse([]byte(doc), fakeEnv(nil))
	if err != nil {
		t.Fatalf("parse with grace 0: %v", err)
	}
	if cfg.Server.ShutdownGraceMs != DefaultShutdownGraceMs {
		t.Errorf("grace 0: ShutdownGraceMs = %d, want the default %d", cfg.Server.ShutdownGraceMs, DefaultShutdownGraceMs)
	}
}

func TestValidate_ListenMustBeHostPort(t *testing.T) {
	t.Parallel()
	cases := []struct {
		listen string
		wantIn string
	}{
		{listen: "127.0.0.1:8080", wantIn: ""}, // valid
		{listen: ":8080", wantIn: ""},          // valid: all interfaces
		{listen: "[::1]:8080", wantIn: ""},     // valid: IPv6 loopback
		{listen: "127.0.0.1", wantIn: "missing port in address"},
		{listen: "127.0.0.1:notaport", wantIn: `port "notaport" is not numeric`},
		// The port must be strictly numeric: strconv would accept the sign.
		{listen: "127.0.0.1:+8080", wantIn: `port "+8080" is not numeric`},
		{listen: "127.0.0.1:-8080", wantIn: `port "-8080" is not numeric`},
		{listen: "127.0.0.1: 8080", wantIn: `port " 8080" is not numeric`},
		{listen: "127.0.0.1:0", wantIn: "outside 1..65535"},
		{listen: "127.0.0.1:70000", wantIn: "outside 1..65535"},
		{listen: "", wantIn: "missing port in address"},
	}
	for _, tc := range cases {
		cfg := validConfig(t)
		cfg.Server.Listen = tc.listen
		errs := validationErrors(t, cfg.Validate())
		if tc.wantIn == "" {
			if len(errs) != 0 {
				t.Errorf("listen %q: got errors %v, want none", tc.listen, errs)
			}
			continue
		}
		found := false
		for _, e := range errs {
			if e.Field == "server.listen" && strings.Contains(e.Msg, tc.wantIn) {
				found = true
			}
		}
		if !found {
			t.Errorf("listen %q: got %v, want server.listen error containing %q", tc.listen, errs, tc.wantIn)
		}
	}
}

func TestValidate_AllErrorsAreAggregated(t *testing.T) {
	t.Parallel()
	cfg := validConfig(t)
	cfg.ConfigVersion = 9
	cfg.Server.Listen = "no-port-here"
	cfg.Server.ShutdownGraceMs = -1
	cfg.Providers = []Provider{
		{Name: "dup", BaseURL: "https://a.example", Path: "/p", Auth: "none", TimeoutMs: 1000, MaxConcurrency: 1},
		{Name: "dup", BaseURL: "https://b.example", Path: "/p", Auth: "none", TimeoutMs: 1000, MaxConcurrency: 1},
	}
	cfg.Models = nil
	errs := validationErrors(t, cfg.Validate())
	if len(errs) != 4 {
		t.Fatalf("got %d errors (%v), want 4", len(errs), errs)
	}
	wantFields := []string{
		"config_version",
		"server.listen",
		"server.shutdown_grace_ms",
		"providers[1].name",
	}
	for i, field := range wantFields {
		if errs[i].Field != field {
			t.Errorf("errs[%d].Field = %q, want %q", i, errs[i].Field, field)
		}
	}
}

// TestValidate_FiveIndependentErrorsAreAllReported pins the aggregation
// contract on a document with five unrelated violations: every one is
// reported in one pass.
func TestValidate_FiveIndependentErrorsAreAllReported(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(validDoc, "config_version = 1", "config_version = 2", 1)
	doc = strings.Replace(doc, `listen = "127.0.0.1:18080"`, "listen = \"127.0.0.1:18080\"\nlog_level = \"trace\"\nadmin_listen = \"127.0.0.1:18081\"\nadmin_token = \"short\"", 1)
	doc = strings.Replace(doc, "max_attempts = 3", "max_attempts = 3\n", 1)
	// Duplicate the typesafe provider as a third entry.
	doc += `
[[providers]]
name = "typesafe"
base_url = "https://api2.typesafe.ai"
path = "/v1/systemone"
auth = "none"

[breaker]
threshold = -1
`
	cfg, err := Parse([]byte(doc), fakeEnv(map[string]string{"TYPESAFE_API_KEY": "ts-secret"}))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	errs := validationErrors(t, cfg.Validate())
	want := []struct{ field, msg string }{
		{"config_version", "unsupported version 2 (supported: 1)"},
		{"server.log_level", `must be one of "debug", "info", "warn", "error"`},
		{"server.admin_token", "must be at least 32 characters"},
		{"providers[2].name", `duplicate "typesafe"`},
		{"breaker.threshold", "must be at least 1"},
	}
	if len(errs) != len(want) {
		t.Fatalf("got %d errors (%v), want %d", len(errs), errs, len(want))
	}
	for i, w := range want {
		if errs[i].Field != w.field || errs[i].Msg != w.msg {
			t.Errorf("errs[%d] = %s: %s, want %s: %s", i, errs[i].Field, errs[i].Msg, w.field, w.msg)
		}
	}
}

// TestValidate_ResultUnwrapsForErrorsAs pins the exported error contract:
// callers use errors.As on the returned error, no hand-rolled conversion.
func TestValidate_ResultUnwrapsForErrorsAs(t *testing.T) {
	t.Parallel()
	cfg := validConfig(t)
	cfg.Server.Listen = "bad"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var verrs ValidationErrors
	if !errors.As(err, &verrs) {
		t.Fatalf("errors.As(ValidationErrors) = false for %v", err)
	}
	var ce ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("errors.As(ConfigError) = false for %v", err)
	}
}

// TestValidate_ModelIDsAreSpliceSafe pins the charset rule that keeps the
// model rewrite a pure byte splice: an alias or target model containing a
// quote, a backslash or whitespace is rejected at boot, not at request
// time.
func TestValidate_ModelIDsAreSpliceSafe(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, doc, field, msg string }{
		{
			name:  "alias with a quote",
			doc:   strings.Replace(validDoc, `[models."jev-latest"]`, `[models."jev\"latest"]`, 1),
			field: `models."jev\"latest"`,
			msg:   "model alias must match [A-Za-z0-9._:-]+",
		},
		{
			name:  "alias with a space",
			doc:   strings.Replace(validDoc, `[models."jev-latest"]`, `[models."jev latest"]`, 1),
			field: `models."jev latest"`,
			msg:   "model alias must match [A-Za-z0-9._:-]+",
		},
		{
			name:  "target model with a backslash",
			doc:   strings.Replace(validDoc, `model = "jev-latest"`, `model = "jev\\latest"`, 1),
			field: `models."jev-latest".targets[0].model`,
			msg:   "model id must match [A-Za-z0-9._:-]+",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Parse([]byte(tc.doc), fakeEnv(map[string]string{"TYPESAFE_API_KEY": "ts-secret"}))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			errs := validationErrors(t, cfg.Validate())
			found := false
			for _, e := range errs {
				if strings.HasPrefix(e.Field, tc.field) && strings.Contains(e.Msg, tc.msg) {
					found = true
				}
			}
			if !found {
				t.Fatalf("errors = %v, want %s containing %q", errs, tc.field, tc.msg)
			}
		})
	}
}

// TestValidate_ModelMetadataBounds pins the two metadata rules: a release
// date must be a real calendar date and a description is bounded.
func TestValidate_ModelMetadataBounds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		old   string
		repl  string
		field string
		msg   string
	}{
		{
			name:  "release date shape",
			old:   "max_attempts = 3",
			repl:  "max_attempts = 3\nrelease_date = \"15-09-2026\"",
			field: `models."jev-latest".release_date`,
			msg:   "must be a YYYY-MM-DD date",
		},
		{
			name:  "impossible calendar date",
			old:   "max_attempts = 3",
			repl:  "max_attempts = 3\nrelease_date = \"2026-13-45\"",
			field: `models."jev-latest".release_date`,
			msg:   "must be a YYYY-MM-DD date",
		},
		{
			name:  "description too long",
			old:   "max_attempts = 3",
			repl:  "max_attempts = 3\ndescription = \"" + strings.Repeat("x", 501) + "\"",
			field: `models."jev-latest".description`,
			msg:   "must be at most 500 characters",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc := strings.Replace(validDoc, tc.old, tc.repl, 1)
			cfg, err := Parse([]byte(doc), fakeEnv(map[string]string{"TYPESAFE_API_KEY": "ts-secret"}))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			errs := validationErrors(t, cfg.Validate())
			for _, e := range errs {
				if e.Field == tc.field && strings.Contains(e.Msg, tc.msg) {
					return
				}
			}
			t.Fatalf("errors = %v, want %s containing %q", errs, tc.field, tc.msg)
		})
	}

	// A valid date and a normal description are accepted.
	doc := strings.Replace(validDoc, "max_attempts = 3",
		"max_attempts = 3\nrelease_date = \"2026-09-15\"\ndescription = \"Latest model.\"", 1)
	cfg, err := Parse([]byte(doc), fakeEnv(map[string]string{"TYPESAFE_API_KEY": "ts-secret"}))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if errs := validationErrors(t, cfg.Validate()); len(errs) != 0 {
		t.Fatalf("valid metadata rejected: %v", errs)
	}
}

// TestValidate_IdentifiersAreLogSafe pins the charset rule for every
// operator-supplied identity that reaches logs or metric labels.
func TestValidate_IdentifiersAreLogSafe(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		old   string
		repl  string
		field string
	}{
		{
			name:  "provider name",
			old:   `name = "loopback"`,
			repl:  `name = "loop back"`,
			field: "providers[1].name",
		},
		{
			name:  "provider key id",
			old:   `api_keys = [{ id = "primary", value = "${TYPESAFE_API_KEY}" }]`,
			repl:  `api_keys = [{ id = "pri mary", value = "${TYPESAFE_API_KEY}" }]`,
			field: "providers[0].api_keys[0].id",
		},
		{
			name:  "account name",
			old:   `[accounts."typesafe-main"]`,
			repl:  `[accounts."typesafe main"]`,
			field: `accounts."typesafe main"`,
		},
		{
			name:  "provider model id",
			old:   `models = ["jev-latest"]`,
			repl:  `models = ["jev latest"]`,
			field: "providers[0].models",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc := strings.Replace(validDoc, tc.old, tc.repl, 1)
			cfg, err := Parse([]byte(doc), fakeEnv(map[string]string{"TYPESAFE_API_KEY": "ts-secret"}))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			errs := validationErrors(t, cfg.Validate())
			for _, e := range errs {
				if e.Field == tc.field {
					return
				}
			}
			t.Fatalf("errors = %v, want one anchored at %s", errs, tc.field)
		})
	}

	// The client id check needs an [auth] section, which validDoc omits.
	doc := validDoc + `
[auth]
keys = [{ id = "a\nb", hash = "` + strings.Repeat("a", 64) + `" }]
`
	cfg, err := Parse([]byte(doc), fakeEnv(map[string]string{"TYPESAFE_API_KEY": "ts-secret"}))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	errs := validationErrors(t, cfg.Validate())
	for _, e := range errs {
		if e.Field == "auth.keys[0].id" {
			return
		}
	}
	t.Fatalf("errors = %v, want one anchored at auth.keys[0].id", errs)
}

// TestValidate_DuplicateProviderKeyIDs is a fail-closed guard: runtime
// secret lookup is by id, so duplicates would silently shadow a secret.
func TestValidate_DuplicateProviderKeyIDs(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(validDoc,
		`api_keys = [{ id = "primary", value = "${TYPESAFE_API_KEY}" }]`,
		`api_keys = [
  { id = "primary", value = "${TYPESAFE_API_KEY}" },
  { id = "primary", value = "second" },
]`, 1)
	cfg, err := Parse([]byte(doc), fakeEnv(map[string]string{"TYPESAFE_API_KEY": "ts-secret"}))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	errs := validationErrors(t, cfg.Validate())
	for _, e := range errs {
		if e.Field == "providers[0].api_keys[1].id" && strings.Contains(e.Msg, "duplicate") {
			return
		}
	}
	t.Fatalf("errors = %v, want a duplicate id error", errs)
}

// TestValidate_ListenerOverlap pins that addresses which collide at bind
// time are rejected even when the strings differ.
func TestValidate_ListenerOverlap(t *testing.T) {
	t.Parallel()
	conflicts := []struct{ listen, admin string }{
		{"0.0.0.0:8080", "127.0.0.1:8080"},
		{"127.0.0.1:8080", "0.0.0.0:8080"},
		{"localhost:8080", "127.0.0.1:8080"},
		{"[::]:8080", "[::1]:8080"},
		{"127.0.0.1:8080", "127.0.0.1:8080"},
	}
	for _, tc := range conflicts {
		if !listenersConflict(tc.listen, tc.admin) {
			t.Errorf("listenersConflict(%q, %q) = false, want true", tc.listen, tc.admin)
		}
	}
	distinct := []struct{ listen, admin string }{
		{"127.0.0.1:8080", "127.0.0.1:8081"},
		{"127.0.0.1:8080", "127.0.0.2:8080"},
		{"[::1]:8080", "127.0.0.1:8080"},
	}
	for _, tc := range distinct {
		if listenersConflict(tc.listen, tc.admin) {
			t.Errorf("listenersConflict(%q, %q) = true, want false", tc.listen, tc.admin)
		}
	}
}
