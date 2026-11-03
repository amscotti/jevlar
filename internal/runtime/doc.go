// Package runtime holds the immutable Runtime snapshot a request is served
// against: the validated configuration, the model-alias routing table, the
// immutable provider views and their borrowed transports. Snapshots are
// swapped wholesale by internal/app; a request loads a snapshot once at
// admission and never observes a reload mid-flight. Runtime borrows
// transports from the app's process-lifetime registry — it never creates or
// closes them. It is the only package that composes config, keyring and
// health. The implementation lands with the configuration-contract work;
// this package pins its contract only.
package runtime
