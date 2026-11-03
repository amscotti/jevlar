package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// secretDoc is a document with one bearer provider whose key comes from a
// file; tests rewrite the api_keys line per case.
const secretDoc = `
config_version = 1

[server]
listen = "127.0.0.1:18080"

[[providers]]
name = "typesafe"
base_url = "https://api.typesafe.ai"
path = "/v1/systemone"
auth = "bearer"
%s
`

// writeSecretFile writes data to a fresh file in dir with the given mode
// and returns its path.
func writeSecretFile(t *testing.T, dir, data string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// parseDoc parses doc with the standard fake environment.
func parseDoc(t *testing.T, doc string) (*Config, error) {
	t.Helper()
	return Parse([]byte(doc), fakeEnv(map[string]string{"TYPESAFE_API_KEY": "ts-secret"}))
}

// TestResolve_ValueFileIsReadAndTrimmedAtParse: the file's contents become
// the key's resolved value with exactly one trailing newline removed, and
// no warning is recorded for a 0600 file.
func TestResolve_ValueFileIsReadAndTrimmedAtParse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeSecretFile(t, dir, "sk-file-contents\n", 0o600)
	cfg, err := parseDoc(t, fmt.Sprintf(secretDoc, `api_keys = [{ id = "primary", value_file = "`+path+`" }]`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.Providers[0].APIKeys[0].Resolved; got != "sk-file-contents" {
		t.Fatalf("Resolved = %q, want sk-file-contents", got)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none", cfg.Warnings)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestResolve_ValueFileTrimsCRLF: a secret file written on Windows must not
// resolve to a key with a stray carriage return.
func TestResolve_ValueFileTrimsCRLF(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeSecretFile(t, dir, "sk-crlf-secret\r\n", 0o600)
	cfg, err := parseDoc(t, fmt.Sprintf(secretDoc, `api_keys = [{ id = "primary", value_file = "`+path+`" }]`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.Providers[0].APIKeys[0].Resolved; got != "sk-crlf-secret" {
		t.Fatalf("Resolved = %q, want sk-crlf-secret", got)
	}
}

// TestResolve_ValueFileMissingIsAFieldPathError.
func TestResolve_ValueFileMissingIsAFieldPathError(t *testing.T) {
	t.Parallel()
	absent := filepath.Join(t.TempDir(), "absent")
	_, err := parseDoc(t, fmt.Sprintf(secretDoc, `api_keys = [{ id = "primary", value_file = "`+absent+`" }]`))
	var verrs ValidationErrors
	if !asValidation(err, &verrs) || len(verrs) != 1 {
		t.Fatalf("err = %v, want one ValidationErrors entry", err)
	}
	if verrs[0].Field != "providers[0].api_keys[0].value_file" || !strings.Contains(verrs[0].Msg, "open:") {
		t.Fatalf("err = %v, want providers[0].api_keys[0].value_file open error", verrs[0])
	}
}

// TestResolve_ValueFileMustBeARegularFile: a directory named as a
// value_file is rejected at the key's field path.
func TestResolve_ValueFileMustBeARegularFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := parseDoc(t, fmt.Sprintf(secretDoc, `api_keys = [{ id = "primary", value_file = "`+dir+`" }]`))
	var verrs ValidationErrors
	if !asValidation(err, &verrs) || len(verrs) != 1 {
		t.Fatalf("err = %v, want one ValidationErrors entry", err)
	}
	if verrs[0].Field != "providers[0].api_keys[0].value_file" || !strings.Contains(verrs[0].Msg, "is not a regular file") {
		t.Fatalf("err = %v, want a not-a-regular-file error", verrs[0])
	}
}

// TestResolve_GroupReadableValueFileWarnsByDefault: a group/world-readable
// key file is a warning, not an error, because container runtimes mount
// secrets at 0444/0644.
func TestResolve_GroupReadableValueFileWarnsByDefault(t *testing.T) {
	t.Parallel()
	path := writeSecretFile(t, t.TempDir(), "sk\n", 0o644)
	cfg, err := parseDoc(t, fmt.Sprintf(secretDoc, `api_keys = [{ id = "primary", value_file = "`+path+`" }]`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.Warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly the permission warning", cfg.Warnings)
	}
	if !strings.Contains(cfg.Warnings[0].Msg, "group/world-accessible") {
		t.Fatalf("warning = %q, want the permission finding", cfg.Warnings[0].Msg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestResolve_StrictFilePermissionsTurnWarningsIntoErrors: the same
// group-readable file is a load error under
// security.strict_file_permissions = true.
func TestResolve_StrictFilePermissionsTurnWarningsIntoErrors(t *testing.T) {
	t.Parallel()
	path := writeSecretFile(t, t.TempDir(), "sk\n", 0o640)
	doc := strings.Replace(secretDoc, "[server]", "[security]\nstrict_file_permissions = true\n\n[server]", 1)
	_, err := parseDoc(t, fmt.Sprintf(doc, `api_keys = [{ id = "primary", value_file = "`+path+`" }]`))
	var verrs ValidationErrors
	if !asValidation(err, &verrs) || len(verrs) != 1 {
		t.Fatalf("err = %v, want one ValidationErrors entry", err)
	}
	if verrs[0].Field != "providers[0].api_keys[0].value_file" || !strings.Contains(verrs[0].Msg, "group/world-accessible") {
		t.Fatalf("err = %v, want the strict permission error", verrs[0])
	}
}

// TestResolve_LiteralValueWarns: an inline key value with no ${...}
// reference warns; a ${VAR} reference does not.
func TestResolve_LiteralValueWarns(t *testing.T) {
	t.Parallel()
	cfg, err := parseDoc(t, fmt.Sprintf(secretDoc, `api_keys = [{ id = "primary", value = "sk-literal" }]`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.Warnings) != 1 || cfg.Warnings[0].Field != "providers[0].api_keys[0].value" {
		t.Fatalf("warnings = %v, want the literal-value warning", cfg.Warnings)
	}
	if cfg.Providers[0].APIKeys[0].Resolved != "sk-literal" {
		t.Fatalf("Resolved = %q, want sk-literal", cfg.Providers[0].APIKeys[0].Resolved)
	}

	cfg, err = parseDoc(t, fmt.Sprintf(secretDoc, `api_keys = [{ id = "primary", value = "${TYPESAFE_API_KEY}" }]`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none for a reference value", cfg.Warnings)
	}
	if cfg.Providers[0].APIKeys[0].Resolved != "ts-secret" {
		t.Fatalf("Resolved = %q, want the expanded value", cfg.Providers[0].APIKeys[0].Resolved)
	}
}

// TestResolve_ValueAndValueFileAreMutuallyExclusive.
func TestResolve_ValueAndValueFileAreMutuallyExclusive(t *testing.T) {
	t.Parallel()
	path := writeSecretFile(t, t.TempDir(), "sk\n", 0o600)
	_, err := parseDoc(t, fmt.Sprintf(secretDoc, `api_keys = [{ id = "primary", value = "x", value_file = "`+path+`" }]`))
	var verrs ValidationErrors
	if !asValidation(err, &verrs) || len(verrs) != 1 {
		t.Fatalf("err = %v, want one ValidationErrors entry", err)
	}
	if verrs[0].Field != "providers[0].api_keys[0]" || verrs[0].Msg != "define either value or value_file, not both" {
		t.Fatalf("err = %v, want the mutual-exclusion error", verrs[0])
	}
}

// TestResolve_KeyWithNeitherValueNorFileIsRejected.
func TestResolve_KeyWithNeitherValueNorFileIsRejected(t *testing.T) {
	t.Parallel()
	_, err := parseDoc(t, fmt.Sprintf(secretDoc, `api_keys = [{ id = "primary" }]`))
	var verrs ValidationErrors
	if !asValidation(err, &verrs) || len(verrs) != 1 {
		t.Fatalf("err = %v, want one ValidationErrors entry", err)
	}
	if verrs[0].Field != "providers[0].api_keys[0]" || verrs[0].Msg != "key must define value or value_file" {
		t.Fatalf("err = %v, want the missing-secret error", verrs[0])
	}
}

// TestLoad_ConfigFilePermissionsFollowTheSamePolicy: a group-readable
// configuration file warns by default and is a load error under strict
// mode. Both findings name the file and its mode.
func TestLoad_ConfigFilePermissionsFollowTheSamePolicy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(strict string, mode os.FileMode) (*Config, error) {
		path := filepath.Join(dir, "jevlar-"+strict+".toml")
		doc := fmt.Sprintf(secretDoc, `api_keys = [{ id = "primary", value = "${TYPESAFE_API_KEY}" }]`)
		if strict != "" {
			doc = strings.Replace(doc, "[server]", "[security]\nstrict_file_permissions = "+strict+"\n\n[server]", 1)
		}
		if err := os.WriteFile(path, []byte(doc), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return Load(path, WithLookup(fakeEnv(map[string]string{"TYPESAFE_API_KEY": "ts-secret"})))
	}

	cfg, err := write("", 0o644)
	if err != nil {
		t.Fatalf("default mode load: %v", err)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0].Msg, "group/world-accessible (0644)") {
		t.Fatalf("warnings = %v, want the file-permission warning", cfg.Warnings)
	}

	if _, err := write("true", 0o644); err == nil {
		t.Fatal("strict mode accepted a group-readable config file, want an error")
	}

	cfg, err = write("true", 0o600)
	if err != nil {
		t.Fatalf("strict mode rejected a 0600 config file: %v", err)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none", cfg.Warnings)
	}
}

// TestParse_MapKeyedTablesValuesAreExpandedKeysAreNot: values inside
// accounts and models expand; the map keys themselves never do, so a key
// literally spelled "${NOPE}" survives untouched without a lookup.
func TestParse_MapKeyedTablesValuesAreExpandedKeysAreNot(t *testing.T) {
	t.Parallel()
	doc := `
config_version = 1

[server]
listen = "127.0.0.1:18080"

[accounts."${NOPE}"]
rpm = 10

[accounts.main]
rpm = 20

[[providers]]
name = "loopback"
base_url = "http://127.0.0.1:${UPSTREAM_PORT}"
path = "/v1/systemone"
auth = "none"
account = "main"

[models."${ALIAS}"]
targets = [{ provider = "loopback", model = "m-${SUFFIX}" }]
`
	cfg, err := Parse([]byte(doc), fakeEnv(map[string]string{"UPSTREAM_PORT": "9", "SUFFIX": "one"}))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := cfg.Accounts["${NOPE}"]; !ok {
		t.Fatalf("accounts keys must not be expanded; got %v", cfg.Accounts)
	}
	if _, ok := cfg.Models["${ALIAS}"]; !ok {
		t.Fatalf("models keys must not be expanded; got %v", cfg.Models)
	}
	if cfg.Providers[0].BaseURL != "http://127.0.0.1:9" {
		t.Errorf("base_url = %q, want expanded", cfg.Providers[0].BaseURL)
	}
	if cfg.Providers[0].Account != "main" {
		t.Errorf("account = %q, want main", cfg.Providers[0].Account)
	}
	if got := cfg.Models["${ALIAS}"].Targets[0].Model; got != "m-one" {
		t.Errorf("target model = %q, want expanded m-one", got)
	}
}

// TestParse_ExpansionErrorInsideMapValueCarriesTheKeyPath: an unset
// variable inside a map-keyed table is anchored to the quoted key.
func TestParse_ExpansionErrorInsideMapValueCarriesTheKeyPath(t *testing.T) {
	t.Parallel()
	doc := `
config_version = 1

[server]
listen = "127.0.0.1:18080"

[accounts."main"]
rpm = 10

[[providers]]
name = "loopback"
base_url = "http://127.0.0.1:9"
path = "/v1/systemone"
auth = "none"

[models."jev-latest"]
targets = [{ provider = "${MISSING_PROVIDER}", model = "m" }]
`
	_, err := Parse([]byte(doc), fakeEnv(nil))
	var verrs ValidationErrors
	if !asValidation(err, &verrs) || len(verrs) != 1 {
		t.Fatalf("err = %v, want one ValidationErrors entry", err)
	}
	want := `models."jev-latest".targets[0].provider`
	if verrs[0].Field != want {
		t.Fatalf("field = %q, want %q", verrs[0].Field, want)
	}
}

// asValidation extracts ValidationErrors from err, reporting whether it
// matched.
func asValidation(err error, target *ValidationErrors) bool {
	if err == nil {
		return false
	}
	verrs, ok := err.(ValidationErrors) //nolint:errorlint // narrow internal helper; the type is the package's own
	if !ok {
		return false
	}
	*target = verrs
	return true
}
