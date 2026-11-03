// Package testutil provides test doubles shared by Jevlar's unit,
// integration and end-to-end tests: a scriptable fake upstream provider and
// helpers for exercising the compiled binary over real sockets. Nothing in
// this package is imported by production code.
package testutil

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// FakeUpstream is a scriptable httptest.Server standing in for a decision
// model provider. It echoes the SHA-256 of each request body in the
// X-Body-Sha256 response header, records the last request it served, and can
// be scripted via SetStatus, SetDelay and SetBody to imitate provider
// failure modes. It is safe for concurrent use.
type FakeUpstream struct {
	server *httptest.Server

	mu         sync.Mutex
	status     int
	delay      time.Duration
	body       []byte
	handler    http.HandlerFunc
	requests   int
	lastBody   []byte
	lastHeader http.Header
}

// NewFakeUpstream starts a FakeUpstream on 127.0.0.1 and registers its
// shutdown with t. The default behaviour is 200 OK with an empty body.
func NewFakeUpstream(t testing.TB) *FakeUpstream {
	f := &FakeUpstream{status: http.StatusOK}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

// URL returns the upstream's origin, suitable for a provider base_url.
func (f *FakeUpstream) URL() string {
	return f.server.URL
}

// SetStatus scripts the HTTP status the upstream returns.
func (f *FakeUpstream) SetStatus(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = code
}

// SetDelay scripts how long the upstream holds a request before responding.
func (f *FakeUpstream) SetDelay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delay = d
}

// SetBody scripts the response body the upstream returns.
func (f *FakeUpstream) SetBody(b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body = b
}

// SetHandler replaces the scripted behaviour with an arbitrary handler,
// which is how tests imitate a mid-body close, a hang or a truncated
// response. Passing nil restores the scripted status/delay/body path.
func (f *FakeUpstream) SetHandler(h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = h
}

// Requests reports how many requests the upstream has served, which is how
// tests assert that a failover did or did not contact a second provider.
func (f *FakeUpstream) Requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// LastRequest returns the body and headers of the most recent request the
// upstream served.
func (f *FakeUpstream) LastRequest() (body []byte, header http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastBody, f.lastHeader.Clone()
}

// serve applies the scripted behaviour for one request.
func (f *FakeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	handler := f.handler
	f.requests++
	f.mu.Unlock()
	if handler != nil {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "fake upstream: read body: "+err.Error(), http.StatusInternalServerError)
			return
		}
		f.mu.Lock()
		f.lastBody = body
		f.lastHeader = r.Header.Clone()
		f.mu.Unlock()
		handler(w, r)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "fake upstream: read body: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256(body)

	f.mu.Lock()
	status := f.status
	delay := f.delay
	respBody := f.body
	f.lastBody = body
	f.lastHeader = r.Header.Clone()
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}

	w.Header().Set("X-Body-Sha256", hex.EncodeToString(sum[:]))
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
}
