package testutil

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestFakeUpstream_EchoesBodySha256ResponseHeader(t *testing.T) {
	t.Parallel()
	up := NewFakeUpstream(t)

	body := `{"model":"jev-latest","state":"s","questions":{}}`
	resp, err := http.Post(up.URL(), "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // assertion below reads what matters
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	sum := sha256.Sum256([]byte(body))
	if got, want := resp.Header.Get("X-Body-Sha256"), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("X-Body-Sha256 = %q, want %q", got, want)
	}
}

func TestFakeUpstream_RecordsTheLastRequest(t *testing.T) {
	t.Parallel()
	up := NewFakeUpstream(t)

	first := `{"n":1}`
	resp, err := http.Post(up.URL(), "application/json", strings.NewReader(first))
	if err != nil {
		t.Fatalf("post 1: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	second := `{"n":2}`
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, up.URL(), strings.NewReader(second))
	req.Header.Set("X-Marker", "second")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post 2: %v", err)
	}
	defer resp2.Body.Close() //nolint:errcheck // body not needed

	gotBody, gotHeader := up.LastRequest()
	if string(gotBody) != second {
		t.Fatalf("last body = %q, want %q", gotBody, second)
	}
	if gotHeader.Get("X-Marker") != "second" {
		t.Fatalf("last header X-Marker = %q, want %q", gotHeader.Get("X-Marker"), "second")
	}
}

func TestFakeUpstream_StatusDelayAndBodyAreScriptable(t *testing.T) {
	t.Parallel()
	up := NewFakeUpstream(t)
	up.SetStatus(http.StatusTooManyRequests)
	up.SetBody([]byte(`{"error":"rate_limited"}`))
	up.SetDelay(30 * time.Millisecond)

	start := time.Now()
	resp, err := http.Post(up.URL(), "application/json", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // body read below
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got := string(body); got != `{"error":"rate_limited"}` {
		t.Fatalf("body = %q, want scripted body", got)
	}
	if elapsed := time.Since(start); elapsed < 25*time.Millisecond {
		t.Fatalf("responded in %v, before the scripted delay", elapsed)
	}
}
