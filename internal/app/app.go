package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	goruntime "runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/amscotti/jevlar/internal/admin"
	"github.com/amscotti/jevlar/internal/config"
	"github.com/amscotti/jevlar/internal/health"
	"github.com/amscotti/jevlar/internal/metrics"
	"github.com/amscotti/jevlar/internal/proxy"
	"github.com/amscotti/jevlar/internal/runtime"
	"github.com/amscotti/jevlar/internal/usage"
)

// Version is the gateway version reported in the boot log and in
// jevlar_build_info. cmd/jevlar assigns the build-time injected version
// before calling Serve; it stays "dev" when nothing is injected.
var Version = "dev"

const (
	// idleTimeout bounds idle keep-alive connections.
	idleTimeout = 60 * time.Second
)

// server owns the process's serving state: the published runtime
// snapshot, the reload pipeline and the two listeners (main and admin).
// It is created by Serve and never escapes it.
type server struct {
	configPath string
	log        *slog.Logger
	metrics    *metrics.Metrics
	bootAt     time.Time

	rt atomic.Pointer[runtime.Runtime]

	// bootListen and bootAdminListen are the addresses actually bound at
	// boot. Reloads never re-bind; they warn when a config asks for a
	// different address.
	bootListen      string
	bootAdminListen string
	bootUsage       config.Usage

	// reloadMu serialises reloads: a SIGHUP racing a POST /admin/reload
	// must not interleave two pipelines.
	reloadMu sync.Mutex

	// transports owns the process-lifetime upstream pools, shared across
	// every snapshot so a reload cannot orphan connections.
	transports *Registry

	// health owns key, breaker, account and client state across reloads.
	health *health.Registry
}

// Serve validates nothing itself: cfg must already be loaded and
// validated (cmd does both). It builds the first runtime snapshot from
// cfg — nothing binds until that succeeds — publishes it, binds the main
// listener (and the admin listener when server.admin_listen is set),
// then blocks until shutdown.
//
// Reloads: SIGHUP on signals and POST /admin/reload both run the same
// pipeline (load configPath, validate, build, swap). A failed reload
// logs the error, increments jevlar_config_reload_failures_total and
// leaves the previous snapshot serving; a successful one swaps the
// snapshot and logs the new configuration hash.
//
// Shutdown is two-stage: the first SIGTERM/SIGINT (or cancellation of
// ctx) starts a graceful shutdown of both listeners bounded by
// server.shutdown_grace_ms; a second signal closes them immediately;
// grace expiry closes them hard and logs a warning. SIGHUP never shuts
// anything down. In all shutdown cases Serve returns nil — the shutdown
// was handled. It returns a non-nil error only for start-up failures
// (snapshot build or bind).
//
// Serve never installs a signal handler: the caller owns signals and
// passes the channel, so signals delivered before the bind are not lost.
func Serve(ctx context.Context, cfg *config.Config, configPath string, log *slog.Logger, signals <-chan os.Signal) error {
	s := &server{
		configPath: configPath,
		log:        log,
		metrics: metrics.New(metrics.Options{
			Version:   Version,
			GoVersion: goruntime.Version(),
		}),
		bootAt:          time.Now(),
		bootListen:      cfg.Server.Listen,
		bootAdminListen: cfg.Server.AdminListen,
		bootUsage:       cfg.Usage,
		transports:      NewRegistry(),
		health:          health.New(),
	}
	defer s.transports.Close()

	// Build the snapshot before any socket is bound: a configuration
	// that cannot produce a serving snapshot stops the process here.
	rt, err := runtime.Build(cfg)
	if err != nil {
		log.Error("runtime build failed", "error", err)
		return err
	}
	rt.StartedAt = s.bootAt
	// Borrow pooled transports and concurrency slots before the snapshot is
	// published, so every request sees a fully wired snapshot.
	s.transports.Apply(rt)
	s.attachHealth(cfg, rt)
	s.rt.Store(rt)

	proxyHandler := proxy.New(s.rt.Load, log, s.metrics)

	// Durable rollups own no request-path work: a ticker snapshots the
	// tracker, so accounting never delays a caller. The stop function is
	// deferred so the final interval is flushed before Serve returns.
	defer startRollups(ctx, log, &configUsage{
		Enabled:       cfg.Usage.Enabled,
		Dir:           cfg.Usage.Dir,
		RetentionDays: cfg.Usage.RetentionDays,
		MaxFileMB:     cfg.Usage.MaxFileMB,
		Interval:      time.Hour,
	}, proxyHandler.Tracker())()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.Handle("/metrics", s.metrics.Handler())
	mux.HandleFunc("/v1/systemone", methodOnly(http.MethodPost, proxyHandler.ServeSystemOne))
	mux.HandleFunc("/v1/models", methodOnly(http.MethodGet, proxyHandler.Models))

	mainServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: time.Duration(cfg.Server.ReadHeaderTimeoutMs) * time.Millisecond,
		IdleTimeout:       idleTimeout,
	}
	listener, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		log.Error("listen failed", "listen", cfg.Server.Listen, "error", err)
		return err
	}
	// Log the address actually bound, not the configured string: they
	// differ exactly when the operator asked for an ephemeral port.
	log.Info("boot", "version", Version, "listen", listener.Addr().String(),
		"config_hash", rt.ConfigHash)

	servers := []*http.Server{mainServer}
	listeners := []net.Listener{listener}
	if cfg.Server.AdminListen != "" {
		adminListener, err := net.Listen("tcp", cfg.Server.AdminListen)
		if err != nil {
			_ = listener.Close()
			log.Error("admin listen failed", "admin_listen", cfg.Server.AdminListen, "error", err)
			return err
		}
		// The durable-rollup reader exists only when rollups are enabled at
		// boot; otherwise /admin/usage?window= answers 503 instead of
		// surfacing a filesystem error for a directory that was never used.
		var usageHistory func(time.Duration) (usage.Rollup, error)
		if cfg.Usage.Enabled {
			dir := cfg.Usage.Dir
			usageHistory = func(since time.Duration) (usage.Rollup, error) {
				return usage.ReadSince(dir, time.Now().Add(-since))
			}
		}
		adminPlane := admin.New(admin.Options{
			Version: Version,
			Token:   cfg.Server.AdminToken,
			Runtime: s.rt.Load,
			Reload: func() (string, error) {
				rt, err := s.reload()
				if err != nil {
					return "", err
				}
				return rt.ConfigHash, nil
			},
			Usage:        proxyHandler.Tracker().Snapshot,
			UsageHistory: usageHistory,
		})
		adminServer := &http.Server{
			Handler:           adminPlane.Handler(),
			ReadHeaderTimeout: time.Duration(cfg.Server.ReadHeaderTimeoutMs) * time.Millisecond,
			IdleTimeout:       idleTimeout,
		}
		servers = append(servers, adminServer)
		listeners = append(listeners, adminListener)
		log.Info("admin listener bound", "admin_listen", adminListener.Addr().String())
	}

	serveErr := make(chan error, len(servers))
	for i := range servers {
		srv, l := servers[i], listeners[i]
		go func() { serveErr <- srv.Serve(l) }()
	}

	// Signal loop: SIGHUP reloads and keeps serving; SIGTERM/SIGINT (or
	// ctx cancellation) leaves the loop and shuts both listeners down.
	var first os.Signal
	shutdown := false
	for !shutdown {
		select {
		case err := <-serveErr:
			// Serve never returns nil, and http.ErrServerClosed cannot
			// reach this branch: a server is only closed after the loop
			// below has left on a shutdown trigger. Anything here is a
			// real start-up failure; release the ports before exiting.
			for _, l := range listeners {
				_ = l.Close()
			}
			log.Error("serve failed", "error", err)
			return err
		case sig := <-signals:
			if sig == syscall.SIGHUP {
				_, _ = s.reload()
				continue
			}
			first = sig
			shutdown = true
		case <-ctx.Done():
			first = nil
			shutdown = true
		}
	}

	grace := time.Duration(cfg.Server.ShutdownGraceMs) * time.Millisecond
	if first != nil {
		log.Info("shutdown", "signal", first.String(), "grace_ms", cfg.Server.ShutdownGraceMs)
	} else {
		log.Info("shutdown", "cause", "context cancelled", "grace_ms", cfg.Server.ShutdownGraceMs)
	}
	// The shutdown budget is independent of ctx: cancelling ctx triggered
	// the shutdown and must not also cut the grace window short — in-flight
	// work gets the full grace. context.WithoutCancel drops the parent's
	// cancellation while keeping its values.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		var wg sync.WaitGroup
		for _, srv := range servers {
			wg.Add(1)
			go func(srv *http.Server) {
				defer wg.Done()
				if err := srv.Shutdown(shutdownCtx); err != nil {
					log.Warn("grace expired, forcing close", "error", err)
					_ = srv.Close()
				}
			}(srv)
		}
		wg.Wait()
	}()
	select {
	case <-shutdownDone:
	case sig := <-signals:
		if sig != syscall.SIGHUP {
			// Second shutdown signal: stop waiting and close immediately.
			for _, srv := range servers {
				_ = srv.Close()
			}
		}
		// SIGHUP is only ever a reload trigger. A reload during the grace
		// window cannot take effect (the process is exiting), so it is
		// drained without forcing the close; keep waiting for the drain.
		for {
			select {
			case <-shutdownDone:
				return nil
			case sig := <-signals:
				if sig == syscall.SIGHUP {
					continue
				}
				for _, srv := range servers {
					_ = srv.Close()
				}
			}
		}
	}
	return nil
}

// reload runs the reload pipeline: load the configuration file, validate
// it, build a new snapshot, publish it. On any failure the previous
// snapshot stays published, the failure is logged and the
// jevlar_config_reload_failures_total counter incremented. The returned
// snapshot is the newly published one.
func (s *server) reload() (*runtime.Runtime, error) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	cfg, err := config.Load(s.configPath)
	if err != nil {
		s.reloadFailed(err)
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		s.reloadFailed(err)
		return nil, err
	}
	for _, w := range cfg.Warnings {
		s.log.Warn("configuration warning", "finding", w.String())
	}
	prev := s.rt.Load()
	rt, err := runtime.Build(cfg)
	if err != nil {
		s.reloadFailed(err)
		return nil, err
	}
	rt.StartedAt = s.bootAt
	s.transports.Apply(rt)
	s.attachHealth(cfg, rt)
	// Listeners are bound once at boot and never re-bound: an in-flight
	// reload that changes an address would either need to move a live
	// socket (dropping connections) or silently ignore the new value.
	// Jevlar ignores it loudly so the operator is not left believing the
	// change took effect. Compare against the boot address, not the
	// previous snapshot, so a flip-flop is still reported.
	if cfg.Server.Listen != s.bootListen {
		s.log.Warn("server.listen change requires a restart; still listening on the boot address",
			"configured", cfg.Server.Listen, "bound", s.bootListen)
	}
	if cfg.Server.AdminListen != s.bootAdminListen {
		s.log.Warn("server.admin_listen change requires a restart; the admin plane keeps its boot address",
			"configured", cfg.Server.AdminListen, "bound", s.bootAdminListen)
	}
	if cfg.Usage != s.bootUsage {
		s.log.Warn("usage changes require a restart; the rollup writer and /admin/usage?window= keep the boot settings",
			"configured", cfg.Usage, "boot", s.bootUsage)
	}
	s.rt.Store(rt)
	// Give back the pools the retired snapshot borrowed only after the new
	// one is published: in-flight requests keep using the old handles, and
	// a pool closes when its last reference goes away.
	s.transports.Release(prev)
	s.log.Info("config reloaded", "config_hash", rt.ConfigHash)
	return rt, nil
}

// reloadFailed records one failed reload: a log line and the counter.
func (s *server) reloadFailed(err error) {
	s.metrics.ConfigReloadFailures().Inc()
	s.log.Error("config reload failed", "error", err)
}

// handleHealthz answers 200 with a fixed healthy payload.
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, `{"status":"ok"}`)
}

// handleReadyz answers 200 only when at least one configured target has a
// usable key; otherwise 503, so a load balancer stops sending traffic to a
// gateway that cannot serve anything.
func (s *server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	rt := s.rt.Load()
	ready := false
	if rt != nil {
		for _, p := range rt.Providers {
			if p.Auth == "none" {
				// A keyless provider is usable as long as its breaker is not
				// holding it open.
				if b := s.health.Breaker(p.Name); b == nil || !b.Open() {
					ready = true
					break
				}
				continue
			}
			// A bearer provider is ready only when a key can actually be
			// selected: every key disabled or cooling means not ready.
			if p.Pool != nil && p.Pool.Available() {
				ready = true
				break
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, `{"status":"not_ready"}`)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, `{"status":"ready"}`)
}

// methodOnly rejects other methods with 405 instead of silently serving or
// 404ing, which is what clients probing the surface expect.
func methodOnly(method string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	}
}
