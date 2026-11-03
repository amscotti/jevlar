package runtime

import (
	"encoding/hex"
	"fmt"
)

// ParseClientHash decodes a 64-character hex SHA-256 client-key hash into
// its 32-byte form. Validation already guarantees the shape; this returns
// a fail-closed error rather than panicking when a caller skips
// validation.
func ParseClientHash(h string) ([32]byte, error) {
	var out [32]byte
	if len(h) != 64 {
		return out, fmt.Errorf("must be 64 hex characters")
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		return out, fmt.Errorf("must be 64 hex characters: %w", err)
	}
	copy(out[:], b)
	return out, nil
}
