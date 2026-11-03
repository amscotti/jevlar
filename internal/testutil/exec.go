package testutil

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// buildOnce guards the one-shot build of the jevlar binary used by
// end-to-end tests.
var buildOnce sync.Once

// builtPath is the compiled binary path, set by buildOnce.
var builtPath string

// buildErr records a failed binary build, set by buildOnce.
var buildErr error

// JevlarBinary compiles cmd/jevlar once per test binary run and returns its
// path. End-to-end tests exec this binary so exit codes, signal handling and
// listener behaviour are exercised exactly as shipped. Call
// CleanupBuiltBinary from TestMain to remove the artefact when the run is
// over.
func JevlarBinary(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() {
		root := ModuleRoot(t)
		out := filepath.Join(os.TempDir(), fmt.Sprintf("jevlar-e2e-%d", os.Getpid()))
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", "build", "-o", out, "./cmd/jevlar")
		cmd.Dir = root
		var buildOutput []byte
		buildOutput, buildErr = cmd.CombinedOutput()
		if buildErr != nil {
			buildErr = fmt.Errorf("go build ./cmd/jevlar: %w: %s", buildErr, buildOutput)
			return
		}
		builtPath = out
	})
	if buildErr != nil {
		t.Fatalf("jevlar binary unavailable: %v", buildErr)
	}
	return builtPath
}

// CleanupBuiltBinary removes the binary produced by JevlarBinary, if any, so
// test runs leave no temporary artefacts behind. Call it from TestMain after
// m.Run; it is safe to call when nothing was built.
func CleanupBuiltBinary() {
	if builtPath != "" {
		_ = os.Remove(builtPath)
	}
}

// ModuleRoot locates the repository root by walking up from this package's
// source directory; test code uses it to reach committed fixtures such as
// configs/minimal.toml.
func ModuleRoot(t testing.TB) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate testutil source file")
	}
	dir := thisFile
	for i := 0; i < 10; i++ {
		dir = filepath.Dir(dir)
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
	}
	t.Fatalf("go.mod not found above %s", thisFile)
	return ""
}

// FreeLoopbackPort reserves an ephemeral loopback port and releases it,
// returning the port number. The caller should listen promptly; the port is
// not guaranteed to stay free, which mirrors real start-up races.
func FreeLoopbackPort(t testing.TB) int {
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

// probeClient bounds every health probe so a hung socket cannot stall a
// test for the whole poll window.
var probeClient = &http.Client{Timeout: 2 * time.Second}

// WaitForHealthy polls url until it answers 200 or the timeout elapses.
func WaitForHealthy(t testing.TB, url string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("build probe for %s: %w", url, err)
		}
		resp, err := probeClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("wait for %s: %w", url, lastErr)
}
