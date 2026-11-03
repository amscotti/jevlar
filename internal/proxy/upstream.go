package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/amscotti/jevlar/internal/runtime"
)

// hopByHopHeaders are stripped in both directions per RFC 9110 §7.6.1.
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// stripHopByHop removes hop-by-hop headers from h, including any header
// named by the Connection header itself.
func stripHopByHop(h http.Header) {
	for _, name := range strings.Split(h.Get("Connection"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			h.Del(name)
		}
	}
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
}

// upstreamURL joins a provider origin with its configured path. The
// origin is validated to carry no path of its own, so this cannot produce
// a doubled prefix.
func upstreamURL(p *runtime.Provider) string {
	u := url.URL{Scheme: p.BaseURL.Scheme, Host: p.BaseURL.Host, Path: p.Path}
	return u.String()
}

// buildUpstreamRequest assembles one attempt from the buffered client
// body. The body is re-read from the buffer on every attempt, so a retry
// after a partial write still sends the whole request.
//
// The client's Authorization header never reaches the provider: it is
// replaced by the selected provider key (or removed for auth = "none").
func buildUpstreamRequest(
	ctx context.Context,
	p *runtime.Provider,
	body []byte,
	contentEncoding string,
	clientHeader http.Header,
	secret string,
	requestID string,
	attempt int,
) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL(p), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	copied := clientHeader.Clone()
	stripHopByHop(copied)
	copied.Del("Authorization")
	copied.Del("Host")
	copied.Del("Content-Length")
	// Jevlar control headers are for the gateway, not the provider.
	copied.Del("X-Provider")
	for name := range copied {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), "X-Jevlar-") {
			copied.Del(name)
		}
	}
	// Upstream responses are always requested uncompressed so the usage
	// block stays readable; the pooled transport disables auto-gzip too.
	copied.Set("Accept-Encoding", "identity")
	// The request body is forwarded with whatever encoding it arrived in,
	// except when Jevlar rewrote it: the rewritten document is sent decoded.
	if contentEncoding == "" {
		copied.Del("Content-Encoding")
	} else {
		copied.Set("Content-Encoding", contentEncoding)
	}
	req.Header = copied
	req.Header.Set("X-Request-Id", requestID)
	req.Header.Set("X-Jevlar-Attempt", strconv.Itoa(attempt))

	if p.Auth == "bearer" && secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	return req, nil
}

// copyResponseHeader copies the end-to-end headers of an upstream response
// into w's header map, dropping hop-by-hop and length-defining headers:
// Jevlar always writes the length it actually buffered.
func copyResponseHeader(dst, src http.Header) {
	for name, values := range src {
		canonical := http.CanonicalHeaderKey(name)
		if isHopByHop(canonical) || canonical == "Content-Length" {
			continue
		}
		// Jevlar's own correlation headers are authoritative: an upstream
		// echo must not duplicate or replace them.
		if canonical == "X-Request-Id" || strings.HasPrefix(canonical, "X-Jevlar-") {
			continue
		}
		for _, v := range values {
			dst.Add(canonical, v)
		}
	}
}

func isHopByHop(name string) bool {
	for _, h := range hopByHopHeaders {
		if http.CanonicalHeaderKey(h) == name {
			return true
		}
	}
	return false
}
