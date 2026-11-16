// Package app owns process boot: it loads and validates configuration,
// builds the immutable runtime snapshot, binds the main and admin listeners
// (serving /healthz, /readyz, /metrics, /v1/systemone, /v1/models and the
// admin plane), publishes the runtime pointer that the proxy and admin
// plane read, owns the process-lifetime transport registry, health registry
// and durable usage rollup loop, and orchestrates reload and graceful
// shutdown. All wiring lives here; no other package decides how Jevlar is
// assembled.
package app
