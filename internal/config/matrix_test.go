package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The validation matrix: one table-test case per rule row of the v1
// configuration contract, each asserting the exact field path and message
// the rule reports. Cases mutate a complete valid document (validDoc) so
// exactly the rule under test can fire; rows that cover several bounds
// assert every listed message in one document.

// matrixDoc returns validDoc with each (old, new) replacement applied once.
func matrixDoc(reps ...[2]string) string {
	doc := validDoc
	for _, r := range reps {
		doc = strings.Replace(doc, r[0], r[1], 1)
	}
	return doc
}

// checkErrors parses and validates doc and asserts the errors equal want
// (field and message, in order).
func checkErrors(t *testing.T, doc string, env map[string]string, want []ConfigError) {
	t.Helper()
	if env == nil {
		env = map[string]string{"TYPESAFE_API_KEY": "ts-secret"}
	}
	cfg, err := Parse([]byte(doc), fakeEnv(env))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	errs := validationErrors(t, cfg.Validate())
	if len(errs) != len(want) {
		t.Fatalf("got %d errors:\n%v\nwant %d:\n%v", len(errs), errs, len(want), want)
	}
	for i := range want {
		if errs[i].Field != want[i].Field || errs[i].Msg != want[i].Msg {
			t.Errorf("errs[%d] = %s: %s, want %s: %s", i, errs[i].Field, errs[i].Msg, want[i].Field, want[i].Msg)
		}
	}
}

func TestValidationMatrix(t *testing.T) {
	t.Parallel()
	absentDir := filepath.Join(t.TempDir(), "does-not-exist")
	cases := []struct {
		name string
		doc  string
		env  map[string]string
		want []ConfigError
	}{
		{
			name: "row 1: config_version must be 1",
			doc:  matrixDoc([2]string{"config_version = 1", "config_version = 2"}),
			want: []ConfigError{{Field: "config_version", Msg: "unsupported version 2 (supported: 1)"}},
		},
		{
			name: "row 2: at least one provider",
			doc: `config_version = 1

[server]
listen = "127.0.0.1:18080"
`,
			want: []ConfigError{{Field: "providers", Msg: "at least one provider is required"}},
		},
		{
			name: "row 3: provider names are unique",
			doc: matrixDoc(
				[2]string{`base_url = "http://127.0.0.1:9"`, `base_url = "http://127.0.0.1:9"`},
			) + `
[[providers]]
name = "typesafe"
base_url = "https://api2.typesafe.ai"
path = "/v1/systemone"
auth = "none"
`,
			want: []ConfigError{{Field: "providers[2].name", Msg: `duplicate "typesafe"`}},
		},
		{
			name: "row 4: plain http is only allowed for loopback",
			doc: matrixDoc(
				[2]string{`base_url = "https://api.typesafe.ai"`, `base_url = "http://api.typesafe.ai"`},
			),
			want: []ConfigError{{Field: "providers[0].base_url", Msg: `scheme "http" is only allowed for loopback`}},
		},
		{
			name: "row 5: path must start with a slash",
			doc:  matrixDoc([2]string{`path = "/v1/systemone"`, `path = "v1/systemone"`}),
			want: []ConfigError{{Field: "providers[0].path", Msg: `must start with "/"`}},
		},
		{
			name: "row 6: bearer auth requires at least one key",
			doc: matrixDoc(
				[2]string{`api_keys = [{ id = "primary", value = "${TYPESAFE_API_KEY}" }]`, ""},
			),
			want: []ConfigError{{Field: "providers[0].api_keys", Msg: "bearer auth requires at least one key"}},
		},
		{
			name: "row 7: auth none must not define keys",
			doc: matrixDoc(
				[2]string{`path = "/v1/systemone"
auth = "none"`, `path = "/v1/systemone"
auth = "none"
api_keys = [{ id = "stray", value = "literal" }]`},
			),
			want: []ConfigError{{Field: "providers[1].api_keys", Msg: `auth "none" must not define keys`}},
		},
		{
			name: "row 8: provider models are unique",
			doc: matrixDoc(
				[2]string{`models = ["jev-latest"]`, `models = ["jev-latest", "jev-latest"]`},
			),
			want: []ConfigError{{Field: "providers[0].models", Msg: `duplicate "jev-latest"`}},
		},
		{
			name: "row 9: targets name an existing provider",
			doc: matrixDoc(
				[2]string{`{ provider = "loopback", model = "nimble" }`, `{ provider = "ollama", model = "nimble" }`},
			),
			want: []ConfigError{{Field: `models."jev-latest".targets[1].provider`, Msg: `unknown provider "ollama"`}},
		},
		{
			name: "row 10: policy must be priority or fastest",
			doc: matrixDoc(
				[2]string{`policy = "priority"`, `policy = "random"`},
			),
			want: []ConfigError{{Field: `models."jev-latest".policy`, Msg: "must be priority or fastest"}},
		},
		{
			name: "row 11: timeout bounds and concurrency floor",
			doc: matrixDoc(
				[2]string{"timeout_ms = 8000\nmax_concurrency = 64", "timeout_ms = 120001\nmax_concurrency = -1"},
			),
			want: []ConfigError{
				{Field: "providers[0].timeout_ms", Msg: "must be between 1 and 120000"},
				{Field: "providers[0].max_concurrency", Msg: "must be at least 1"},
			},
		},
		{
			name: "row 12: provider timeout within the request deadline",
			doc: matrixDoc(
				[2]string{"timeout_ms = 8000", "timeout_ms = 40000"},
			),
			want: []ConfigError{{Field: "providers[0].timeout_ms", Msg: "must not exceed server.request_timeout_ms"}},
		},
		{
			name: "row 13: inbound hash is 64 hex characters",
			doc: matrixDoc(
				[2]string{"[accounts.\"typesafe-main\"]", `[auth]
keys = [
  { id = "app-prod", hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", rpm = 600, tpm = 6000000 },
  { id = "batch",    hash = "not-hex", rpm = 120, tpm = 12000000 },
]

[accounts."typesafe-main"]`},
			),
			want: []ConfigError{{Field: "auth.keys[1].hash", Msg: "must be 64 hex characters"}},
		},
		{
			name: "row 14: quotas are non-negative",
			doc: matrixDoc(
				[2]string{"[accounts.\"typesafe-main\"]", `[auth]
keys = [
  { id = "app-prod", hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", rpm = -5, tpm = -6 },
]

[accounts."typesafe-main"]`},
			),
			want: []ConfigError{
				{Field: "auth.keys[0].rpm", Msg: "must be >= 0"},
				{Field: "auth.keys[0].tpm", Msg: "must be >= 0"},
			},
		},
		{
			name: "row 15: provider account must exist",
			doc: matrixDoc(
				[2]string{"[accounts.\"typesafe-main\"]", "[accounts.\"renamed-main\"]"},
			),
			want: []ConfigError{{Field: "providers[0].account", Msg: `unknown account "typesafe-main"`}},
		},
		{
			name: "row 16: log_bodies requires debug",
			doc: matrixDoc(
				[2]string{`listen = "127.0.0.1:18080"`, "listen = \"127.0.0.1:18080\"\nlog_bodies = true"},
			),
			want: []ConfigError{{Field: "server.log_bodies", Msg: `requires log_level = "debug"`}},
		},
		{
			name: "row 17: admin listener is loopback when no token is set",
			doc: matrixDoc(
				[2]string{`listen = "127.0.0.1:18080"`, "listen = \"127.0.0.1:18080\"\nadmin_listen = \"0.0.0.0:18081\""},
			),
			want: []ConfigError{{Field: "server.admin_listen", Msg: "must be loopback when admin_token is empty"}},
		},
		{
			name: fmt.Sprintf("row 18: usage dir must be writable when enabled (%s)", absentDir),
			doc: matrixDoc(
				[2]string{"[accounts.\"typesafe-main\"]", fmt.Sprintf("[usage]\nenabled = true\ndir = %q\n\n[accounts.\"typesafe-main\"]", absentDir)},
			),
			want: []ConfigError{{Field: "usage.dir", Msg: fmt.Sprintf("%q is not writable", absentDir)}},
		},
		{
			name: "row 19: body and response caps and inflight floor",
			doc: matrixDoc(
				[2]string{`listen = "127.0.0.1:18080"`, "listen = \"127.0.0.1:18080\"\nmax_body_bytes = 512\nmax_response_bytes = 512\nmax_inflight = -1"},
			),
			want: []ConfigError{
				{Field: "server.max_body_bytes", Msg: "must be at least 1024"},
				{Field: "server.max_response_bytes", Msg: "must be at least 1024"},
				{Field: "server.max_inflight", Msg: "must be at least 1"},
			},
		},
		{
			name: "row 20: admin token length when set",
			doc: matrixDoc(
				[2]string{`listen = "127.0.0.1:18080"`, "listen = \"127.0.0.1:18080\"\nadmin_listen = \"127.0.0.1:18081\"\nadmin_token = \"short\""},
			),
			want: []ConfigError{{Field: "server.admin_token", Msg: "must be at least 32 characters"}},
		},
		{
			name: "row 21: breaker bounds",
			doc: matrixDoc(
				[2]string{"max_attempts = 3", "max_attempts = 3\n\n[breaker]\nthreshold = -1\ncooldown_ms = 99"},
			),
			want: []ConfigError{
				{Field: "breaker.threshold", Msg: "must be at least 1"},
				{Field: "breaker.cooldown_ms", Msg: "must be at least 100"},
			},
		},
		{
			name: "row 22: explore_every floor under the fastest policy",
			doc: matrixDoc(
				[2]string{`policy = "priority"`, `policy = "fastest"`},
				[2]string{"max_attempts = 3", "max_attempts = 3\nexplore_every = -1"},
			),
			want: []ConfigError{{Field: `models."jev-latest".explore_every`, Msg: "must be at least 1"}},
		},
		{
			name: "row 23: log level enum",
			doc: matrixDoc(
				[2]string{`listen = "127.0.0.1:18080"`, "listen = \"127.0.0.1:18080\"\nlog_level = \"trace\""},
			),
			want: []ConfigError{{Field: "server.log_level", Msg: `must be one of "debug", "info", "warn", "error"`}},
		},
		{
			name: "row 24: one inbound hash maps to one key id",
			doc: matrixDoc(
				[2]string{"[accounts.\"typesafe-main\"]", `[auth]
keys = [
  { id = "app-prod", hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", rpm = 600, tpm = 6000000 },
  { id = "batch",    hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", rpm = 120, tpm = 12000000 },
]

[accounts."typesafe-main"]`},
			),
			want: []ConfigError{{Field: "auth.keys[1].hash", Msg: `hash already used by key "app-prod"`}},
		},
		{
			name: "row 25: base_url carries no path",
			doc: matrixDoc(
				[2]string{`base_url = "https://api.typesafe.ai"`, `base_url = "https://api.typesafe.ai/v1"`},
			),
			want: []ConfigError{{Field: "providers[0].base_url", Msg: "must be scheme://host[:port] with no path (use providers[].path)"}},
		},
		{
			name: "row 26: target model is offered by its provider",
			doc: matrixDoc(
				[2]string{`{ provider = "typesafe", model = "jev-latest" }`, `{ provider = "typesafe", model = "nimble" }`},
			),
			want: []ConfigError{{Field: `models."jev-latest".targets[0].model`, Msg: `"nimble" is not offered by provider "typesafe"`}},
		},
		{
			name: "row 27: usage bounds when enabled",
			doc: matrixDoc(
				[2]string{"[accounts.\"typesafe-main\"]", "[usage]\nenabled = true\nretention_days = -1\nmax_file_mb = -1\n\n[accounts.\"typesafe-main\"]"},
			),
			want: []ConfigError{
				{Field: "usage.dir", Msg: fmt.Sprintf("%q is not writable", DefaultUsageDir)},
				{Field: "usage.retention_days", Msg: "must be at least 1"},
				{Field: "usage.max_file_mb", Msg: "must be at least 1"},
			},
		},
		{
			name: "row 28: admin listener differs from the main listener",
			doc: matrixDoc(
				[2]string{`listen = "127.0.0.1:18080"`, "listen = \"127.0.0.1:18080\"\nadmin_listen = \"127.0.0.1:18080\""},
			),
			want: []ConfigError{{Field: "server.admin_listen", Msg: "must differ from server.listen (same port on an overlapping address)"}},
		},
		{
			name: "row 29: ewma and hysteresis bounds under the fastest policy",
			doc: matrixDoc(
				[2]string{`policy = "priority"`, `policy = "fastest"`},
				[2]string{"max_attempts = 3", "max_attempts = 3\newma_alpha = 1.5\nhysteresis_ms = -1"},
			),
			want: []ConfigError{
				{Field: `models."jev-latest".ewma_alpha`, Msg: "must be in (0, 1]"},
				{Field: `models."jev-latest".hysteresis_ms`, Msg: "must be at least 0"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			checkErrors(t, tc.doc, tc.env, tc.want)
		})
	}
}

// TestValidationMatrix_Row27WithWritableDirIsolation: row 27's retention
// and size bounds are checked independently of the writability rule when
// the usage directory is writable (the default dir is not, on purpose).
func TestValidationMatrix_Row27WithWritableDirIsolation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	doc := matrixDoc(
		[2]string{"[accounts.\"typesafe-main\"]", fmt.Sprintf("[usage]\nenabled = true\ndir = %q\nretention_days = -1\nmax_file_mb = -1\n\n[accounts.\"typesafe-main\"]", dir)},
	)
	checkErrors(t, doc, nil, []ConfigError{
		{Field: "usage.retention_days", Msg: "must be at least 1"},
		{Field: "usage.max_file_mb", Msg: "must be at least 1"},
	})
}

// TestValidationMatrix_Row18WithUnwritableDirIsolation: an existing but
// unwritable directory is reported as not writable, not just a missing
// one. The probe respects the directory's actual permissions.
func TestValidationMatrix_Row18WithUnwritableDirIsolation(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses directory permissions; the probe cannot fail")
	}
	t.Parallel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	doc := matrixDoc(
		[2]string{"[accounts.\"typesafe-main\"]", fmt.Sprintf("[usage]\nenabled = true\ndir = %q\n\n[accounts.\"typesafe-main\"]", dir)},
	)
	checkErrors(t, doc, nil, []ConfigError{{Field: "usage.dir", Msg: fmt.Sprintf("%q is not writable", dir)}})
}

// TestValidationMatrix_IntegerOverflowSurfacesFieldPaths: an integer far
// outside its legal range still decodes (TOML integers are 64-bit) and is
// reported by the semantic range rule with its field path — never as a
// decoder type-name message.
func TestValidationMatrix_IntegerOverflowSurfacesFieldPaths(t *testing.T) {
	t.Parallel()
	doc := matrixDoc([2]string{"timeout_ms = 8000", "timeout_ms = 999999999999"})
	checkErrors(t, doc, nil, []ConfigError{
		{Field: "providers[0].timeout_ms", Msg: "must be between 1 and 120000"},
	})
}
