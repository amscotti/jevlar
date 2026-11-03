package proxy

import (
	"errors"
	"fmt"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Content encodings Jevlar understands on a request body.
const (
	encodingIdentity = "identity"
	encodingZstd     = "zstd"
)

// ErrUnsupportedEncoding reports a Content-Encoding Jevlar cannot inspect.
var ErrUnsupportedEncoding = fmt.Errorf("unsupported Content-Encoding; use identity or zstd")

// ErrBadEncoding reports a body that claims an encoding it does not hold.
var ErrBadEncoding = errors.New("request body could not be decoded as zstd")

// ErrDecodedTooLarge reports a decoded body above the configured cap. It
// exists so a small compressed body cannot expand into an unbounded buffer
// (a decompression bomb).
var ErrDecodedTooLarge = errors.New("decoded request body exceeds the configured cap")

// zstdDecoder is created once: constructing a decoder allocates window
// buffers, and the proxy inspects every compressed request.
var zstdDecoder = func() *zstd.Decoder {
	dec, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(64<<20),
	)
	if err != nil {
		// A failure here is a build-time impossibility (the options are
		// constants); treat it as a programming error rather than silently
		// disabling decompression.
		panic("zstd decoder: " + err.Error())
	}
	return dec
}()

// decodeRequestBody returns the bytes Jevlar should inspect for the model
// id. The official SDK zstd-compresses request bodies by default once they
// reach 1 KiB, so this is the common path for real traffic, not an edge
// case.
//
// limit bounds the decoded size: the compressed body is already bounded,
// and without a decoded cap a small body could expand without bound.
func decodeRequestBody(raw []byte, contentEncoding string, limit int64) ([]byte, error) {
	encoding := strings.ToLower(strings.TrimSpace(contentEncoding))
	switch encoding {
	case "", encodingIdentity:
		return raw, nil
	case encodingZstd:
		if limit <= 0 {
			return nil, ErrDecodedTooLarge
		}
		decoded, err := zstdDecoder.DecodeAll(raw, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrBadEncoding, err)
		}
		if int64(len(decoded)) > limit {
			return nil, ErrDecodedTooLarge
		}
		return decoded, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedEncoding, encoding)
	}
}
