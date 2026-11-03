// Package app owns process boot: it loads and validates configuration,
// builds the immutable runtime snapshot, binds listeners, publishes the
// runtime pointer that the proxy and admin plane read, owns the
// process-lifetime transport registry, and orchestrates reload and graceful
// shutdown. All wiring lives here; no other package decides how Jevlar is
// assembled.
//
// Today the package owns the serve loop: Serve binds the configured
// listener, publishes /healthz and /metrics, and runs the two-stage
// shutdown. The remaining runtime snapshot, reload and transport-registry
// work extends this package.
package app
