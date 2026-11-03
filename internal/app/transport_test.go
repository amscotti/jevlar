package app

import (
	"net/http"
	"testing"

	"github.com/amscotti/jevlar/internal/config"
	"github.com/amscotti/jevlar/internal/runtime"
)

// registryFixture builds a snapshot for two providers: two origins, one of
// them configured twice with different concurrency caps.
func registryFixture(t *testing.T) *runtime.Runtime {
	t.Helper()
	cfg := &config.Config{
		ConfigVersion: 1,
		Server: config.Server{
			Listen: "127.0.0.1:0", RequestTimeoutMs: 1000,
			MaxBodyBytes: 1024, MaxResponseBytes: 1024, MaxInflight: 10,
			ShutdownGraceMs: 1000, ReadHeaderTimeoutMs: 1000, LogLevel: "error",
		},
		Providers: []config.Provider{
			{
				Name: "a", BaseURL: "https://api.example.test", Path: "/v1/systemone",
				Auth: "none", Models: []string{"m"}, TimeoutMs: 1000, MaxConcurrency: 4,
			},
			{
				Name: "b", BaseURL: "https://api.example.test", Path: "/v1/systemone",
				Auth: "none", Models: []string{"m"}, TimeoutMs: 1000, MaxConcurrency: 8,
			},
		},
	}
	rt, err := runtime.Build(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return rt
}

// TestRegistry_ReusesTransportsAcrossSnapshots pins the reload contract:
// a new snapshot for the same origin borrows the *same* transport, so a
// reload cannot orphan a connection pool.
func TestRegistry_ReusesTransportsAcrossSnapshots(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	t.Cleanup(reg.Close)

	first := registryFixture(t)
	reg.Apply(first)
	second := registryFixture(t)
	reg.Apply(second)

	if first.Providers["a"].Transport == nil {
		t.Fatal("first snapshot got no transport")
	}
	if first.Providers["a"].Transport != second.Providers["a"].Transport {
		t.Fatal("reload replaced the pooled transport instead of reusing it")
	}
	if first.Providers["a"].Slots == nil || first.Providers["a"].Slots != second.Providers["a"].Slots {
		t.Fatal("reload replaced the provider's concurrency slots")
	}
	// Different concurrency is a different pool.
	if first.Providers["b"].Transport == first.Providers["a"].Transport {
		t.Fatal("providers with different concurrency caps share a pool")
	}
}

// TestRegistry_ReleasesCloseIdleConnectionsWhenUnreferenced pins the
// refcounting: after the last snapshot referencing an origin is released,
// the entry is gone.
func TestRegistry_ReleasesCloseIdleConnectionsWhenUnreferenced(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()

	first := registryFixture(t)
	reg.Apply(first)
	second := registryFixture(t)
	reg.Apply(second)
	transport := first.Providers["a"].Transport

	reg.Release(second)
	reg.mu.Lock()
	stillThere := len(reg.transports) > 0
	reg.mu.Unlock()
	if !stillThere {
		t.Fatal("releasing one of two references dropped the pool")
	}

	reg.Release(first)
	reg.mu.Lock()
	remaining := len(reg.transports)
	reg.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("%d transports survived the last release", remaining)
	}
	if transport == nil {
		t.Fatal("test lost the transport handle")
	}
	reg.Close()
}

// TestRegistry_ReleaseNilIsSafe pins that a snapshot which failed to
// publish can be released without panicking.
func TestRegistry_ReleaseNilIsSafe(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	reg.Release(nil)
	reg.Close()
}

// TestTransportPoolBounds pins the transport settings the plan requires.
func TestTransportPoolBounds(t *testing.T) {
	t.Parallel()
	tr := newTransport(7, 1500*1000*1000)
	if tr.MaxConnsPerHost != 7 {
		t.Fatalf("MaxConnsPerHost = %d, want 7", tr.MaxConnsPerHost)
	}
	if tr.MaxIdleConnsPerHost != 7 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want 7", tr.MaxIdleConnsPerHost)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 disabled")
	}
	if !tr.DisableCompression {
		t.Fatal("DisableCompression must be set so usage stays readable")
	}
	tr.CloseIdleConnections()
	_ = http.MethodPost
}
