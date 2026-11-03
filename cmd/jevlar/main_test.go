package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/amscotti/jevlar/internal/testutil"
)

// TestMain removes the shared e2e binary so test runs leave no temporary
// artefacts behind.
func TestMain(m *testing.M) {
	code := m.Run()
	testutil.CleanupBuiltBinary()
	os.Exit(code)
}

// TestJevlar_CheckFlagValidatesGoodConfigAndExitsZero runs the binary with
// --check against configs/minimal.toml and expects exit 0.
func TestJevlar_CheckFlagValidatesGoodConfigAndExitsZero(t *testing.T) {
	t.Parallel()
	bin := testutil.JevlarBinary(t)
	out, code := runJevlar(t, bin, "--check", "--config", repoPath(t, "configs/minimal.toml"))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
	}
	if !strings.Contains(out, "configuration is valid") {
		t.Errorf("output missing confirmation:\n%s", out)
	}
}

// TestJevlar_DuplicateProviderNameExitsTwoWithFieldPath: a config with a
// duplicate provider name must exit 2 and print the field path of the later
// occurrence.
func TestJevlar_DuplicateProviderNameExitsTwoWithFieldPath(t *testing.T) {
	t.Parallel()
	bin := testutil.JevlarBinary(t)
	port := testutil.FreeLoopbackPort(t)
	doc := fmt.Sprintf(`
config_version = 1

[server]
listen = "127.0.0.1:%d"

[[providers]]
name = "dup"
base_url = "http://127.0.0.1:9"
path = "/v1/systemone"
auth = "none"

[[providers]]
name = "dup"
base_url = "http://127.0.0.1:9"
path = "/v1/systemone"
auth = "none"
`, port)
	cfg := writeConfig(t, doc)

	out, code := runJevlar(t, bin, "--check", "--config", cfg)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; output:\n%s", code, out)
	}
	if !strings.Contains(out, `providers[1].name: duplicate "dup"`) {
		t.Errorf("output missing field-path error:\n%s", out)
	}
}

// TestJevlar_InvalidConfigNeverBindsThePort proves the boot ordering, not
// just the exit code. The test pre-binds a port and keeps it bound for the
// whole test, then:
//
// (a) an invalid config on that port exits 2 with the validation message and
//
//	never reports a bind error — validation rejected the configuration
//	before any bind was attempted;
//
// (b) a valid config on the same pre-bound port exits 1 with the bind error
//
//	— the boot path reaches the bind only after validation passes.
//
// Before the ordering was asserted, a binary that bound first and validated
// later could pass a mere exit-code check: (b) is what makes (a) meaningful.
func TestJevlar_InvalidConfigNeverBindsThePort(t *testing.T) {
	t.Parallel()
	bin := testutil.JevlarBinary(t)

	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-bind port: %v", err)
	}
	t.Cleanup(func() { _ = held.Close() })
	port := held.Addr().(*net.TCPAddr).Port

	badDoc := fmt.Sprintf(`
config_version = 7

[server]
listen = "127.0.0.1:%d"

[[providers]]
name = "solo"
base_url = "http://127.0.0.1:9"
path = "/v1/systemone"
auth = "none"
`, port)
	goodDoc := fmt.Sprintf(`
config_version = 1

[server]
listen = "127.0.0.1:%d"

[[providers]]
name = "solo"
base_url = "http://127.0.0.1:9"
path = "/v1/systemone"
auth = "none"
`, port)

	// (a) invalid config: exit 2, the validation message, and no bind error.
	out, code := runJevlar(t, bin, "--config", writeConfig(t, badDoc))
	if code != 2 {
		t.Fatalf("(a) exit code = %d, want 2; output:\n%s", code, out)
	}
	if !strings.Contains(out, "config_version") {
		t.Errorf("(a) output missing field path:\n%s", out)
	}
	if strings.Contains(out, "address already in use") {
		t.Errorf("(a) output reports a bind error; validation must run before the bind:\n%s", out)
	}

	// (b) valid config on the same held port: exit 1 with the bind error.
	out, code = runJevlar(t, bin, "--config", writeConfig(t, goodDoc))
	if code != 1 {
		t.Fatalf("(b) exit code = %d, want 1; output:\n%s", code, out)
	}
	if !strings.Contains(out, "address already in use") {
		t.Errorf("(b) output missing the bind error:\n%s", out)
	}
}

// TestJevlar_ServesHealthAndMetricsAndExitsCleanlyOnSIGTERM is the end-to-end
// smoke test: boot against a config pointing at a fake upstream, verify
// /healthz and /metrics, SIGTERM, expect exit 0 within the grace period.
func TestJevlar_ServesHealthAndMetricsAndExitsCleanlyOnSIGTERM(t *testing.T) {
	upstream := testutil.NewFakeUpstream(t)
	port := testutil.FreeLoopbackPort(t)
	doc := fmt.Sprintf(`
config_version = 1

[server]
listen = "127.0.0.1:%d"
shutdown_grace_ms = 2000

[[providers]]
name = "fake"
base_url = "%s"
path = "/v1/systemone"
auth = "none"
models = ["jev-latest"]
`, port, upstream.URL())
	cfg := writeConfig(t, doc)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	bin := testutil.JevlarBinary(t)
	cmd := exec.CommandContext(ctx, bin, "--config", cfg)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Single-owner reap: exactly one goroutine waits for the process, started
	// immediately after Start, on every path the test can take. Nothing ever
	// calls Process.Wait directly, so the process can only be reaped once.
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	// kill kills at most once; the wait goroutine above does the reaping.
	var killOnce sync.Once
	kill := func() {
		killOnce.Do(func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		})
	}
	t.Cleanup(kill)
	// killAndWait is the failure-path discipline: stop the process and let
	// the reaper finish before reading stderr, so the builder is not read
	// while the process still writes to it.
	killAndWait := func() {
		kill()
		select {
		case err := <-waitDone:
			_ = err // reaped; the failure path reports its own error below
		case <-time.After(5 * time.Second):
		}
	}

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := testutil.WaitForHealthy(t, base+"/healthz", 10*time.Second); err != nil {
		killAndWait()
		t.Fatalf("healthz never became healthy: %v (stderr: %s)", err, stderr.String())
	}

	healthBody := fetch(t, base+"/healthz")
	if got := strings.TrimSpace(healthBody); got != `{"status":"ok"}` {
		t.Errorf("healthz body = %q, want {\"status\":\"ok\"}", got)
	}
	if !strings.Contains(fetch(t, base+"/metrics"), "jevlar_build_info") {
		t.Errorf("metrics body lacks jevlar_build_info")
	}

	start := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		killAndWait()
		t.Fatalf("signal: %v", err)
	}
	select {
	case err := <-waitDone:
		killOnce.Do(func() {}) // process reaped; disarm the cleanup kill
		if err != nil {
			t.Fatalf("exit after SIGTERM not clean: %v (stderr: %s)", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		killAndWait()
		t.Fatal("process did not exit within 10s of SIGTERM")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("shutdown took %v, want well within the 2s grace", elapsed)
	}
}

// TestJevlar_VersionFlagPrintsVersionAndExitsZero checks the --version flag
// end to end.
func TestJevlar_VersionFlagPrintsVersionAndExitsZero(t *testing.T) {
	t.Parallel()
	bin := testutil.JevlarBinary(t)
	out, code := runJevlar(t, bin, "--version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
	}
	if !strings.HasPrefix(out, "jevlar ") {
		t.Errorf("output %q does not start with version line", out)
	}
}

// TestRun_FlagMatrix exercises run in-process: the flag surface and exit
// codes without spawning processes. --check keeps every case off the serve
// path, which the SIGTERM test above owns.
func TestRun_FlagMatrix(t *testing.T) {
	good := writeConfig(t, `
config_version = 1

[server]
listen = "127.0.0.1:1"

[[providers]]
name = "solo"
base_url = "http://127.0.0.1:9"
path = "/v1/systemone"
auth = "none"
`)
	bad := writeConfig(t, "config_version = 7\n")
	cases := []struct {
		name     string
		args     []string
		wantExit int
		wantOut  string
		wantErr  string
	}{
		{name: "check good config", args: []string{"--check", "--config", good}, wantExit: exitOK, wantOut: "configuration is valid"},
		{name: "check bad config", args: []string{"--check", "--config", bad}, wantExit: exitConfig, wantErr: "config_version"},
		{name: "version", args: []string{"--version"}, wantExit: exitOK, wantOut: "jevlar "},
		// --version wins over --check.
		{name: "version beats check", args: []string{"--version", "--check"}, wantExit: exitOK, wantOut: "jevlar "},
		{name: "unknown flag", args: []string{"--nope"}, wantExit: exitUsage, wantErr: "flag provided but not defined"},
		{name: "positional args rejected", args: []string{"extra"}, wantExit: exitUsage, wantErr: "unexpected argument"},
		{name: "config required", args: nil, wantExit: exitUsage, wantErr: "--config is required"},
		{name: "missing file", args: []string{"--config", filepath.Join(t.TempDir(), "absent.toml")}, wantExit: exitConfig, wantErr: "read config file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			code := run(tc.args, &stdout, &stderr)
			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d; stderr:\n%s", code, tc.wantExit, stderr.String())
			}
			if tc.wantOut != "" && !strings.Contains(stdout.String(), tc.wantOut) {
				t.Errorf("stdout %q does not contain %q", stdout.String(), tc.wantOut)
			}
			if tc.wantErr != "" && !strings.Contains(stderr.String(), tc.wantErr) {
				t.Errorf("stderr %q does not contain %q", stderr.String(), tc.wantErr)
			}
		})
	}
}

// TestRun_VersionFlagPrintsInjectedVersion: run reports the version variable
// that -ldflags "-X main.version=..." injects at build time, so a non-"dev"
// build prints a non-"dev" version.
func TestRun_VersionFlagPrintsInjectedVersion(t *testing.T) {
	// Mutates the package-level version; must not run in parallel.
	old := version
	version = "v0.0.1-testbuild"
	t.Cleanup(func() { version = old })

	var stdout strings.Builder
	if code := run([]string{"--version"}, &stdout, io.Discard); code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stdout.String(), "v0.0.1-testbuild") {
		t.Fatalf("stdout %q does not contain the injected version", stdout.String())
	}
}

// runJevlar execs the built binary with a bounded context and returns its
// combined output and exit code.
func runJevlar(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("run %s %v: %v; output:\n%s", bin, args, err, out)
	return "", -1
}

// fetchClient bounds every fetch so a hung socket cannot stall the test.
var fetchClient = &http.Client{Timeout: 2 * time.Second}

// fetch gets url with the test's context and the bounded client, and
// returns the body.
func fetch(t *testing.T, url string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request %s: %v", url, err)
	}
	resp, err := fetchClient.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // body is fully read below
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get %s: status %d", url, resp.StatusCode)
	}
	return string(body)
}

// writeConfig writes doc to a fresh temp file and returns its path.
func writeConfig(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jevlar.toml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// repoPath returns a path inside this repository.
func repoPath(t *testing.T, rel string) string {
	t.Helper()
	return filepath.Join(testutil.ModuleRoot(t), rel)
}

// TestRun_KeyhashMatchesIndependentComputation: the printed digest equals
// a SHA-256 computed independently in the test.
func TestRun_KeyhashMatchesIndependentComputation(t *testing.T) {
	t.Parallel()
	token := "my-inbound-client-token"
	sum := sha256.Sum256([]byte(token))
	want := hex.EncodeToString(sum[:])

	var stdout strings.Builder
	if code := run([]string{"keyhash", token}, &stdout, io.Discard); code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}
	got := strings.TrimSpace(stdout.String())
	if got != want {
		t.Fatalf("keyhash = %q, want %q", got, want)
	}
	if len(got) != 64 {
		t.Fatalf("keyhash output length = %d, want 64", len(got))
	}
}

// TestRun_KeyhashUsageErrors: a missing token or extra arguments are
// usage errors (exit 2); the usage line names the subcommand.
func TestRun_KeyhashUsageErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
	}{
		{name: "missing token", args: []string{"keyhash"}},
		{name: "extra argument", args: []string{"keyhash", "a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stderr strings.Builder
			if code := run(tc.args, io.Discard, &stderr); code != exitUsage {
				t.Fatalf("exit = %d, want %d; stderr %q", code, exitUsage, stderr.String())
			}
			if !strings.Contains(stderr.String(), "usage: jevlar keyhash") {
				t.Errorf("stderr %q lacks the usage line", stderr.String())
			}
		})
	}
}

// TestRun_KeyhashNeverEchoesTheToken: neither stdout nor stderr ever
// carries the token — only its digest.
func TestRun_KeyhashNeverEchoesTheToken(t *testing.T) {
	t.Parallel()
	token := "secret-token-never-echo-me"
	var stdout, stderr strings.Builder
	if code := run([]string{"keyhash", token}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(stdout.String()+stderr.String(), token) {
		t.Fatalf("token echoed; stdout %q stderr %q", stdout.String(), stderr.String())
	}
}

// TestJevlar_KeyhashViaBinary runs the built binary's subcommand and
// checks the digest end to end.
func TestJevlar_KeyhashViaBinary(t *testing.T) {
	t.Parallel()
	bin := testutil.JevlarBinary(t)
	out, code := runJevlar(t, bin, "keyhash", "e2e-token")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; output:\n%s", code, out)
	}
	sum := sha256.Sum256([]byte("e2e-token"))
	if got := strings.TrimSpace(out); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("output = %q, want %q", got, hex.EncodeToString(sum[:]))
	}

	out, code = runJevlar(t, bin, "keyhash")
	if code != 2 {
		t.Fatalf("missing-arg exit = %d, want 2; output:\n%s", code, out)
	}
}

// TestJevlar_CheckAcceptsTheFullExampleConfig: --check on the committed
// example (with its references resolved: env for the ${VAR}s, a temp file
// for the documented secret mount) exits 0.
func TestJevlar_CheckAcceptsTheFullExampleConfig(t *testing.T) {
	t.Parallel()
	bin := testutil.JevlarBinary(t)

	hashOf := func(token string) string {
		sum := sha256.Sum256([]byte(token))
		return hex.EncodeToString(sum[:])
	}
	secret := filepath.Join(t.TempDir(), "typesafe_secondary")
	if err := os.WriteFile(secret, []byte("secondary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(repoPath(t, "configs/example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.Replace(string(data), "/run/secrets/typesafe_secondary", secret, 1)
	cfgPath := writeConfig(t, doc)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--check", "--config", cfgPath)
	cmd.Env = append(os.Environ(),
		"JEVLAR_APP_PROD_HASH="+hashOf("app-prod-token"),
		"JEVLAR_BATCH_HASH="+hashOf("batch-token"),
		"TYPESAFE_API_KEY=ts-example-key",
		"UPSTAGE_API_KEY=up-example-key",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--check on the example failed: %v; output:\n%s", err, out)
	}
	if !strings.Contains(string(out), "configuration is valid") {
		t.Fatalf("output missing confirmation:\n%s", out)
	}
}

// TestJevlar_CheckRejectsTheFullExampleConfigWithoutEnv: without the
// environment the example's required references fail --check with field
// paths and exit 2 — fail-closed, not half-valid.
func TestJevlar_CheckRejectsTheFullExampleConfigWithoutEnv(t *testing.T) {
	t.Parallel()
	bin := testutil.JevlarBinary(t)
	cfgPath := repoPath(t, "configs/example.toml")

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--check", "--config", cfgPath)
	// A minimal static environment: the test must not read the real one
	// (environment access belongs to internal/config) and the point of
	// this case is that no Jevlar variables are set.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/tmp"}
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("--check without env: err = %v, want exit 2; output:\n%s", err, out)
	}
	if !strings.Contains(string(out), "TYPESAFE_API_KEY") {
		t.Fatalf("output missing the missing-variable field path:\n%s", out)
	}
}

// TestJevlar_SIGHUPReloadsTheRunningServer sends a real SIGHUP to the
// running binary: /admin/status reports the new configuration hash and
// provider after the reload, and SIGTERM still exits cleanly.
func TestJevlar_SIGHUPReloadsTheRunningServer(t *testing.T) {
	upstream := testutil.NewFakeUpstream(t)
	mainPort := testutil.FreeLoopbackPort(t)
	adminPort := testutil.FreeLoopbackPort(t)

	doc := func(timeoutMs int) string {
		return fmt.Sprintf(`
config_version = 1

[server]
listen = "127.0.0.1:%d"
admin_listen = "127.0.0.1:%d"
shutdown_grace_ms = 2000

[[providers]]
name = "fake"
base_url = "%s"
path = "/v1/systemone"
auth = "none"
timeout_ms = %d
`, mainPort, adminPort, upstream.URL(), timeoutMs)
	}
	cfgPath := writeConfig(t, doc(8000))

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	bin := testutil.JevlarBinary(t)
	cmd := exec.CommandContext(ctx, bin, "--config", cfgPath)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	var killOnce sync.Once
	kill := func() {
		killOnce.Do(func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		})
	}
	t.Cleanup(kill)
	killAndWait := func() {
		kill()
		select {
		case err := <-waitDone:
			_ = err // reaped; the failure path reports its own error below
		case <-time.After(5 * time.Second):
		}
	}

	adminBase := fmt.Sprintf("http://127.0.0.1:%d", adminPort)
	mainBase := fmt.Sprintf("http://127.0.0.1:%d", mainPort)
	if err := testutil.WaitForHealthy(t, adminBase+"/admin/status", 10*time.Second); err != nil {
		killAndWait()
		t.Fatalf("admin status never answered: %v (stderr: %s)", err, stderr.String())
	}

	fetchHash := func() string {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, adminBase+"/admin/status", nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := fetchClient.Do(req)
		if err != nil {
			t.Fatalf("get status: %v", err)
		}
		defer resp.Body.Close() //nolint:errcheck // read below
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read status: %v", err)
		}
		var status struct {
			ConfigHash string `json:"config_hash"`
		}
		if err := json.Unmarshal(body, &status); err != nil {
			t.Fatalf("status body: %v\n%s", err, body)
		}
		return status.ConfigHash
	}

	oldHash := fetchHash()

	// Rewrite the configuration (a changed timeout changes the hash) and
	// send the real signal to the running process.
	if err := os.WriteFile(cfgPath, []byte(doc(4000)), 0o600); err != nil {
		killAndWait()
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		killAndWait()
		t.Fatalf("SIGHUP: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	newHash := oldHash
	for time.Now().Before(deadline) {
		newHash = fetchHash()
		if newHash != oldHash {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if newHash == oldHash {
		killAndWait()
		t.Fatalf("config hash did not change after SIGHUP (stderr: %s)", stderr.String())
	}

	// The main listener served throughout.
	if err := testutil.WaitForHealthy(t, mainBase+"/healthz", 5*time.Second); err != nil {
		killAndWait()
		t.Fatalf("main listener did not answer after reload: %v (stderr: %s)", err, stderr.String())
	}

	// A SIGHUP with a now-invalid file keeps the server alive; the
	// failure counter appears in /metrics.
	if err := os.WriteFile(cfgPath, []byte("config_version = 2\n"), 0o600); err != nil {
		killAndWait()
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		killAndWait()
		t.Fatalf("second SIGHUP: %v", err)
	}
	deadline = time.Now().Add(10 * time.Second)
	sawFailure := false
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, mainBase+"/metrics", nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := fetchClient.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if strings.Contains(string(body), "jevlar_config_reload_failures_total 1") {
				sawFailure = true
			}
		}
		if sawFailure {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sawFailure {
		killAndWait()
		t.Fatalf("reload-failure counter never appeared (stderr: %s)", stderr.String())
	}
	if got := fetchHash(); got != newHash {
		killAndWait()
		t.Fatalf("hash changed after a failed reload: %q (want %q)", got, newHash)
	}

	// Clean exit on SIGTERM.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		killAndWait()
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case err := <-waitDone:
		killOnce.Do(func() {})
		if err != nil {
			t.Fatalf("exit after SIGTERM not clean: %v (stderr: %s)", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		killAndWait()
		t.Fatal("process did not exit within 10s of SIGTERM")
	}
	if !strings.Contains(stderr.String(), "config reload failed") {
		t.Errorf("stderr lacks the reload-failure log (stderr: %s)", stderr.String())
	}
}

// TestJevlar_HealthcheckSubcommand pins the container probe: exit 0 while
// the listen address answers /healthz, non-zero once it does not.
func TestJevlar_HealthcheckSubcommand(t *testing.T) {
	t.Parallel()
	bin := testutil.JevlarBinary(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	doc := fmt.Sprintf(`
config_version = 1

[server]
listen = %q

[[providers]]
name = "loop"
base_url = "http://127.0.0.1:9"
path = "/v1/systemone"
auth = "none"
`, u.Host)
	path := writeConfig(t, doc)

	out, code := runJevlar(t, bin, "healthcheck", "--config", path)
	if code != 0 {
		t.Fatalf("healthcheck exit = %d, want 0; output:\n%s", code, out)
	}

	srv.Close()
	out, code = runJevlar(t, bin, "healthcheck", "--config", path)
	if code == 0 {
		t.Fatalf("healthcheck against a closed listener exited 0; output:\n%s", out)
	}
}
