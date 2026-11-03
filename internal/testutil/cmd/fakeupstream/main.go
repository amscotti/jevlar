// Command fakeupstream is a standalone scriptable decision-model provider
// used by the end-to-end, benchmark, soak and container smoke scripts. It
// speaks enough of the System One shape to be useful and echoes the
// SHA-256 of each request body in X-Body-Sha256 so a test can prove the
// gateway forwarded the bytes unchanged.
//
// It is deliberately self-contained (standard library only): it must be
// buildable into a scratch container without the rest of the module.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18090", "listen address")
	delay := flag.Duration("delay", 0, "artificial response delay")
	status := flag.Int("status", http.StatusOK, "HTTP status to return")
	bodySize := flag.Int("body-size", 0, "if positive, return a JSON body of about this many bytes")
	failEvery := flag.Int("fail-every", 0, "if positive, return 500 on every Nth request")
	rateLimitEvery := flag.Int("ratelimit-every", 0, "if positive, return 429 with Retry-After on every Nth request")
	retryAfter := flag.String("retry-after", "1", "Retry-After value used by -ratelimit-every")
	retryAfterMS := flag.Int("retry-after-ms", 0, "if positive, send Retry-After as an HTTP-date this many milliseconds in the future (exercises sub-second cooldown)")
	flag.Parse()

	const template = `{"model":"fake","answers":{"urgent":{"type":"noul","noul":0.75}},"usage":{"input_tokens":312,"output_tokens":8}}`

	var served atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/systemone", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		sum := sha256.Sum256(body)
		w.Header().Set("X-Body-Sha256", hex.EncodeToString(sum[:]))

		n := served.Add(1)
		if *delay > 0 {
			select {
			case <-time.After(*delay):
			case <-r.Context().Done():
				return
			}
		}
		if *rateLimitEvery > 0 && n%int64(*rateLimitEvery) == 0 {
			if *retryAfterMS > 0 {
				w.Header().Set("Retry-After", time.Now().Add(time.Duration(*retryAfterMS)*time.Millisecond).UTC().Format(http.TimeFormat))
			} else {
				w.Header().Set("Retry-After", *retryAfter)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"rate limited"}`)
			return
		}
		if *failEvery > 0 && n%int64(*failEvery) == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"injected failure"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(*status)
		if *bodySize > 0 {
			pad := *bodySize - len(template)
			if pad < 0 {
				pad = 0
			}
			_, _ = io.WriteString(w, strings.Replace(template, `"fake"`, `"fake"`, 1))
			_, _ = io.WriteString(w, `{"pad":"`)
			_, _ = io.WriteString(w, strings.Repeat("x", pad))
			_, _ = io.WriteString(w, `"}`)
			return
		}
		_, _ = io.WriteString(w, template)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/__stats", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"served":%d}`, served.Load())
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		_ = srv.Close()
	}()

	log.Printf("fakeupstream listening on %s", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("fakeupstream: %v", err)
	}
}
