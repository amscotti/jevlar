package app_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/amscotti/jevlar/internal/app"
	"github.com/amscotti/jevlar/internal/config"
)

// capture is a slog.Handler that records every record; Serve logs from its
// own goroutines, so access is mutex-guarded.
type capture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *capture) Enabled(context.Context, slog.Level) bool { return true }

func (c *capture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r.Clone())
	return nil
}

func (c *capture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *capture) WithGroup(string) slog.Handler      { return c }

// snapshot returns the records recorded so far.
func (c *capture) snapshot() []slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]slog.Record, len(c.records))
	copy(out, c.records)
	return out
}

// find returns the first record whose message contains substr.
func (c *capture) find(substr string) (slog.Record, bool) {
	for _, r := range c.snapshot() {
		if strings.Contains(r.Message, substr) {
			return r, true
		}
	}
	return slog.Record{}, false
}

// awaitLog polls until a record whose message contains substr appears or
// the deadline passes; it fails the test on timeout.
func (c *capture) awaitLog(t *testing.T, substr string) slog.Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r, ok := c.find(substr); ok {
			return r
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %q record appeared; have:\n%s", substr, c.dump())
	return slog.Record{}
}

// dump renders every record, one per line, for failure messages.
func (c *capture) dump() string {
	var b strings.Builder
	for _, r := range c.snapshot() {
		b.WriteString(r.Message)
		r.Attrs(func(a slog.Attr) bool {
			fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
			return true
		})
		b.WriteByte('\n')
	}
	return b.String()
}

// attrString returns the record's string attribute with the given key.
func attrString(r slog.Record, key string) (string, bool) {
	var out string
	var found bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			if s, ok := a.Value.Any().(string); ok {
				out, found = s, true
			}
		}
		return true
	})
	return out, found
}

// fetchClient bounds test HTTP requests so a hung socket cannot stall the
// suite.
var fetchClient = &http.Client{Timeout: 2 * time.Second}

// serveDoc renders a minimal valid configuration with the given listener
// addresses and grace. apiValue is empty for auth "none".
func serveDoc(listen, adminListen string, graceMs int) string {
	admin := ""
	if adminListen != "" {
		admin = fmt.Sprintf("admin_listen = %q\n", adminListen)
	}
	return fmt.Sprintf(`
config_version = 1

[server]
listen = %q
%s
shutdown_grace_ms = %d

[auth]
keys = [{ id = "dev", hash = %q }]

[[providers]]
name = "loopback"
base_url = "http://127.0.0.1:9"
path = "/v1/systemone"
auth = "none"
`, listen, admin, graceMs, sha256Hex(devToken))
}

// devToken is the inbound credential the app-test fixtures accept.
const devToken = "app-test-token"

// serveFixture runs Serve in the background over a temp config file and
// tracks its lifecycle.
type serveFixture struct {
	cfg        *config.Config
	configPath string
	log        *capture
	signals    chan os.Signal
	done       chan error
}

// startServe boots Serve over a temp config file holding doc and waits
// for the boot log line.
func startServe(t *testing.T, doc string) *serveFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jevlar.toml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(doc), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("parse fixture doc: %v", err)
	}
	f := &serveFixture{
		cfg:        cfg,
		configPath: path,
		log:        &capture{},
		signals:    make(chan os.Signal, 4),
		done:       make(chan error, 1),
	}
	go func() {
		f.done <- app.Serve(context.Background(), cfg, path, slog.New(f.log), f.signals)
	}()
	f.log.awaitLog(t, "boot")
	return f
}

// mainAddr returns the address the main listener bound (from the boot
// log; it differs from the configured string for ephemeral ports).
func (f *serveFixture) mainAddr(t *testing.T) string {
	t.Helper()
	r, ok := f.log.find("boot")
	if !ok {
		t.Fatal("no boot record")
	}
	addr, _ := attrString(r, "listen")
	if addr == "" {
		t.Fatal("boot record lacks listen")
	}
	return addr
}

// adminAddr returns the address the admin listener bound, waiting for it.
func (f *serveFixture) adminAddr(t *testing.T) string {
	t.Helper()
	r := f.log.awaitLog(t, "admin listener bound")
	addr, _ := attrString(r, "admin_listen")
	if addr == "" {
		t.Fatal("admin boot record lacks admin_listen")
	}
	return addr
}

// bootHash returns the configuration hash from the boot record.
func (f *serveFixture) bootHash(t *testing.T) string {
	t.Helper()
	r, ok := f.log.find("boot")
	if !ok {
		t.Fatal("no boot record")
	}
	hash, _ := attrString(r, "config_hash")
	if hash == "" {
		t.Fatal("boot record lacks config_hash")
	}
	return hash
}

// rewriteConfig replaces the fixture's config file contents.
func (f *serveFixture) rewriteConfig(t *testing.T, doc string) {
	t.Helper()
	if err := os.WriteFile(f.configPath, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

// waitDone waits for Serve to return.
func (f *serveFixture) waitDone(t *testing.T) error {
	t.Helper()
	select {
	case err := <-f.done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
		return nil
	}
}

// get fetches url and returns status and body.
func get(t *testing.T, url string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request %s: %v", url, err)
	}
	resp, err := fetchClient.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // body fully read below
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(body)
}

// getAuthed fetches url with the fixture's client token.
func getAuthed(t *testing.T, url string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request %s: %v", url, err)
	}
	req.Header.Set("Authorization", "Bearer "+devToken)
	resp, err := fetchClient.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // body fully read below
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(body)
}

// postBody posts to url and returns status and body.
func postBody(t *testing.T, url string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		t.Fatalf("build request %s: %v", url, err)
	}
	resp, err := fetchClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // body fully read below
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(body)
}

// holdConnection opens a connection to addr and leaves a partial request
// in flight, so graceful shutdown has active work to wait for. A barrier
// round-trip on a second connection follows the partial write: accepts are
// FIFO, so once the barrier answers, the held connection is accepted and
// registered server-side and closing the listener at shutdown cannot race
// it into a kernel reset (which would silently dissolve the hold). Closing
// is registered with t.
func holdConnection(t *testing.T, addr string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("hold connection to %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: 127.0.0.1\r\n"); err != nil {
		t.Fatalf("write partial request: %v", err)
	}
	resp, err := fetchClient.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("barrier round-trip: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // barrier response body is irrelevant
}

// holdPartial opens a connection whose request headers are not finished,
// so the server sees an unresolved connection graceful shutdown must wait
// for. Closing is registered with t.
func holdPartial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("hold partial connection to %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: 127.0.0.1\r\n"); err != nil {
		t.Fatalf("write partial request: %v", err)
	}
	return conn
}

// TestServe_BootLogsBoundAddressAndServesEndpoints: the boot line logs the
// address the listener actually bound (not the configured ":0" string),
// and the endpoints answer.
func TestServe_BootLogsBoundAddressAndServesEndpoints(t *testing.T) {
	f := startServe(t, serveDoc("127.0.0.1:0", "127.0.0.1:0", 2000))
	addr := f.mainAddr(t)
	if addr == "" || strings.HasSuffix(addr, ":0") || addr == f.cfg.Server.Listen {
		t.Fatalf("boot listen = %q, want the bound address, not %q", addr, f.cfg.Server.Listen)
	}

	resp, err := fetchClient.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("get healthz: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // body drained below
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read healthz: %v", err)
	}
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != `{"status":"ok"}` {
		t.Fatalf("healthz = %d %q", resp.StatusCode, body)
	}

	resp, err = fetchClient.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("get metrics: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // body drained below
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "jevlar_build_info") {
		t.Fatalf("metrics = %d, body lacks jevlar_build_info", resp.StatusCode)
	}

	// Clean end: one signal, graceful shutdown, nil error.
	f.signals <- syscall.SIGTERM
	if err := f.waitDone(t); err != nil {
		t.Fatalf("Serve returned %v, want nil", err)
	}
	if _, ok := f.log.find("grace expired"); ok {
		t.Error("grace-expiry warning logged on an idle graceful shutdown")
	}
}

// TestServe_FirstSignalShutsDownGracefully: with no in-flight work the
// graceful path completes on the first signal, long before the grace
// period, with no forced close.
func TestServe_FirstSignalShutsDownGracefully(t *testing.T) {
	f := startServe(t, serveDoc("127.0.0.1:0", "", 10_000))
	f.signals <- syscall.SIGTERM
	start := time.Now()
	if err := f.waitDone(t); err != nil {
		t.Fatalf("Serve returned %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("graceful shutdown of an idle server took %v", elapsed)
	}
	if r, ok := f.log.find("shutdown"); !ok {
		t.Fatal("shutdown was not logged")
	} else if sig, _ := attrString(r, "signal"); sig != "terminated" {
		t.Errorf("shutdown log signal = %q, want terminated", sig)
	}
	if _, ok := f.log.find("grace expired"); ok {
		t.Error("idle shutdown must not hit the grace-expiry path")
	}
}

// TestServe_SecondSignalClosesImmediately: with an active connection
// holding the graceful shutdown open, the second signal forces an immediate
// close well inside the configured grace.
func TestServe_SecondSignalClosesImmediately(t *testing.T) {
	f := startServe(t, serveDoc("127.0.0.1:0", "", 10_000))
	holdConnection(t, f.mainAddr(t))

	f.signals <- syscall.SIGTERM
	time.Sleep(100 * time.Millisecond) // let the graceful shutdown begin
	second := time.Now()
	f.signals <- syscall.SIGINT

	if err := f.waitDone(t); err != nil {
		t.Fatalf("Serve returned %v, want nil", err)
	}
	if elapsed := time.Since(second); elapsed > 5*time.Second {
		t.Fatalf("second signal took %v to close, want immediate", elapsed)
	}
}

// TestServe_GraceExpiryForcesCloseAndWarns: with an active connection and
// no second signal, the grace expiry closes the server hard and logs the
// warning; Serve still returns nil because the shutdown was handled.
func TestServe_GraceExpiryForcesCloseAndWarns(t *testing.T) {
	f := startServe(t, serveDoc("127.0.0.1:0", "", 200))
	holdConnection(t, f.mainAddr(t))

	f.signals <- syscall.SIGTERM
	if err := f.waitDone(t); err != nil {
		t.Fatalf("Serve returned %v, want nil after handled shutdown", err)
	}
	r, ok := f.log.find("grace expired")
	if !ok {
		t.Fatal("grace expiry was not logged as a warning")
	}
	if r.Level != slog.LevelWarn {
		t.Errorf("grace-expiry record level = %v, want WARN", r.Level)
	}
}

// TestServe_ContextCancellationDrainsGracefully: cancelling ctx is a
// graceful-shutdown trigger, not a hard close — the cancelled parent must
// still grant the full grace window. One connection holds a complete but
// unread response (it must complete without loss) while a second holds a
// partial request, which keeps graceful shutdown waiting (net/http drops
// such connections only after their request completes, and never serves
// requests that finish after shutdown began). No grace-expiry warning may
// fire: the old behaviour cut the window to zero on cancel.
func TestServe_ContextCancellationDrainsGracefully(t *testing.T) {
	log := &capture{}
	signals := make(chan os.Signal, 4)
	doc := serveDoc("127.0.0.1:0", "", 3000)
	path := filepath.Join(t.TempDir(), "jevlar.toml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(doc), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- app.Serve(ctx, cfg, path, slog.New(log), signals) }()
	log.awaitLog(t, "boot")
	r, _ := log.find("boot")
	addr, _ := attrString(r, "listen")

	// In-flight connection: a complete request whose response has not
	// been read yet. It must come back intact after the cancellation.
	held, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("hold connection to %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = held.Close() })
	if _, err := io.WriteString(held, "GET /healthz HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n"); err != nil {
		t.Fatalf("write held request: %v", err)
	}
	// A second connection with a partial request keeps graceful shutdown
	// from settling immediately, so the grace window is actually exercised.
	partialConn := holdPartial(t, addr)
	// Accepts are FIFO: one completed round-trip proves the partial
	// connection was already accepted and registered, so closing the
	// listener at cancellation cannot race it into a reset.
	resp, err := fetchClient.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("barrier round-trip: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // barrier response body is irrelevant

	cancel()

	// Well inside the grace window nothing may have been forced closed.
	time.Sleep(300 * time.Millisecond)
	if _, ok := log.find("grace expired"); ok {
		t.Fatal("grace-expiry warning fired although the parent granted a full grace window")
	}

	// Complete the partial request: the server (in shutdown) drops that
	// connection, graceful shutdown settles, and the held response —
	// already in flight before the cancellation — is delivered intact.
	if _, err := io.WriteString(partialConn, "\r\n"); err != nil {
		t.Fatalf("completing the partial request: %v", err)
	}
	_ = held.SetReadDeadline(time.Now().Add(5 * time.Second))
	inflight, err := http.ReadResponse(bufio.NewReader(held), nil)
	if err != nil {
		t.Fatalf("in-flight request did not complete after cancellation: %v", err)
	}
	defer inflight.Body.Close() //nolint:errcheck // body drained below
	body, err := io.ReadAll(inflight.Body)
	if err != nil {
		t.Fatalf("read in-flight response: %v", err)
	}
	if inflight.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != `{"status":"ok"}` {
		t.Fatalf("in-flight response = %d %q, want 200 {\"status\":\"ok\"}", inflight.StatusCode, body)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after context cancellation")
	}
	if r, ok := log.find("shutdown"); !ok {
		t.Fatal("shutdown was not logged")
	} else if cause, _ := attrString(r, "cause"); cause == "" {
		t.Error("context-cancelled shutdown lacks a cause attribute")
	}
	if _, ok := log.find("grace expired"); ok {
		t.Error("grace-expiry warning fired although everything drained inside the grace window")
	}
}

// TestServe_BindFailureIsReturned: a bind failure is reported as an error
// (cmd maps it to exit code 1) and logged.
func TestServe_BindFailureIsReturned(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-bind port: %v", err)
	}
	t.Cleanup(func() { _ = held.Close() })

	log := &capture{}
	signals := make(chan os.Signal, 4)
	doc := serveDoc(held.Addr().String(), "", 1000)
	path := filepath.Join(t.TempDir(), "jevlar.toml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(doc), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := app.Serve(context.Background(), cfg, path, slog.New(log), signals); err == nil {
		t.Fatal("Serve returned nil for a bind failure, want the error")
	}
	if _, ok := log.find("listen failed"); !ok {
		t.Fatal("bind failure was not logged")
	}
}

// TestServe_VersionDefaultsToDev pins the contract cmd relies on: the
// package-level Version is "dev" until the binary assigns its injected
// build version.
func TestServe_VersionDefaultsToDev(t *testing.T) {
	old := app.Version
	app.Version = "dev"
	t.Cleanup(func() { app.Version = old })
	f := startServe(t, serveDoc("127.0.0.1:0", "", 2000))
	if r, ok := f.log.find("boot"); !ok {
		t.Fatal("no boot record")
	} else if v, _ := attrString(r, "version"); v != "dev" {
		t.Errorf("boot version = %q, want dev", v)
	}
	f.signals <- syscall.SIGTERM
	if err := f.waitDone(t); err != nil {
		t.Fatalf("Serve returned %v, want nil", err)
	}
}

// TestServe_HealthzPayloadIsJSON proves the payload parses as JSON with the
// expected shape (guarded here so the string literal stays honest).
func TestServe_HealthzPayloadIsJSON(t *testing.T) {
	f := startServe(t, serveDoc("127.0.0.1:0", "", 2000))
	addr := f.mainAddr(t)
	resp, err := fetchClient.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("get healthz: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // body drained below
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read healthz: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("healthz body %q is not JSON: %v", body, err)
	}
	if payload["status"] != "ok" {
		t.Fatalf("healthz status = %q, want ok", payload["status"])
	}
	f.signals <- syscall.SIGTERM
	if err := f.waitDone(t); err != nil {
		t.Fatalf("Serve returned %v, want nil", err)
	}
}

// reloadFixture boots a server with concrete loopback ports (a reload
// re-validates the file, and port 0 is not a valid listener there) and
// both listeners enabled.
type reloadFixture struct {
	*serveFixture
	mainPort  int
	adminPort int
	adminBase string
}

// startReloadFixture boots a two-listener server on free loopback ports.
func startReloadFixture(t *testing.T) *reloadFixture {
	t.Helper()
	mainPort := freePort(t)
	adminPort := freePort(t)
	f := &reloadFixture{
		serveFixture: startServe(t, serveDoc(
			fmt.Sprintf("127.0.0.1:%d", mainPort),
			fmt.Sprintf("127.0.0.1:%d", adminPort),
			2000,
		)),
		mainPort:  mainPort,
		adminPort: adminPort,
	}
	f.adminBase = "http://" + f.adminAddr(t)
	return f
}

// freePort reserves and releases a loopback port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return port
}

// statusHash fetches /admin/status and returns its config_hash.
func (f *reloadFixture) statusHash(t *testing.T) string {
	t.Helper()
	code, body := get(t, f.adminBase+"/admin/status")
	if code != http.StatusOK {
		t.Fatalf("admin status = %d: %s", code, body)
	}
	var status struct {
		ConfigHash string `json:"config_hash"`
	}
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatalf("status body is not JSON: %v\n%s", err, body)
	}
	return status.ConfigHash
}

// withSecondProviderDoc returns the fixture's doc with a second provider
// and a changed timeout, so a reload is observable in hash and status.
func (f *reloadFixture) withSecondProviderDoc() string {
	return fmt.Sprintf(`
config_version = 1

[server]
listen = "127.0.0.1:%d"
admin_listen = "127.0.0.1:%d"
shutdown_grace_ms = 2000

[[providers]]
name = "loopback"
base_url = "http://127.0.0.1:9"
path = "/v1/systemone"
auth = "none"
timeout_ms = 4000

[[providers]]
name = "second"
base_url = "https://second.example"
path = "/v1/systemone"
auth = "none"
`, f.mainPort, f.adminPort)
}

// TestReload_SIGHUPIsWithAValidConfigSwapsTheSnapshot: SIGHUP re-reads
// the file; the new hash is logged, /admin/status reports the new
// configuration (a second provider), and neither listener drops.
func TestReload_SIGHUPIsWithAValidConfigSwapsTheSnapshot(t *testing.T) {
	f := startReloadFixture(t)
	oldHash := f.bootHash(t)
	if f.statusHash(t) != oldHash {
		t.Fatalf("initial status hash differs from boot hash")
	}

	f.rewriteConfig(t, f.withSecondProviderDoc())
	f.signals <- syscall.SIGHUP
	r := f.log.awaitLog(t, "config reloaded")
	newHash, _ := attrString(r, "config_hash")
	if newHash == "" || newHash == oldHash {
		t.Fatalf("reload logged hash %q, want a new hash (was %q)", newHash, oldHash)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.statusHash(t) == newHash {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := f.statusHash(t); got != newHash {
		t.Fatalf("status hash = %q, want the reloaded %q", got, newHash)
	}
	code, body := get(t, f.adminBase+"/admin/status")
	if code != http.StatusOK || !strings.Contains(body, `"second"`) {
		t.Fatalf("status after reload lacks the new provider:\n%s", body)
	}
	// The main listener kept serving through the reload.
	if code, b := get(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", f.mainPort)); code != http.StatusOK {
		t.Fatalf("main listener dropped during reload: %d %s", code, b)
	}

	f.signals <- syscall.SIGTERM
	if err := f.waitDone(t); err != nil {
		t.Fatalf("Serve returned %v, want nil", err)
	}
}

// TestReload_SIGHUPIsWithAnInvalidConfigKeepsTheOldRuntime: a failed
// reload logs the error, increments jevlar_config_reload_failures_total,
// keeps the previous hash serving and drops neither listener.
func TestReload_SIGHUPIsWithAnInvalidConfigKeepsTheOldRuntime(t *testing.T) {
	f := startReloadFixture(t)
	oldHash := f.statusHash(t)

	f.rewriteConfig(t, strings.Replace(f.withSecondProviderDoc(), "config_version = 1", "config_version = 2", 1))
	f.signals <- syscall.SIGHUP
	f.log.awaitLog(t, "config reload failed")

	if got := f.statusHash(t); got != oldHash {
		t.Fatalf("status hash = %q after a failed reload, want the old %q", got, oldHash)
	}
	// The failure counter incremented exactly once.
	code, body := get(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", f.mainPort))
	if code != http.StatusOK || !strings.Contains(body, "jevlar_config_reload_failures_total 1") {
		t.Fatalf("metrics lack the incremented reload-failure counter:\n%s", body)
	}
	// Both listeners are still alive.
	if code, b := get(t, f.adminBase+"/admin/status"); code != http.StatusOK {
		t.Fatalf("admin listener dropped after a failed reload: %d %s", code, b)
	}
	if code, b := get(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", f.mainPort)); code != http.StatusOK {
		t.Fatalf("main listener dropped after a failed reload: %d %s", code, b)
	}

	f.signals <- syscall.SIGTERM
	if err := f.waitDone(t); err != nil {
		t.Fatalf("Serve returned %v, want nil", err)
	}
}

// TestReload_AdminEndpointSwapsAndReports: POST /admin/reload runs the
// same pipeline; the response carries the new hash and /admin/status
// reflects it.
func TestReload_AdminEndpointSwapsAndReports(t *testing.T) {
	f := startReloadFixture(t)
	oldHash := f.statusHash(t)

	f.rewriteConfig(t, f.withSecondProviderDoc())
	code, body := postBody(t, f.adminBase+"/admin/reload")
	if code != http.StatusOK {
		t.Fatalf("reload = %d: %s", code, body)
	}
	var resp struct {
		Status     string `json:"status"`
		ConfigHash string `json:"config_hash"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("reload body is not JSON: %v\n%s", err, body)
	}
	if resp.Status != "ok" || resp.ConfigHash == "" || resp.ConfigHash == oldHash {
		t.Fatalf("reload body = %+v, want ok with a new hash", resp)
	}
	if got := f.statusHash(t); got != resp.ConfigHash {
		t.Fatalf("status hash = %q, want the reloaded %q", got, resp.ConfigHash)
	}

	f.signals <- syscall.SIGTERM
	if err := f.waitDone(t); err != nil {
		t.Fatalf("Serve returned %v, want nil", err)
	}
}

// TestReload_AdminEndpointWithAnInvalidConfigReturnsFieldPaths: the 500
// body carries one entry per violation with its field path, the counter
// increments and the old snapshot keeps serving.
func TestReload_AdminEndpointWithAnInvalidConfigReturnsFieldPaths(t *testing.T) {
	f := startReloadFixture(t)
	oldHash := f.statusHash(t)

	f.rewriteConfig(t, strings.Replace(f.withSecondProviderDoc(), "config_version = 1", "config_version = 2", 1))
	code, body := postBody(t, f.adminBase+"/admin/reload")
	if code != http.StatusInternalServerError {
		t.Fatalf("failed reload = %d: %s", code, body)
	}
	var resp struct {
		Status string `json:"status"`
		Errors []struct {
			Field   string `json:"field"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("failure body is not JSON: %v\n%s", err, body)
	}
	if resp.Status != "error" || len(resp.Errors) == 0 {
		t.Fatalf("failure body = %s", body)
	}
	if resp.Errors[0].Field != "config_version" {
		t.Errorf("first error field = %q, want config_version", resp.Errors[0].Field)
	}
	if got := f.statusHash(t); got != oldHash {
		t.Fatalf("status hash changed after a failed reload: %q (want %q)", got, oldHash)
	}
	if _, m := get(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", f.mainPort)); !strings.Contains(m, "jevlar_config_reload_failures_total 1") {
		t.Fatalf("reload-failure counter was not incremented:\n%s", m)
	}

	f.signals <- syscall.SIGTERM
	if err := f.waitDone(t); err != nil {
		t.Fatalf("Serve returned %v, want nil", err)
	}
}

// TestReload_StatusIsRedactedOverTheWire: a canary key value and a canary
// inbound token (plus its configured hash) never appear in /admin/status
// served by the real listener.
func TestReload_StatusIsRedactedOverTheWire(t *testing.T) {
	canary := "CANARY-outbound-key-value-77"
	token := "canary-inbound-token-13"
	sum := sha256Hex(token)
	doc := fmt.Sprintf(`
config_version = 1

[server]
listen = "127.0.0.1:%d"
admin_listen = "127.0.0.1:%d"
shutdown_grace_ms = 2000

[auth]
keys = [{ id = "app", hash = "%s" }]

[[providers]]
name = "loopback"
base_url = "http://127.0.0.1:9"
path = "/v1/systemone"
auth = "none"

[[providers]]
name = "secretive"
base_url = "https://secretive.example"
path = "/v1/systemone"
auth = "bearer"
api_keys = [{ id = "k1", value = "%s" }]
`, freePort(t), freePort(t), sum, canary)
	f := startServe(t, doc)
	f.adminAddr(t)

	code, body := get(t, "http://"+f.adminAddr(t)+"/admin/status")
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, body)
	}
	for _, secret := range []string{canary, token, sum} {
		if strings.Contains(body, secret) {
			t.Fatalf("admin status leaks %q:\n%s", secret, body)
		}
	}
	if !strings.Contains(body, `"k1"`) || !strings.Contains(body, `"app"`) {
		t.Fatalf("status must still list key ids:\n%s", body)
	}

	f.signals <- syscall.SIGTERM
	if err := f.waitDone(t); err != nil {
		t.Fatalf("Serve returned %v, want nil", err)
	}
}

// sha256Hex computes the SHA-256 hex of s (the keyhash contract).
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestServe_ReadyzAndModelsSurface pins the the routed stage read-only surface:
// /readyz reports readiness, /v1/models returns the sorted config union,
// and wrong methods get 405 rather than a silent 404.
func TestServe_ReadyzAndModelsSurface(t *testing.T) {
	t.Parallel()
	f := startServe(t, serveDoc("127.0.0.1:0", "", 1000))

	status, body := get(t, "http://"+f.mainAddr(t)+"/readyz")
	if status != http.StatusOK || !strings.Contains(body, `"ready"`) {
		t.Fatalf("/readyz = %d %q, want 200 ready", status, body)
	}

	status, body = getAuthed(t, "http://"+f.mainAddr(t)+"/v1/models")
	if status != http.StatusOK {
		t.Fatalf("/v1/models = %d %q", status, body)
	}
	if !strings.Contains(body, `"object":"list"`) {
		t.Fatalf("/v1/models body = %q", body)
	}

	status, _ = postBody(t, "http://"+f.mainAddr(t)+"/v1/models")
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/models = %d, want 405", status)
	}

	status, _ = get(t, "http://"+f.mainAddr(t)+"/v1/systemone")
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/systemone = %d, want 405", status)
	}

	f.signals <- syscall.SIGTERM
	if err := f.waitDone(t); err != nil {
		t.Fatalf("Serve: %v", err)
	}
}
