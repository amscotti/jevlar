package app_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/amscotti/jevlar/internal/testutil"
)

// healthDoc builds a config with a bearer provider pointed at upstream, an
// admin listener, one inbound client and a breaker.
func healthDoc(listen, adminListen, upstream, clientHash string) string {
	return fmt.Sprintf(`
config_version = 1

[server]
listen = %q
admin_listen = %q
request_timeout_ms = 5000
shutdown_grace_ms = 1000

[auth]
keys = [{ id = "dev", hash = %q }]

[[providers]]
name = "up"
base_url = %q
path = "/v1/systemone"
auth = "bearer"
models = ["jev-latest"]
timeout_ms = 1000
max_concurrency = 32
api_keys = [{ id = "k1", value = "secret-one" }]

[models."jev-latest"]
targets = [{ provider = "up", model = "jev-latest" }]
policy = "priority"
max_attempts = 1

[breaker]
threshold = 3
cooldown_ms = 1000
`, listen, adminListen, clientHash, upstream)
}

// TestReload_DisabledKeySurvivesFiftyReloads pins the headline the routed stage
// guarantee end to end: a key disabled by an upstream 401 stays disabled
// across 50 reloads, and the process does not leak file descriptors.
func TestReload_DisabledKeySurvivesFiftyReloads(t *testing.T) {
	up := testutil.NewFakeUpstream(t)
	up.SetHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	token := "reload-dev-token"
	sum := sha256.Sum256([]byte(token))
	doc := healthDoc(
		fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		up.URL(),
		hex.EncodeToString(sum[:]),
	)
	f := startServe(t, doc)
	adminAddr := f.adminAddr(t)
	mainAddr := f.mainAddr(t)

	// One request: the upstream rejects the key, so it is disabled.
	body := `{"model":"jev-latest","state":"x"}`
	req, err := http.NewRequest(http.MethodPost, "http://"+mainAddr+"/v1/systemone", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	if got := keyState(t, adminAddr); got != "disabled" {
		t.Fatalf("key state after a 401 = %q, want disabled", got)
	}

	fdsBefore := countFDs()
	// Let the process settle, then record the baseline goroutine count.
	time.Sleep(100 * time.Millisecond)
	goroutinesBefore := runtime.NumGoroutine()

	for i := 0; i < 50; i++ {
		f.signals <- syscall.SIGHUP
		// Give the reload pipeline a moment so the reloads are sequential.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if hash := statusHash(t, adminAddr); hash != "" {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	if got := keyState(t, adminAddr); got != "disabled" {
		t.Fatalf("key state after 50 reloads = %q, want disabled", got)
	}
	// No goroutine growth across 50 reloads. A small margin absorbs the
	// runtime's own bookkeeping goroutines; a leak would be tens per reload.
	time.Sleep(200 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > goroutinesBefore+10 {
		t.Fatalf("goroutines grew across 50 reloads: %d -> %d", goroutinesBefore, after)
	}

	if fdsBefore >= 0 {
		// Allow a small margin for test-harness connections, but not growth
		// proportional to the reload count.
		if after := countFDs(); after > fdsBefore+8 {
			t.Fatalf("file descriptors grew across 50 reloads: %d -> %d", fdsBefore, after)
		}
	}

	f.signals <- syscall.SIGTERM
	if err := f.waitDone(t); err != nil {
		t.Fatalf("Serve: %v", err)
	}
}

// statusDoc fetches /admin/status and decodes the parts the tests read.
type statusDoc struct {
	ConfigHash string `json:"config_hash"`
	Keys       []struct {
		ID    string `json:"id"`
		State string `json:"state"`
	} `json:"keys"`
}

func fetchStatus(t *testing.T, adminAddr string) statusDoc {
	t.Helper()
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + adminAddr + "/admin/status")
	if err != nil {
		t.Fatalf("admin status: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var doc statusDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return doc
}

func keyState(t *testing.T, adminAddr string) string {
	t.Helper()
	doc := fetchStatus(t, adminAddr)
	if len(doc.Keys) == 0 {
		t.Fatal("status reports no keys")
	}
	return doc.Keys[0].State
}

func statusHash(t *testing.T, adminAddr string) string {
	t.Helper()
	return fetchStatus(t, adminAddr).ConfigHash
}

// countFDs returns this process's open descriptor count on Linux, or -1
// where /proc is unavailable.
func countFDs() int {
	if runtime.GOOS != "linux" {
		return -1
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}

// TestReload_UnderTrafficIsRaceFree pins the "configuration is immutable
// per request" invariant: reloads swap the snapshot while requests are in
// flight, and every request still completes against exactly one snapshot.
func TestReload_UnderTrafficIsRaceFree(t *testing.T) {
	up := testutil.NewFakeUpstream(t)
	up.SetHandler(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1}}`))
	})

	token := "traffic-token"
	sum := sha256.Sum256([]byte(token))
	doc := healthDoc(
		fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		up.URL(),
		hex.EncodeToString(sum[:]),
	)
	f := startServe(t, doc)
	mainAddr := f.mainAddr(t)
	client := &http.Client{Timeout: 5 * time.Second}

	const workers = 8
	const perWorker = 30
	done := make(chan error, workers)
	for w := 0; w < workers; w++ {
		go func() {
			for i := 0; i < perWorker; i++ {
				req, err := http.NewRequest(http.MethodPost,
					"http://"+mainAddr+"/v1/systemone", strings.NewReader(`{"model":"jev-latest","state":"x"}`))
				if err != nil {
					done <- err
					return
				}
				req.Header.Set("Authorization", "Bearer "+token)
				resp, err := client.Do(req)
				if err != nil {
					done <- fmt.Errorf("request: %w", err)
					return
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					done <- fmt.Errorf("status %d during reload", resp.StatusCode)
					return
				}
			}
			done <- nil
		}()
	}

	// Reload continuously while the workers run.
	for i := 0; i < 20; i++ {
		f.signals <- syscall.SIGHUP
		time.Sleep(5 * time.Millisecond)
	}

	for w := 0; w < workers; w++ {
		if err := <-done; err != nil {
			t.Fatalf("worker failed: %v", err)
		}
	}

	f.signals <- syscall.SIGTERM
	if err := f.waitDone(t); err != nil {
		t.Fatalf("Serve: %v", err)
	}
}
