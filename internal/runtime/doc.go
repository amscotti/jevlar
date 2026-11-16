// Package runtime holds the immutable Runtime snapshot a request is served
// against: the validated configuration, the model-alias routing table, the
// immutable provider views with their resolved key material, and their
// borrowed transports, concurrency slots and key pools. Snapshots are
// swapped wholesale by internal/app; a request loads a snapshot once at
// admission and never observes a reload mid-flight. Runtime borrows
// transports from the app's process-lifetime registry — it never creates or
// closes them. It is the only package that composes config, keyring and
// health.
//
// This package owns the configuration hash: a SHA-256 over a canonical
// serialisation of the resolved configuration (see canonical.go), so
// comments and formatting never count as a change while any resolved
// non-secret value does. Secret values are deliberately excluded — the
// hash is published by /admin/status and must not become a verifier of
// key material.
package runtime
