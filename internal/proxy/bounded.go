package proxy

import (
	"errors"
	"io"
)

// ErrBodyTooLarge reports a request or response body above its cap.
var ErrBodyTooLarge = errors.New("body exceeds the configured cap")

// readBounded reads at most max bytes plus one, so an over-cap body is
// detected without buffering it. It never returns a partially consumed
// buffer together with ErrBodyTooLarge.
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, ErrBodyTooLarge
	}
	buf, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > limit {
		return nil, ErrBodyTooLarge
	}
	return buf, nil
}
