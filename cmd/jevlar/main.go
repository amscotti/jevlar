// Command jevlar is the Jevlar gateway binary. It parses flags, installs
// the signal handler, loads and validates the TOML configuration, and hands
// the process to internal/app, which owns all wiring from there: it builds
// the runtime snapshot, binds the main and admin listeners, serves
// /healthz, /metrics, /admin/status and /admin/reload, orchestrates
// reloads and shuts down gracefully.
//
// Signals: the handler is installed before the configuration is loaded, so
// a signal arriving during start-up is caught rather than missed. SIGHUP
// reloads the configuration (a failed reload keeps the previous
// configuration serving and increments
// jevlar_config_reload_failures_total). The first SIGINT or SIGTERM starts
// a graceful shutdown bounded by server.shutdown_grace_ms; a second signal
// closes the listeners immediately without waiting for the grace period.
//
// Exit codes:
//
//	0  clean run and shutdown, or a successful --check / --version /
//	   keyhash / healthcheck
//	1  startup failure other than configuration or flag usage (for example,
//	   a bind failure), or an unhealthy healthcheck probe
//	2  configuration load or validation failure, or a flag-usage error (the
//	   Go flag convention); configuration errors are printed with their
//	   field paths, and nothing is bound
//
// Subcommands: `jevlar keyhash <token>` prints an inbound key hash, and
// `jevlar healthcheck --config <path>` probes the gateway's own /healthz
// (the distroless container has no shell or HTTP client).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/amscotti/jevlar/internal/app"
	"github.com/amscotti/jevlar/internal/config"
)

// version is the binary version, overridden at build time with
// -ldflags "-X main.version=..." and "dev" when built without injection.
var version = "dev"

// main wires run to the process and maps its result to the exit code.
func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the binary with the given arguments and returns the process
// exit code.
func run(args []string, stdout, stderr io.Writer) int {
	// Install the signal handler before anything that can take time —
	// flag parsing, config load, bind — so the pre-bind window cannot
	// swallow a shutdown or reload request. Serve consumes the channel.
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(signals)

	// keyhash is a subcommand, not a flag: `jevlar keyhash <token>`
	// prints the SHA-256 to configure in [auth] keys. It runs before
	// flag parsing so no --config is needed.
	if len(args) > 0 && args[0] == "keyhash" {
		return runKeyhash(args[1:], stdout, stderr)
	}
	// healthcheck is the distroless container's probe: the image has no
	// shell, curl or wget, so the binary checks its own /healthz.
	if len(args) > 0 && args[0] == "healthcheck" {
		return runHealthcheck(args[1:], stderr)
	}

	flags := flag.NewFlagSet("jevlar", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "path to the TOML configuration file")
	checkOnly := flags.Bool("check", false, "validate the configuration and exit: 0 valid, 2 invalid")
	showVersion := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if flags.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "jevlar: unexpected argument %q\n", flags.Arg(0))
		return exitUsage
	}
	if *showVersion {
		_, _ = fmt.Fprintf(stdout, "jevlar %s (%s)\n", version, runtime.Version())
		return exitOK
	}
	if *configPath == "" {
		_, _ = fmt.Fprintln(stderr, "jevlar: --config is required")
		return exitUsage
	}

	logger := slog.New(slog.NewJSONHandler(stderr, nil))

	cfg, err := config.Load(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "jevlar: configuration is invalid: %v\n", err)
		return exitConfig
	}
	if err := cfg.Validate(); err != nil {
		_, _ = fmt.Fprintf(stderr, "jevlar: configuration is invalid: %v\n", err)
		return exitConfig
	}
	logConfigWarnings(logger, cfg)

	if *checkOnly {
		_, _ = fmt.Fprintln(stdout, "configuration is valid")
		return exitOK
	}

	app.Version = version
	if err := app.Serve(context.Background(), cfg, *configPath, logger, signals); err != nil {
		return exitStartup
	}
	return exitOK
}

// runHealthcheck probes the gateway's own /healthz endpoint on the
// loopback address implied by the configuration. It exists for the
// distroless container, whose runtime has no shell or HTTP client.
func runHealthcheck(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("jevlar healthcheck", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "path to the TOML configuration file")
	if err := flags.Parse(args); err != nil || *configPath == "" {
		_, _ = fmt.Fprintln(stderr, "usage: jevlar healthcheck --config <path>")
		return exitUsage
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "jevlar: healthcheck: %v\n", err)
		return exitConfig
	}
	host, port, err := net.SplitHostPort(cfg.Server.Listen)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "jevlar: healthcheck: %v\n", err)
		return exitConfig
	}
	switch host {
	case "", "0.0.0.0", "::":
		// A wildcard bind is reachable on loopback.
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "jevlar: healthcheck: %v\n", err)
		return exitStartup
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(stderr, "jevlar: healthcheck: status %d\n", resp.StatusCode)
		return exitStartup
	}
	return exitOK
}

// runKeyhash prints the SHA-256 hex of the token argument. The token is
// never logged and never echoed outside the digest; a missing or extra
// argument is a usage error.
func runKeyhash(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: jevlar keyhash <token>")
		return exitUsage
	}
	sum := sha256.Sum256([]byte(args[0]))
	_, _ = fmt.Fprintln(stdout, hex.EncodeToString(sum[:]))
	return exitOK
}

// logConfigWarnings surfaces load-time warnings (file permissions, literal
// key values) on the boot logger so operators see them before the process
// goes quiet serving.
func logConfigWarnings(logger *slog.Logger, cfg *config.Config) {
	for _, w := range cfg.Warnings {
		logger.Warn("configuration warning", "finding", w.String())
	}
}

// exit codes for run. exitUsage follows the Go flag convention of using 2
// for usage errors, the same code as configuration failures.
const (
	exitOK      = 0
	exitStartup = 1
	exitConfig  = 2
	exitUsage   = 2
)
