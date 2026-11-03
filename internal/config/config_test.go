package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const minimalDoc = `
config_version = 1

[server]
listen = "127.0.0.1:18080"

[[providers]]
name = "loopback"
base_url = "http://127.0.0.1:9"
path = "/v1/systemone"
auth = "none"
`

func TestParse_MinimalConfigDecodes(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(minimalDoc), fakeEnv(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ConfigVersion != 1 {
		t.Errorf("ConfigVersion = %d, want 1", cfg.ConfigVersion)
	}
	if cfg.Server.Listen != "127.0.0.1:18080" {
		t.Errorf("Listen = %q, want 127.0.0.1:18080", cfg.Server.Listen)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("got %d providers, want 1", len(cfg.Providers))
	}
	p := cfg.Providers[0]
	if p.Name != "loopback" || p.BaseURL != "http://127.0.0.1:9" || p.Path != "/v1/systemone" || p.Auth != "none" {
		t.Errorf("provider decoded as %+v", p)
	}
}

func TestParse_ShutdownGraceDefaultsToTenSeconds(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(minimalDoc), fakeEnv(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.ShutdownGraceMs != DefaultShutdownGraceMs {
		t.Errorf("ShutdownGraceMs = %d, want %d", cfg.Server.ShutdownGraceMs, DefaultShutdownGraceMs)
	}
}

func TestParse_ExplicitShutdownGraceIsKept(t *testing.T) {
	t.Parallel()
	doc := `
config_version = 1

[server]
listen = "127.0.0.1:18080"
shutdown_grace_ms = 2500

[[providers]]
name = "loopback"
base_url = "http://127.0.0.1:9"
path = "/v1/systemone"
auth = "none"
`
	cfg, err := Parse([]byte(doc), fakeEnv(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.ShutdownGraceMs != 2500 {
		t.Errorf("ShutdownGraceMs = %d, want 2500", cfg.Server.ShutdownGraceMs)
	}
}

func TestParse_UnknownTopLevelFieldIsRejected(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte("config_version = 1\nfrobnicate = true\n"), fakeEnv(nil))
	assertConfigError(t, err, "frobnicate", "unknown field")
}

func TestParse_UnknownNestedFieldIsRejectedWithFieldPath(t *testing.T) {
	t.Parallel()
	doc := `
config_version = 1

[server]
listen = "127.0.0.1:18080"
port = 9090

[[providers]]
name = "loopback"
base_url = "http://127.0.0.1:9"
path = "/v1/systemone"
auth = "none"
`
	_, err := Parse([]byte(doc), fakeEnv(nil))
	assertConfigError(t, err, "server.port", "unknown field")
}

// TestParse_UnknownFieldInsideArrayTableIsRejected: the walk anchors the
// unknown key to the offending element with its index, for both the
// [[array of tables]] and inline-table spellings.
func TestParse_UnknownFieldInsideArrayTableIsRejected(t *testing.T) {
	t.Parallel()
	doc := minimalDoc + "retries = 3\n"
	_, err := Parse([]byte(doc), fakeEnv(nil))
	assertConfigError(t, err, "providers[0].retries", "unknown field")
}

// TestParse_UnknownFieldInsideInlineTableArrayUsesIndexPath pins the fix
// for the bare-path bug: go-toml anchors an unknown key inside an inline
// table to "bogus" alone, losing the providers[0] context; the walk keeps
// the full index-aware path.
func TestParse_UnknownFieldInsideInlineTableArrayUsesIndexPath(t *testing.T) {
	t.Parallel()
	doc := `config_version = 1
providers = [{name = "p", bogus = 1}]
`
	_, err := Parse([]byte(doc), fakeEnv(nil))
	assertConfigError(t, err, "providers[0].bogus", "unknown field")
}

// TestParse_ProvidersShapeErrorAndUnknownFieldAreBothReported pins the
// aggregation across kinds: a wrong-shaped providers (integer where an
// array of tables is expected) must not hide the unknown top-level field,
// even though the real decoder abandons immediately.
func TestParse_ProvidersShapeErrorAndUnknownFieldAreBothReported(t *testing.T) {
	t.Parallel()
	doc := "config_version = 1\nfrobnicate = true\nproviders = 3\n"
	_, err := Parse([]byte(doc), fakeEnv(nil))
	errs := asConfigErrors(t, err)
	if len(errs) != 2 {
		t.Fatalf("got %d errors (%v), want 2", len(errs), errs)
	}
	assertConfigError(t, err, "frobnicate", "unknown field")
	assertConfigError(t, err, "providers", "expected array, got integer")
}

// TestParse_ServerShapeErrorAndUnknownFieldAreBothReported: same pin with
// a table-vs-integer mismatch on server.
func TestParse_ServerShapeErrorAndUnknownFieldAreBothReported(t *testing.T) {
	t.Parallel()
	doc := "config_version = 1\nbogus_key = 1\nserver = 5\n"
	_, err := Parse([]byte(doc), fakeEnv(nil))
	errs := asConfigErrors(t, err)
	if len(errs) != 2 {
		t.Fatalf("got %d errors (%v), want 2", len(errs), errs)
	}
	assertConfigError(t, err, "bogus_key", "unknown field")
	assertConfigError(t, err, "server", "expected table, got integer")
}

// TestParse_TypeMismatchesAreReportedAsShapeErrors covers the shape matrix
// on the real schema: scalar and nested mismatches alike render as
// "expected <kind>, got <toml kind>".
func TestParse_TypeMismatchesAreReportedAsShapeErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		doc       string
		wantField string
		wantMsg   string
	}{
		{doc: "config_version = \"one\"\n", wantField: "config_version", wantMsg: "expected integer, got string"},
		{doc: "config_version = 1.5\n", wantField: "config_version", wantMsg: "expected integer, got float"},
		{
			doc:       strings.Replace(minimalDoc, `listen = "127.0.0.1:18080"`, "listen = \"127.0.0.1:18080\"\nshutdown_grace_ms = \"slow\"", 1),
			wantField: "server.shutdown_grace_ms", wantMsg: "expected integer, got string",
		},
		{
			doc:       strings.Replace(minimalDoc, `listen = "127.0.0.1:18080"`, "listen = 8080", 1),
			wantField: "server.listen", wantMsg: "expected string, got integer",
		},
		{doc: minimalDoc + "models = 3\n", wantField: "providers[0].models", wantMsg: "expected array, got integer"},
		{doc: minimalDoc + "models = [\"m\", 4]\n", wantField: "providers[0].models[1]", wantMsg: "expected string, got integer"},
		{doc: minimalDoc + "models = [4]\n", wantField: "providers[0].models[0]", wantMsg: "expected string, got integer"},
	}
	for _, tc := range cases {
		t.Run(tc.wantField, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.doc), fakeEnv(nil))
			assertConfigError(t, err, tc.wantField, tc.wantMsg)
		})
	}
}

// TestParse_SingleTableIsLenientlyAcceptedAsOneElementSlice documents the
// decoder's [providers]-vs-[[providers]] leniency: a single table decodes
// as a one-element list, and unknown keys inside it keep their indexed
// path.
func TestParse_SingleTableIsLenientlyAcceptedAsOneElementSlice(t *testing.T) {
	t.Parallel()
	doc := `
config_version = 1

[server]
listen = "127.0.0.1:18080"

[providers]
name = "solo"
base_url = "http://127.0.0.1:9"
path = "/v1/systemone"
auth = "none"
`
	cfg, err := Parse([]byte(doc), fakeEnv(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "solo" {
		t.Fatalf("providers = %+v, want one element named solo", cfg.Providers)
	}

	_, err = Parse([]byte(doc+"retries = 3\n"), fakeEnv(nil))
	assertConfigError(t, err, "providers[0].retries", "unknown field")
}

// TestParse_DuplicateKeyIsAnError: duplicate TOML keys are rejected by the
// parser itself, before any walking.
func TestParse_DuplicateKeyIsAnError(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte("config_version = 1\nconfig_version = 2\n"), fakeEnv(nil))
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "already defined") {
		t.Errorf("error %q does not mention the duplicate key", err)
	}
}

func TestParse_MultipleUnknownFieldsAreAllReported(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte("config_version = 1\none = 1\ntwo = 2\n"), fakeEnv(nil))
	errs := asConfigErrors(t, err)
	if len(errs) != 2 {
		t.Fatalf("got %d errors (%v), want 2", len(errs), errs)
	}
}

// TestParse_UnknownFieldAndTypeErrorsAreBothReported pins the aggregation:
// a document with one unknown field and one wrong-typed field must report
// both, even though the decoder abandons strict checking at the type error.
func TestParse_UnknownFieldAndTypeErrorsAreBothReported(t *testing.T) {
	t.Parallel()
	doc := "frobnicate = true\nconfig_version = \"one\"\n"
	_, err := Parse([]byte(doc), fakeEnv(nil))
	errs := asConfigErrors(t, err)
	if len(errs) != 2 {
		t.Fatalf("got %d errors (%v), want 2", len(errs), errs)
	}
	assertConfigError(t, err, "frobnicate", "unknown field")
	assertConfigError(t, err, "config_version", "expected integer, got string")
}

// modelPolicy is the map-of-structs fixture for the map-keyed table tests.
type modelPolicy struct {
	Policy int `toml:"policy"`
}

type modelsConfig struct {
	Models map[string]modelPolicy `toml:"models"`
}

// TestParse_MapKeyedTableReportsExactlyOneError pins the walk/decoder
// dedup for map-keyed tables: a wrong-typed field inside [models."x"] is
// reported once, at the walk's TOML-quoted path. The decoder's duplicate
// (which carries no key path at all — go-toml loses it through the map)
// must be dropped, not appended next to the walk's report.
func TestParse_MapKeyedTableReportsExactlyOneError(t *testing.T) {
	t.Parallel()
	doc := []byte("[models.\"x\"]\npolicy = \"five\"\n")
	var cfg modelsConfig
	errs := decodeErrors(doc, &cfg)
	if len(errs) != 1 {
		t.Fatalf("got %d errors (%v), want exactly 1", len(errs), errs)
	}
	want := `models."x".policy`
	if errs[0].Field != want || errs[0].Msg != "expected integer, got string" {
		t.Fatalf("got %v, want {Field: %q, Msg: %q}", errs[0], want, "expected integer, got string")
	}
}

// TestParse_UnknownFieldInsideMapValueReportsExactlyOnce: the unknown-key
// twin of the dedup test — the walk's indexed report stands alone and the
// decoder's strict-missing error is suppressed.
func TestParse_UnknownFieldInsideMapValueReportsExactlyOnce(t *testing.T) {
	t.Parallel()
	doc := []byte("[models.\"x\"]\npolicy = 5\nbogus = 1\n")
	var cfg modelsConfig
	errs := decodeErrors(doc, &cfg)
	if len(errs) != 1 {
		t.Fatalf("got %d errors (%v), want exactly 1", len(errs), errs)
	}
	if errs[0].Field != `models."x".bogus` || errs[0].Msg != msgUnknownField {
		t.Fatalf("got %v, want {Field: %q, Msg: %q}", errs[0], `models."x".bogus`, msgUnknownField)
	}
}

// TestParse_KeyMatchingIsCaseSensitive pins the deliberate strictness: keys
// are matched exactly, so [Server] and Config_Version are unknown fields
// even though go-toml's decoder case-folds key matching (ADR 0002).
func TestParse_KeyMatchingIsCaseSensitive(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		doc       string
		wantField string
	}{
		{doc: "[Server]\nlisten = \"127.0.0.1:1\"\n", wantField: "Server"},
		{doc: "Config_Version = 1\n", wantField: "Config_Version"},
	} {
		_, err := Parse([]byte(tc.doc), fakeEnv(nil))
		errs := asConfigErrors(t, err)
		if len(errs) != 1 {
			t.Fatalf("doc %q: got %d errors (%v), want exactly 1", tc.doc, len(errs), errs)
		}
		if errs[0].Field != tc.wantField || errs[0].Msg != msgUnknownField {
			t.Fatalf("doc %q: got %v, want {Field: %q, Msg: %q}", tc.doc, errs[0], tc.wantField, msgUnknownField)
		}
	}
}

// TestParse_CustomScalarDecodesWithoutWalkError: a schema type whose
// pointer implements encoding.TextUnmarshaler owns its decoding; the
// pipeline must decode "5s" into it cleanly, with no walk error.
func TestParse_CustomScalarDecodesWithoutWalkError(t *testing.T) {
	t.Parallel()
	type timeoutConfig struct {
		Timeout walkDuration `toml:"timeout"`
	}
	var cfg timeoutConfig
	errs := decodeErrors([]byte("timeout = \"5s\"\n"), &cfg)
	if len(errs) != 0 {
		t.Fatalf("got %v, want no errors", errs)
	}
	if cfg.Timeout.Duration != 5*time.Second {
		t.Fatalf("Timeout = %v, want 5s", cfg.Timeout.Duration)
	}
}

// TestParse_EmptyKeyRendersAsPlaceholder: TOML permits "" as a key; its
// path segment renders as <empty> instead of a dangling dot (or a bare
// top-level message).
func TestParse_EmptyKeyRendersAsPlaceholder(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte("\"\" = 1\n"), fakeEnv(nil))
	errs := asConfigErrors(t, err)
	if len(errs) != 1 || errs[0].Field != "<empty>" || errs[0].Msg != msgUnknownField {
		t.Fatalf("top-level empty key: got %v (len %d), want exactly [<empty>: unknown field]", errs, len(errs))
	}

	_, err = Parse([]byte("[server]\n\"\" = 1\nlisten = \"127.0.0.1:1\"\n"), fakeEnv(nil))
	errs = asConfigErrors(t, err)
	if len(errs) != 1 || errs[0].Field != "server.<empty>" || errs[0].Msg != msgUnknownField {
		t.Fatalf("nested empty key: got %v (len %d), want exactly [server.<empty>: unknown field]", errs, len(errs))
	}
}

func TestParse_ExpansionErrorCarriesFieldPathAndVariableName(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(minimalDoc, "127.0.0.1:18080", "${JEVLAR_ABSENT_LISTEN}", 1)
	_, err := Parse([]byte(doc), fakeEnv(nil))
	assertConfigError(t, err, "server.listen", `variable "JEVLAR_ABSENT_LISTEN" is not set`)
}

func TestParse_ExpansionInsideProviderFieldsCarriesIndexedFieldPath(t *testing.T) {
	t.Parallel()
	doc := `
config_version = 1

[server]
listen = "127.0.0.1:18080"

[[providers]]
name = "p1"
base_url = "http://127.0.0.1:9"
path = "/v1/systemone"
auth = "none"

[[providers]]
name = "p2"
base_url = "${UPSTREAM}"
path = "/v1/systemone"
auth = "none"
`
	_, err := Parse([]byte(doc), fakeEnv(nil))
	assertConfigError(t, err, "providers[1].base_url", `variable "UPSTREAM" is not set`)
}

func TestParse_ExpansionIsAppliedToDecodedValues(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(minimalDoc, "http://127.0.0.1:9", "http://${HOST:-127.0.0.1}:${PORT}", 1)
	cfg, err := Parse([]byte(doc), fakeEnv(map[string]string{"HOST": "10.1.2.3", "PORT": "11434"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := cfg.Providers[0].BaseURL, "http://10.1.2.3:11434"; got != want {
		t.Fatalf("BaseURL = %q, want %q", got, want)
	}
}

func TestParse_EscapedDollarSurvivesDecoding(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(minimalDoc, `path = "/v1/systemone"`, `path = "/v$$one"`, 1)
	cfg, err := Parse([]byte(doc), fakeEnv(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := cfg.Providers[0].Path, "/v$one"; got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}
}

func TestParse_ArrayElementsAreExpandedWithIndexedPaths(t *testing.T) {
	t.Parallel()
	doc := minimalDoc + `models = ["${M}"]
`
	cfg, err := Parse([]byte(doc), fakeEnv(map[string]string{"M": "nimble"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := cfg.Providers[0].Models, []string{"nimble"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("Models = %v, want %v", got, want)
	}
}

func TestParse_ArrayElementExpansionErrorCarriesIndexedPath(t *testing.T) {
	t.Parallel()
	doc := minimalDoc + `models = ["${NOPE}"]
`
	_, err := Parse([]byte(doc), fakeEnv(nil))
	assertConfigError(t, err, "providers[0].models[0]", `variable "NOPE" is not set`)
}

func TestParse_MalformedTomlIsAnError(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte("config_version = "), fakeEnv(nil))
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func TestParse_WrongTypeIsAnError(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte("config_version = \"one\"\n"), fakeEnv(nil))
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
}

// TestParse_StripsUTF8BOM: editors on some platforms prepend a BOM; it is
// not part of the TOML grammar and must not fail decoding.
func TestParse_StripsUTF8BOM(t *testing.T) {
	t.Parallel()
	doc := append([]byte{0xEF, 0xBB, 0xBF}, minimalDoc...)
	cfg, err := Parse(doc, fakeEnv(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ConfigVersion != 1 {
		t.Errorf("ConfigVersion = %d, want 1", cfg.ConfigVersion)
	}
}

// TestParse_NonUTF8IsRejected: the document must be UTF-8 text.
func TestParse_NonUTF8IsRejected(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte{0xFF, 0xFE, 'a', '=', '1'}, fakeEnv(nil))
	assertConfigError(t, err, "", "file is not valid UTF-8 TOML")
}

func TestLoad_ReadsFileFromDisk(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "jevlar.toml")
	if err := os.WriteFile(path, []byte(minimalDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Listen != "127.0.0.1:18080" {
		t.Errorf("Listen = %q", cfg.Server.Listen)
	}
}

func TestLoad_MissingFileIsAnError(t *testing.T) {
	t.Parallel()
	_, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var ce ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a ConfigError", err)
	}
	if !strings.Contains(err.Error(), "read config file") {
		t.Errorf("error %q does not mention the read failure", err)
	}
}

// TestLoad_MissingFileUnwrapsToErrNotExist pins the wrapping contract: the
// cause stays reachable through the ConfigError chain.
func TestLoad_MissingFileUnwrapsToErrNotExist(t *testing.T) {
	t.Parallel()
	_, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("errors.Is(err, fs.ErrNotExist) = false for %v", err)
	}
}

// TestLoad_RejectsNonRegularFile: a directory (or FIFO) is configuration in
// no sense; reject it with a clear message instead of a read error.
func TestLoad_RejectsNonRegularFile(t *testing.T) {
	t.Parallel()
	_, err := Load(t.TempDir())
	assertConfigError(t, err, "", "is not a regular file")
}

// TestLoad_WithLookupInjectsTheEnvironment: Load's lookup is injectable so
// callers (and tests) never depend on the process environment.
func TestLoad_WithLookupInjectsTheEnvironment(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(minimalDoc, "127.0.0.1:18080", "${LISTEN}", 1)
	path := filepath.Join(t.TempDir(), "jevlar.toml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, WithLookup(fakeEnv(map[string]string{"LISTEN": "127.0.0.1:19090"})))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Listen != "127.0.0.1:19090" {
		t.Errorf("Listen = %q, want 127.0.0.1:19090", cfg.Server.Listen)
	}
}

func TestConfigError_ErrorRendersFieldPath(t *testing.T) {
	t.Parallel()
	if got, want := (ConfigError{Field: `providers[1].name`, Msg: `duplicate "x"`}).Error(),
		`providers[1].name: duplicate "x"`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got, want := (ConfigError{Msg: "boom"}).Error(), "boom"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestConfigError_UnwrapExposesTheCause pins the wrapping contract.
func TestConfigError_UnwrapExposesTheCause(t *testing.T) {
	t.Parallel()
	cause := errors.New("inner")
	ce := ConfigError{Msg: "read config file: inner", Err: cause}
	if !errors.Is(ce, cause) {
		t.Fatalf("errors.Is(ce, cause) = false for %v", ce)
	}
}

// TestExpandValue_MapValuesAreExpanded exercises map handling: map values
// are not addressable in place, so the map is rebuilt with expanded values.
func TestExpandValue_MapValuesAreExpanded(t *testing.T) {
	t.Parallel()
	type doc struct {
		Labels map[string]string `toml:"labels"`
	}
	d := &doc{Labels: map[string]string{"upstream": "${HOST}", "plain": "direct"}}
	var errs []ConfigError
	expandValue(reflect.ValueOf(d).Elem(), "", fakeEnv(map[string]string{"HOST": "h"}), &errs)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if got := d.Labels["upstream"]; got != "h" {
		t.Errorf("Labels[upstream] = %q, want %q", got, "h")
	}
	if got := d.Labels["plain"]; got != "direct" {
		t.Errorf("Labels[plain] = %q, want %q (must be preserved)", got, "direct")
	}
}

// TestExpandValue_MapExpansionErrorIsAnchoredToTheKeyPath: a failing
// value inside a map reports the map's field path plus the TOML-quoted
// key, matching the walk's display style for map-keyed tables.
func TestExpandValue_MapExpansionErrorIsAnchoredToTheKeyPath(t *testing.T) {
	t.Parallel()
	type doc struct {
		Labels map[string]string `toml:"labels"`
	}
	d := &doc{Labels: map[string]string{"upstream": "${ABSENT}"}}
	var errs []ConfigError
	expandValue(reflect.ValueOf(d).Elem(), "", fakeEnv(nil), &errs)
	if len(errs) != 1 || errs[0].Field != `labels."upstream"` {
		t.Fatalf("got %v, want one error anchored to labels.\"upstream\"", errs)
	}
}

// TestExpandValue_NilMapStaysNil: a nil map has nothing to expand and
// must not be replaced by an empty one.
func TestExpandValue_NilMapStaysNil(t *testing.T) {
	t.Parallel()
	type doc struct {
		Labels map[string]string `toml:"labels"`
	}
	d := &doc{}
	var errs []ConfigError
	expandValue(reflect.ValueOf(d).Elem(), "", fakeEnv(nil), &errs)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if d.Labels != nil {
		t.Fatalf("Labels = %v, want nil", d.Labels)
	}
}

// TestExpandValue_InterfaceKindIsRejected: interface-typed fields cannot be
// walked; they are reported instead of silently skipped.
func TestExpandValue_InterfaceKindIsRejected(t *testing.T) {
	t.Parallel()
	type doc struct {
		Extra any `toml:"extra"`
	}
	d := &doc{Extra: "x"}
	var errs []ConfigError
	expandValue(reflect.ValueOf(d).Elem(), "", fakeEnv(nil), &errs)
	if len(errs) != 1 || errs[0].Field != "extra" || !strings.Contains(errs[0].Msg, "unsupported field kind") {
		t.Fatalf("got %v, want extra: unsupported field kind", errs)
	}
}

// TestExpandValue_UnsettableValueIsReportedNotPanicked: expansion of an
// unsettable (unaddressable) string reports an error instead of panicking.
func TestExpandValue_UnsettableValueIsReportedNotPanicked(t *testing.T) {
	t.Parallel()
	var errs []ConfigError
	expandValue(reflect.ValueOf("${V}"), "f", fakeEnv(map[string]string{"V": "x"}), &errs)
	if len(errs) != 1 || !strings.Contains(errs[0].Msg, "cannot expand unsettable field") {
		t.Fatalf("got %v, want cannot-expand-unsettable error", errs)
	}
}

// TestExpandValue_UnsettableMapIsReportedNotPanicked: same guard for maps.
func TestExpandValue_UnsettableMapIsReportedNotPanicked(t *testing.T) {
	t.Parallel()
	var errs []ConfigError
	expandValue(reflect.ValueOf(map[string]string{"a": "${V}"}), "m", fakeEnv(map[string]string{"V": "x"}), &errs)
	if len(errs) != 1 || !strings.Contains(errs[0].Msg, "cannot expand unsettable field") {
		t.Fatalf("got %v, want cannot-expand-unsettable error", errs)
	}
}

// assertConfigError asserts that err is non-nil and one of its ConfigError
// parts matches field and contains msg.
func assertConfigError(t *testing.T, err error, field, msg string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error with field %q, got nil", field)
	}
	for _, ce := range asConfigErrors(t, err) {
		if ce.Field == field && strings.Contains(ce.Msg, msg) {
			return
		}
	}
	t.Fatalf("error %v does not contain ConfigError{Field: %q, Msg containing %q}", err, field, msg)
}
