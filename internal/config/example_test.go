package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exampleEnv is the environment the committed example configuration needs:
// the two inbound-key hashes must be valid 64-hex values (the file pins
// their shape) and the two provider key references must be set.
func exampleEnv() map[string]string {
	hash := func(token string) string {
		sum := sha256.Sum256([]byte(token))
		return hex.EncodeToString(sum[:])
	}
	return map[string]string{
		"JEVLAR_APP_PROD_HASH": hash("app-prod-token"),
		"JEVLAR_BATCH_HASH":    hash("batch-token"),
		"TYPESAFE_API_KEY":     "ts-example-key",
		"UPSTAGE_API_KEY":      "up-example-key",
	}
}

// loadExample reads the committed example configuration and rewrites the
// one machine-specific secret-mount path (/run/secrets/...) to a temp file
// this test provides; everything else is the committed file verbatim.
func loadExample(t *testing.T) (*Config, error) {
	t.Helper()
	data, err := os.ReadFile("../../configs/example.toml")
	if err != nil {
		t.Fatalf("read example.toml: %v", err)
	}
	secret := filepath.Join(t.TempDir(), "typesafe_secondary")
	if err := os.WriteFile(secret, []byte("secondary-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := strings.Replace(string(data), "/run/secrets/typesafe_secondary", secret, 1)
	return Parse([]byte(doc), fakeEnv(exampleEnv()))
}

// TestExampleConfigValidatesAsIs: the committed example — the complete v1
// key surface — loads, resolves and validates without a single error and
// without warnings.
func TestExampleConfigValidatesAsIs(t *testing.T) {
	t.Parallel()
	cfg, err := loadExample(t)
	if err != nil {
		t.Fatalf("example.toml does not load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("example.toml does not validate: %v", err)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("example.toml produced warnings: %v", cfg.Warnings)
	}

	// Spot-check the resolved surface: expansion ran, files were read and
	// defaults landed where the file omits values.
	if cfg.Server.Listen != "127.0.0.1:8080" {
		t.Errorf("listen = %q, want the expanded default", cfg.Server.Listen)
	}
	if got := cfg.Providers[0].APIKeys[0].Resolved; got != "ts-example-key" {
		t.Errorf("typesafe primary key = %q, want the expanded env value", got)
	}
	if got := cfg.Providers[0].APIKeys[1].Resolved; got != "secondary-key" {
		t.Errorf("typesafe secondary key = %q, want the file contents", got)
	}
	if got := cfg.Providers[1].Name; got != "ollama-local" {
		t.Errorf("providers[1].name = %q, want ollama-local", got)
	}
	if got := cfg.Models["jev-latest"].Targets[1].Model; got != "nimble" {
		t.Errorf("jev-latest fallback model = %q, want nimble", got)
	}
	if len(cfg.Auth.Keys) != 2 {
		t.Errorf("auth keys = %d, want 2", len(cfg.Auth.Keys))
	}
}

// TestExampleConfigRequiresItsReferences: without the environment the
// example's required references name the missing variables with field
// paths — the fail-closed contract, not a boot that half-worked.
func TestExampleConfigRequiresItsReferences(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../configs/example.toml")
	if err != nil {
		t.Fatalf("read example.toml: %v", err)
	}
	_, err = Parse(data, fakeEnv(nil))
	if err == nil {
		t.Fatal("example parsed with an empty environment, want missing-variable errors")
	}
	var verrs ValidationErrors
	if !asValidation(err, &verrs) {
		t.Fatalf("err = %v (%T), want ValidationErrors", err, err)
	}
	for _, want := range []string{
		`auth.keys[0].hash: variable "JEVLAR_APP_PROD_HASH" is not set`,
		`providers[0].api_keys[0].value: variable "TYPESAFE_API_KEY" is not set`,
	} {
		found := false
		for _, ce := range verrs {
			if ce.Error() == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %q in:\n%v", want, verrs)
		}
	}
}

// TestExampleConfigSecretMountUnchanged guards the substitute the tests
// make: the committed file must keep naming the documented mount path, so
// the rewrite in loadExample stays honest.
func TestExampleConfigSecretMountUnchanged(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../configs/example.toml")
	if err != nil {
		t.Fatalf("read example.toml: %v", err)
	}
	if !strings.Contains(string(data), fmt.Sprintf("%q", "/run/secrets/typesafe_secondary")) {
		t.Fatal("example.toml no longer references /run/secrets/typesafe_secondary; update the example tests")
	}
}
