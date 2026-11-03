// Package keyring selects an upstream API key for an attempt: an O(1)
// power-of-two random choice among the provider's healthy keys, releasing
// the choice when the attempt completes. Key selection reads the health
// registry; the keyring owns no mutable state of its own and survives
// reloads. It may import only internal/health and the standard library.
package keyring
