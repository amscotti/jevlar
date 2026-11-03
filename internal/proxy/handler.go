// Package proxy implements Jevlar's request path: admission, inbound
// authentication, bounded reading, target resolution, the attempt loop
// with its deadline budget, byte-preserving forwarding and
// commit-after-read response handling.
//
// The package owns no mutable cross-request state of its own: the health
// registry, key pools, transports and concurrency slots are borrowed from
// the runtime snapshot it loads once per request.
package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/amscotti/jevlar/internal/config"
	"github.com/amscotti/jevlar/internal/health"
	"github.com/amscotti/jevlar/internal/metrics"
	"github.com/amscotti/jevlar/internal/runtime"
	"github.com/amscotti/jevlar/internal/usage"
)

const (
	// attemptFloorMS is the smallest useful window for one attempt. Below
	// it the attempt cannot finish a real upstream call, so the request
	// ends 504 rather than burning a doomed try.
	attemptFloorMS = 100
	// defaultMaxAttempts caps the attempt loop when the alias does not
	// configure one.
	defaultMaxAttempts = 3
	// saturatedRetryAfterMS is the Retry-After hint when Jevlar is at its
	// concurrency bound.
	saturatedRetryAfterMS = 1000
	// forbiddenCooldown is how long a 403 cools a key: long enough to stop
	// hammering a plan-limited credential, short enough to recover without
	// an operator.
	forbiddenCooldown = 60 * time.Second
	// defaultAccountCooldown applies when a 429 arrives without a
	// Retry-After, so the account is not retried immediately.
	defaultAccountCooldown = 5 * time.Second
)

// Snapshot returns the runtime snapshot a request must be served against.
// It is called exactly once per request, so a concurrent reload cannot
// change behaviour mid-flight.
type Snapshot func() *runtime.Runtime

// Handler serves the System One surface. Create one per process with New
// and share it across requests; it is safe for concurrent use.
type Handler struct {
	rt      Snapshot
	log     *slog.Logger
	metric  *metrics.Metrics
	tracker *usage.Tracker

	inflight atomic.Int64
	seq      atomic.Uint64
	prefix   string

	// clock is used for tpm limit deadlines (injectable in tests).
	clock func() time.Time

	// now is injectable for deterministic tests.
	now func() time.Time
}

// New returns a Handler. rt must be non-nil and stable for the process
// lifetime; it is a function, not a pointer, so the handler always sees
// the currently published snapshot.
func New(rt Snapshot, log *slog.Logger, m *metrics.Metrics) *Handler {
	if log == nil {
		log = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	if m == nil {
		m = metrics.New(metrics.Options{Version: "unknown", GoVersion: "unknown"})
	}
	return &Handler{
		rt:      rt,
		log:     log,
		metric:  m,
		tracker: usage.NewTracker(),
		prefix:  bootPrefix(),
		now:     time.Now,
		clock:   time.Now,
	}
}

// bootPrefix returns the per-process half of every request id: eight hex
// characters drawn from the OS CSPRNG. A failure to read randomness is
// not fatal — ids are diagnostic, not security tokens — so it degrades to
// a fixed prefix.
func bootPrefix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b[:])
}

// newRequestID returns "<boot-prefix>-<counter>", unique per process and
// monotonic.
func (h *Handler) newRequestID() string {
	return fmt.Sprintf("%s-%d", h.prefix, h.seq.Add(1))
}

// ServeSystemOne handles POST /v1/systemone.
func (h *Handler) ServeSystemOne(w http.ResponseWriter, r *http.Request) {
	started := h.now()
	reqID := h.newRequestID()
	w.Header().Set("X-Request-Id", reqID)
	// The official SDK surfaces this header as error.request_id and uses it
	// for support requests; give it a value on every response, including
	// errors Jevlar generates before any upstream call.
	w.Header().Set("X-TypeSafe-Request-Id", reqID)

	defer func() {
		if rec := recover(); rec != nil {
			h.log.Error("panic in request path", "request_id", reqID, "panic", fmt.Sprint(rec))
			writeError(w, http.StatusInternalServerError, TypeInternal, "internal error", reqID, 0)
		}
	}()

	rt := h.rt()
	if rt == nil {
		writeError(w, http.StatusServiceUnavailable, TypeUnavailable, "no configuration loaded", reqID, saturatedRetryAfterMS)
		return
	}
	srvCfg := rt.Config.Server

	maxInflight := int64(srvCfg.MaxInflight)
	now := h.inflight.Add(1)
	if maxInflight > 0 && now > maxInflight {
		h.inflight.Add(-1)
		writeError(w, http.StatusServiceUnavailable, TypeCapacityExhausted,
			"gateway is at its in-flight limit", reqID, saturatedRetryAfterMS)
		return
	}
	h.metric.InflightAdd(1)
	defer func() {
		h.inflight.Add(-1)
		h.metric.InflightAdd(-1)
	}()

	client, ok := h.authenticate(r, rt)
	if !ok {
		h.countRequest("unknown", "-", "unknown", "401")
		writeError(w, http.StatusUnauthorized, TypeUnauthorized, "missing or unknown client key", reqID, 0)
		return
	}

	// Client budgets. rpm is exact and pre-flight, so an over-budget client
	// never reaches a provider; tpm is post-hoc, so a client that overspent
	// its minute is refused on its next request with the remaining wait.
	if reg := rt.Health; reg != nil {
		if c := reg.Client(client.ID); c != nil {
			if limited, until := c.LimitedUntil(); limited {
				h.metric.ClientLimited(client.ID, "tpm")
				h.countRequest(client.ID, "-", "unknown", "429")
				writeError(w, http.StatusTooManyRequests, TypeQuotaExceeded,
					"client token budget exhausted", reqID, time.Until(until).Milliseconds())
				return
			}
			if ok, wait := c.TakeRequest(); !ok {
				h.metric.ClientLimited(client.ID, "rpm")
				h.countRequest(client.ID, "-", "unknown", "429")
				writeError(w, http.StatusTooManyRequests, TypeQuotaExceeded,
					"client request budget exhausted", reqID, wait.Milliseconds())
				return
			}
		}
	}

	maxBody := srvCfg.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = config.DefaultMaxBodyBytes
	}
	if r.ContentLength > maxBody {
		h.countRequest(client.ID, "-", "unknown", "413")
		writeError(w, http.StatusRequestEntityTooLarge, TypeInvalidRequest,
			fmt.Sprintf("request body exceeds %d bytes", maxBody), reqID, 0)
		return
	}
	body, err := readBounded(r.Body, maxBody)
	if errors.Is(err, ErrBodyTooLarge) {
		h.countRequest(client.ID, "-", "unknown", "413")
		writeError(w, http.StatusRequestEntityTooLarge, TypeInvalidRequest,
			fmt.Sprintf("request body exceeds %d bytes", maxBody), reqID, 0)
		return
	}
	if err != nil {
		h.countRequest(client.ID, "-", "unknown", "400")
		writeError(w, http.StatusBadRequest, TypeInvalidRequest, "could not read request body", reqID, 0)
		return
	}

	// The debugging switch from the configuration: request bodies contain
	// operator-visible prompt state, so this is gated on log_bodies AND
	// debug level (validation enforces the pair) and never enabled by
	// default.
	if srvCfg.LogBodies {
		h.log.Debug("request body", "request_id", reqID, "client", client.ID, "body", string(body))
	}

	// The model is located according to the body's encoding: a plain JSON
	// document, or the JSON "request" part of a multipart upload (which is
	// what the official SDK sends as soon as a request carries an image).
	// The body may be compressed: the official SDK zstd-compresses request
	// bodies by default at 1 KiB, so this is the ordinary path for real
	// traffic. The raw bytes stay the source of truth for forwarding; the
	// decoded copy is only read for routing.
	contentEncoding := r.Header.Get("Content-Encoding")
	inspectBody, err := decodeRequestBody(body, contentEncoding, maxBody)
	if errors.Is(err, ErrDecodedTooLarge) {
		h.countRequest(client.ID, "-", "unknown", "413")
		writeError(w, http.StatusRequestEntityTooLarge, TypeInvalidRequest,
			fmt.Sprintf("decoded request body exceeds %d bytes", maxBody), reqID, 0)
		return
	}
	if err != nil {
		h.countRequest(client.ID, "-", "unknown", "400")
		writeError(w, http.StatusBadRequest, TypeInvalidRequest, err.Error(), reqID, 0)
		return
	}

	span, err := FindModelByMediaType(inspectBody, r.Header.Get("Content-Type"))
	if err != nil {
		h.countRequest(client.ID, "-", "unknown", "400")
		writeError(w, http.StatusBadRequest, TypeInvalidRequest, err.Error(), reqID, 0)
		return
	}

	targets := resolveTargets(rt, span.Value)
	if len(targets) == 0 {
		h.countRequest(client.ID, "-", metricModel(rt, span.Value), "404")
		writeError(w, http.StatusNotFound, TypeNotFound,
			fmt.Sprintf("model %q has no configured target", span.Value), reqID, 0)
		return
	}

	// X-Provider pins a request to one target of this alias. It is an
	// operator/debugging affordance, so it is validated against the alias's
	// own targets rather than the whole configuration: pinning must not
	// bypass the routing table.
	if pinned := strings.TrimSpace(r.Header.Get("X-Provider")); pinned != "" {
		var err error
		targets, err = pinTargets(targets, pinned)
		if err != nil {
			h.countRequest(client.ID, "-", metricModel(rt, span.Value), "400")
			writeError(w, http.StatusBadRequest, TypeInvalidRequest, err.Error(), reqID, 0)
			return
		}
	}

	// Fastest policy reorders the remaining targets using process-lifetime
	// latency telemetry. Priority keeps configuration order.
	targets = h.orderTargets(rt, span.Value, targets)

	out := h.runAttempts(r.Context(), rt, targets, span.Value, span, body, inspectBody, contentEncoding, r.Header, reqID)
	h.finish(w, rt, out, reqID, client.ID, span.Value, started)
}

// runAttempts executes the attempt loop under one request deadline and
// returns the outcome the client should see.
func (h *Handler) runAttempts(
	parent context.Context,
	rt *runtime.Runtime,
	targets []runtime.Target,
	requestedModel string,
	span ModelSpan,
	body []byte,
	inspectBody []byte,
	contentEncoding string,
	clientHeader http.Header,
	reqID string,
) attemptOutcome {
	requestTimeout := time.Duration(rt.Config.Server.RequestTimeoutMs) * time.Millisecond
	if requestTimeout <= 0 {
		requestTimeout = 30 * time.Second
	}
	deadline := time.Now().Add(requestTimeout)

	maxAttempts := attemptsFor(rt, requestedModel, len(targets))

	var (
		out           attemptOutcome
		lastPassthru  *attemptOutcome
		retryAfterMS  int64
		lastStatus    int
		attempts      int
		capacitySkips int
		realFailures  int
		rateCaused    bool
	)

	// The loop walks targets, but a key-scoped failure (429/401/403 on one
	// key) retries the *same* target with another key from the pool before
	// moving on: that is the whole point of holding several keys for one
	// provider.
	// max_attempts is the budget, so when it exceeds the number of targets
	// the loop revisits them: a transient 5xx on the last target should not
	// fail a request that still has budget left.
	targetIdx := 0
	// A skipped target (open breaker, cooling account) consumes no attempt,
	// so the loop is also bounded by iterations to guarantee termination
	// even when nothing is usable.
	maxIterations := maxAttempts + len(targets)
	for iter := 0; attempts < maxAttempts && len(targets) > 0 && iter < maxIterations; iter++ {
		remaining := time.Until(deadline)
		if remaining < attemptFloorMS*time.Millisecond {
			out = attemptOutcome{
				kind: outcomeRequestDeadline, status: http.StatusGatewayTimeout, errType: TypeUnavailable,
				message: "request deadline expired before an attempt could run",
			}
			break
		}
		target := targets[targetIdx%len(targets)]
		p := rt.Providers[target.Provider]
		if p == nil {
			targetIdx++
			continue
		}

		// Circuit breaker: an open provider is skipped without an attempt.
		br := breakerFor(rt, p)
		if br != nil && !br.Allow() {
			h.metric.Attempt(p.Name, "breaker_open")
			h.syncBreaker(p.Name, br)
			capacitySkips++
			if out.status == 0 {
				out = attemptOutcome{
					kind: outcomeError, status: http.StatusServiceUnavailable,
					errType: TypeCapacityExhausted, message: fmt.Sprintf("provider %q is not accepting traffic", p.Name),
				}
			}
			targetIdx++
			continue
		}

		// Account budget is enforced before key selection: provider limits
		// are per account, so a cooling account makes every key in it
		// useless for the moment.
		acc := accountFor(rt, p)
		if acc != nil {
			if cooling, until := acc.Cooling(); cooling {
				wait := time.Until(until).Milliseconds()
				if wait > retryAfterMS {
					retryAfterMS = wait
				}
				h.metric.Attempt(p.Name, "account_cooling")
				h.syncAccount(acc)
				capacitySkips++
				rateCaused = true
				if out.status == 0 {
					out = attemptOutcome{
						kind: outcomeError, status: http.StatusTooManyRequests,
						errType: TypeCapacityExhausted,
						message: fmt.Sprintf("account %q is cooling down", p.Account),
					}
				}
				targetIdx++
				continue
			}
			if ok, wait := acc.TakeRequest(); !ok {
				acc.CoolFor(wait)
				if ms := wait.Milliseconds(); ms > retryAfterMS {
					retryAfterMS = ms
				}
				h.metric.Attempt(p.Name, "account_limited")
				h.syncAccount(acc)
				capacitySkips++
				rateCaused = true
				if out.status == 0 {
					out = attemptOutcome{
						kind: outcomeError, status: http.StatusTooManyRequests,
						errType: TypeCapacityExhausted,
						message: fmt.Sprintf("account %q exceeded its request budget", p.Account),
					}
				}
				targetIdx++
				continue
			}
		}

		// This attempt plus every attempt that could still follow.
		n := 1 + min(maxAttempts-attempts-1, len(targets)-targetIdx%len(targets)-1)
		if n < 1 {
			n = 1
		}
		attemptTimeout := remaining / time.Duration(n)
		if p.Timeout > 0 && p.Timeout < attemptTimeout {
			attemptTimeout = p.Timeout
		}
		if attemptTimeout < attemptFloorMS*time.Millisecond {
			attemptTimeout = attemptFloorMS * time.Millisecond
		}

		// A target may name a different model than the client asked for:
		// splice exactly that value, leaving every other byte untouched.
		// Forwarding keeps the body exactly as received; only a rewrite
		// changes it, and a rewritten body is sent decoded (identity) since
		// the compressed bytes no longer describe the new document.
		attemptBody := body
		attemptEncoding := contentEncoding
		if target.Model != span.Value {
			rewritten, err := RewriteModel(inspectBody, span, target.Model)
			if err != nil {
				out = attemptOutcome{
					kind: outcomeError, status: http.StatusInternalServerError,
					errType: TypeInternal, message: "could not rewrite the model id",
				}
				realFailures++
				break
			}
			attemptBody = rewritten
			attemptEncoding = ""
		}

		attempts++
		res := h.attemptOnce(parent, rt, p, requestedModel, attemptBody, attemptEncoding, clientHeader, reqID, attempts, attemptTimeout)
		lastStatus = res.status
		if res.saturated && !res.keyScoped && br != nil {
			// No upstream call was made, so a half-open probe must be
			// abandoned rather than left holding the breaker.
			br.ReleaseProbe()
		}

		switch res.kind {
		case outcomeSuccess:
			res.attempts = attempts
			res.provider = p.Name
			res.model = target.Model
			return res
		case outcomePassthroughFinal:
			// 400/409/422: the request is wrong, not the provider. Pass the
			// upstream answer through unchanged and stop — no failover.
			res.attempts = attempts
			res.provider = p.Name
			res.model = target.Model
			return res
		case outcomeRetryable:
			if res.saturated {
				capacitySkips++
				rateCaused = rateCaused || res.rateCaused
			} else {
				realFailures++
			}
			if res.passthrough != nil {
				lastPassthru = res.passthrough
			}
			if res.retryAfterMS > retryAfterMS {
				retryAfterMS = res.retryAfterMS
			}
			out = attemptOutcome{kind: outcomeError, status: res.status, errType: TypeUnavailable, message: res.message}
			if res.saturated && res.status == http.StatusTooManyRequests {
				out.errType = TypeCapacityExhausted
			}
		}
		if res.kind == outcomeRequestDeadline {
			out = attemptOutcome{
				kind: outcomeRequestDeadline, status: http.StatusGatewayTimeout, errType: TypeUnavailable,
				message: "request deadline expired",
			}
			lastPassthru = nil
			break
		}
		// A key-scoped failure retries this target while the pool still has
		// an unused, available key.
		if res.keyScoped && p.Pool != nil && p.Pool.Len() > 1 && p.Pool.Available() {
			continue
		}
		targetIdx++
	}

	if lastPassthru != nil {
		// Every target answered non-retryably: the client gets the last
		// upstream answer unchanged.
		lastPassthru.attempts = attempts
		return *lastPassthru
	}
	if out.status == 0 {
		out = attemptOutcome{
			kind: outcomeError, status: http.StatusBadGateway, errType: TypeUnavailable,
			message: "no upstream target produced a response",
		}
	}
	// When no attempt reached an upstream because nothing had capacity, the
	// client is looking at a retryable gateway condition with a wait hint,
	// not at an upstream fault.
	if capacitySkips > 0 && realFailures == 0 {
		out.errType = TypeCapacityExhausted
		if rateCaused {
			out.status = http.StatusTooManyRequests
			if out.message == "" {
				out.message = "every upstream key is cooling down"
			}
		} else {
			out.status = http.StatusServiceUnavailable
			if out.message == "" {
				out.message = "no upstream capacity available"
			}
		}
		if out.retryAfterMS == 0 && retryAfterMS == 0 {
			out.retryAfterMS = saturatedRetryAfterMS
		}
	}
	if lastStatus == http.StatusTooManyRequests && out.errType == TypeUnavailable {
		out.errType = TypeCapacityExhausted
	}
	out.attempts = attempts
	if retryAfterMS > 0 {
		out.retryAfterMS = retryAfterMS
	}
	return out
}

// attemptOutcome is what one attempt (or the whole loop) produced.
type attemptOutcome struct {
	kind         outcomeKind
	status       int
	header       http.Header
	body         []byte
	provider     string
	model        string
	keyID        string
	attempts     int
	errType      string
	message      string
	retryAfterMS int64
	passthrough  *attemptOutcome
	// saturated reports that the target could not admit the request for
	// lack of a concurrency slot, a usable key, an account budget or a
	// closed breaker.
	saturated bool
	// rateCaused distinguishes a rate-limit stall (429) from a capacity
	// stall (503).
	rateCaused bool
	// keyScoped marks a failure caused by the selected key rather than the
	// provider, so the same target is retried with another key.
	keyScoped bool
}

type outcomeKind int

const (
	// outcomeError is the zero value on purpose: an outcome constructed
	// without an explicit kind must render as an error, never a success.
	outcomeError outcomeKind = iota
	outcomeSuccess
	// outcomePassthroughFinal is a non-retryable upstream rejection
	// (400/409/422): the client gets the upstream body unchanged.
	outcomePassthroughFinal
	outcomeRetryable
	outcomeRequestDeadline
)

// attemptOnce performs exactly one upstream call. The per-attempt context
// is cancelled when this function returns — never deferred to the caller —
// so a long request cannot accumulate attempt contexts.
func (h *Handler) attemptOnce(
	parent context.Context,
	rt *runtime.Runtime,
	p *runtime.Provider,
	alias string,
	body []byte,
	contentEncoding string,
	clientHeader http.Header,
	reqID string,
	attempt int,
	timeout time.Duration,
) attemptOutcome {
	if p.Slots != nil && !p.Slots.TryAcquire() {
		h.metric.Attempt(p.Name, "saturated")
		return attemptOutcome{
			kind: outcomeRetryable, status: http.StatusServiceUnavailable, saturated: true,
			message: fmt.Sprintf("provider %q is at its concurrency limit", p.Name),
		}
	}
	if p.Slots != nil {
		defer p.Slots.Release()
	}

	br := breakerFor(rt, p)
	acc := accountFor(rt, p)

	// Key acquisition. auth = "none" providers have no keys at all.
	var (
		key    *health.Key
		secret string
	)
	if p.Auth == "bearer" {
		if p.Pool == nil || p.Pool.Len() == 0 {
			h.metric.Attempt(p.Name, "no_key")
			return attemptOutcome{
				kind: outcomeRetryable, status: http.StatusServiceUnavailable, saturated: true,
				message: fmt.Sprintf("provider %q has no usable key", p.Name),
			}
		}
		k, ok := p.Pool.Acquire()
		if !ok {
			h.metric.Attempt(p.Name, "all_keys_cooling")
			res := attemptOutcome{
				kind: outcomeRetryable, status: http.StatusTooManyRequests, saturated: true, rateCaused: true, keyScoped: true,
				message: fmt.Sprintf("every key of provider %q is cooling or disabled", p.Name),
			}
			if until := p.Pool.EarliestCooldown(); !until.IsZero() {
				res.retryAfterMS = time.Until(until).Milliseconds()
			}
			return res
		}
		key = k
		defer p.Pool.Release(k)
		secret, _ = p.Secret(k.ID())
	}

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	req, err := buildUpstreamRequest(ctx, p, body, contentEncoding, clientHeader, secret, reqID, attempt)
	if err != nil {
		h.metric.UpstreamError(p.Name, "build")
		return attemptOutcome{kind: outcomeRetryable, status: http.StatusBadGateway, message: err.Error()}
	}

	client := &http.Client{Transport: p.Transport}
	upstreamStarted := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		if parent.Err() != nil {
			return attemptOutcome{
				kind: outcomeRequestDeadline, status: http.StatusGatewayTimeout,
				message: "request deadline expired",
			}
		}
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			h.metric.Attempt(p.Name, "timeout")
			h.metric.UpstreamError(p.Name, "timeout")
			if br != nil {
				br.RecordFailure()
				h.syncBreaker(p.Name, br)
			}
			return attemptOutcome{
				kind: outcomeRetryable, status: http.StatusGatewayTimeout,
				message: fmt.Sprintf("provider %q timed out", p.Name),
			}
		}
		h.metric.Attempt(p.Name, "transport_error")
		h.metric.UpstreamError(p.Name, "transport")
		if br != nil {
			br.RecordFailure()
			h.syncBreaker(p.Name, br)
		}
		return attemptOutcome{
			kind: outcomeRetryable, status: http.StatusBadGateway,
			message: fmt.Sprintf("provider %q request failed", p.Name),
		}
	}
	defer func() { _ = resp.Body.Close() }()

	limit := p.MaxResponseBytes
	if limit <= 0 {
		limit = 1_048_576
	}
	respBody, err := readBounded(resp.Body, limit)
	if errors.Is(err, ErrBodyTooLarge) {
		h.metric.ResponseTooLarge()
		h.metric.Attempt(p.Name, "too_large")
		h.metric.UpstreamError(p.Name, "too_large")
		if br != nil {
			br.RecordFailure()
			h.syncBreaker(p.Name, br)
		}
		return attemptOutcome{
			kind: outcomeRetryable, status: http.StatusBadGateway,
			message: fmt.Sprintf("provider %q response exceeded %d bytes", p.Name, limit),
		}
	}
	if err != nil {
		h.metric.Attempt(p.Name, "body_error")
		h.metric.UpstreamError(p.Name, "body")
		if br != nil {
			br.RecordFailure()
			h.syncBreaker(p.Name, br)
		}
		return attemptOutcome{
			kind: outcomeRetryable, status: http.StatusBadGateway,
			message: fmt.Sprintf("provider %q response could not be read", p.Name),
		}
	}

	// The fastest policy ranks providers by upstream round-trip only, so
	// gateway time never leaks into the ranking. Only answers that prove the
	// provider is working count: a fast 500 must not attract traffic.
	if rt.Health != nil && rankedResponse(resp.StatusCode) {
		rt.Health.ObserveRouteLatency(alias, p.Name, time.Since(upstreamStarted))
	}

	header := make(http.Header, len(resp.Header))
	copyResponseHeader(header, resp.Header)

	status := resp.StatusCode
	switch {
	case status >= 200 && status < 300:
		h.metric.Attempt(p.Name, "success")
		if key != nil {
			key.ResetConsecutive()
			h.syncKeyState(p.Name, key)
		}
		if br != nil {
			br.RecordSuccess()
			h.syncBreaker(p.Name, br)
		}
		out := attemptOutcome{kind: outcomeSuccess, status: status, header: header, body: respBody}
		if key != nil {
			out.keyID = key.ID()
		}
		return out
	case status == http.StatusBadRequest || status == http.StatusConflict || status == http.StatusUnprocessableEntity:
		// The request is wrong, not the provider: pass it through and do
		// not fail over. The provider is demonstrably healthy.
		h.metric.Attempt(p.Name, "passthrough")
		if br != nil {
			br.RecordSuccess()
			h.syncBreaker(p.Name, br)
		}
		return attemptOutcome{kind: outcomePassthroughFinal, status: status, header: header, body: respBody}
	case status == http.StatusNotFound:
		// This target does not serve the model; another may.
		h.metric.Attempt(p.Name, "not_found")
		if br != nil {
			br.RecordSuccess()
			h.syncBreaker(p.Name, br)
		}
		pt := &attemptOutcome{kind: outcomePassthroughFinal, status: status, header: header, body: respBody}
		return attemptOutcome{
			kind: outcomeRetryable, status: status, passthrough: pt,
			message: fmt.Sprintf("provider %q does not serve this model", p.Name),
		}
	case status == http.StatusTooManyRequests:
		h.metric.Attempt(p.Name, "rate_limited")
		h.metric.UpstreamError(p.Name, "rate_limited")
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		if key != nil {
			key.NoteRateLimited(retryAfter)
			h.syncKeyState(p.Name, key)
		}
		// Provider limits are per account: cool the whole account, not just
		// the key that happened to receive the 429.
		if acc != nil {
			cool := retryAfter
			if cool <= 0 {
				cool = defaultAccountCooldown
			}
			acc.CoolFor(cool)
			h.syncAccount(acc)
		}
		if br != nil {
			br.RecordSuccess() // the provider is up; this is a limit, not an outage
			h.syncBreaker(p.Name, br)
		}
		res := attemptOutcome{
			kind: outcomeRetryable, status: http.StatusTooManyRequests,
			message: "upstream rate limit", rateCaused: true, keyScoped: true,
		}
		if retryAfter > 0 {
			res.retryAfterMS = retryAfter.Milliseconds()
		}
		return res
	case status == http.StatusUnauthorized:
		h.metric.Attempt(p.Name, "auth_error")
		h.metric.UpstreamError(p.Name, "auth")
		if key != nil {
			key.Disable("upstream rejected the key (401); re-enable after fixing the credential")
			h.syncKeyState(p.Name, key)
		}
		if br != nil {
			br.RecordSuccess() // the provider answered; the credential is wrong
			h.syncBreaker(p.Name, br)
		}
		return attemptOutcome{
			kind: outcomeRetryable, status: http.StatusBadGateway, keyScoped: true,
			message: fmt.Sprintf("provider %q rejected its key", p.Name),
		}
	case status == http.StatusForbidden:
		// 403 is not proof of revocation: providers also use it for quota
		// and plan limits, so disable nothing and cool the key instead.
		h.metric.Attempt(p.Name, "forbidden")
		h.metric.UpstreamError(p.Name, "forbidden")
		h.metric.KeySuspect()
		if key != nil {
			key.CoolFor(forbiddenCooldown)
			h.syncKeyState(p.Name, key)
		}
		if acc != nil {
			acc.CoolFor(forbiddenCooldown)
			h.syncAccount(acc)
		}
		if br != nil {
			br.RecordSuccess()
			h.syncBreaker(p.Name, br)
		}
		return attemptOutcome{
			kind: outcomeRetryable, status: http.StatusBadGateway, keyScoped: true,
			message: fmt.Sprintf("provider %q forbade the request", p.Name),
		}
	default:
		h.metric.Attempt(p.Name, "status_error")
		h.metric.UpstreamError(p.Name, "status")
		if br != nil {
			br.RecordFailure()
			h.syncBreaker(p.Name, br)
		}
		return attemptOutcome{
			kind: outcomeRetryable, status: http.StatusBadGateway,
			message: fmt.Sprintf("provider %q returned %d", p.Name, status),
		}
	}
}

// breakerFor returns the provider's breaker, or nil when the registry has
// none (configuration without a breaker section).
func breakerFor(rt *runtime.Runtime, p *runtime.Provider) *health.Breaker {
	if rt.Health == nil {
		return nil
	}
	return rt.Health.Breaker(p.Name)
}

// accountFor returns the provider's account state, or nil when the
// provider is ungrouped.
func accountFor(rt *runtime.Runtime, p *runtime.Provider) *health.Account {
	if rt.Health == nil || p.Account == "" {
		return nil
	}
	return rt.Health.Account(p.Account)
}

// syncKeyState publishes a key's state as a gauge.
func (h *Handler) syncKeyState(provider string, k *health.Key) {
	if h.metric == nil {
		return
	}
	var v float64
	switch k.State() {
	case health.StateCooldown:
		v = 1
	case health.StateDisabled:
		v = 2
	}
	h.metric.KeyState(provider, k.ID(), v)
}

// syncBreaker publishes a breaker's state as a gauge.
func (h *Handler) syncBreaker(provider string, b *health.Breaker) {
	if h.metric == nil {
		return
	}
	var v float64
	if b.State() == health.StateCooldown {
		if b.Open() {
			v = 2
		} else {
			v = 1
		}
	}
	h.metric.BreakerState(provider, v)
}

// syncAccount publishes an account's remaining cooldown as a gauge.
func (h *Handler) syncAccount(a *health.Account) {
	if h.metric == nil {
		return
	}
	secs := 0.0
	if cooling, until := a.Cooling(); cooling {
		secs = time.Until(until).Seconds()
		if secs < 0 {
			secs = 0
		}
	}
	h.metric.AccountCooldown(a.Name(), secs)
}

// finish writes the outcome to the client and records the request line.
// The response is committed only once it is complete, so a mid-flight
// upstream failure can never look like a truncated 200.
func (h *Handler) finish(w http.ResponseWriter, rt *runtime.Runtime, out attemptOutcome, reqID, clientID, requestedModel string, started time.Time) {
	model := metricModel(rt, requestedModel)
	if out.provider == "" {
		out.provider = "-"
	}
	w.Header().Set("X-Jevlar-Attempts", fmt.Sprint(max(out.attempts, 1)))
	w.Header().Set("X-Jevlar-Model", model)
	if out.provider != "-" {
		w.Header().Set("X-Jevlar-Provider", out.provider)
	}

	if out.kind == outcomeSuccess || out.kind == outcomePassthroughFinal {
		for name, values := range out.header {
			for _, v := range values {
				w.Header().Add(name, v)
			}
		}
		// When the provider supplied its own request id, expose it as the
		// provider-side reference and keep Jevlar's as the primary id.
		if upstreamID := out.header.Get("X-TypeSafe-Request-Id"); upstreamID != "" && upstreamID != reqID {
			w.Header().Set("X-TypeSafe-Request-Id", reqID)
			w.Header().Set("X-TypeSafe-Upstream-Request-Id", upstreamID)
		} else {
			w.Header().Set("X-TypeSafe-Request-Id", reqID)
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(out.body)))
		w.WriteHeader(out.status)
		if _, err := w.Write(out.body); err != nil {
			h.log.Debug("client write failed", "request_id", reqID, "error", err)
		}
	} else {
		status := out.status
		if status == 0 {
			status = http.StatusBadGateway
		}
		typ := out.errType
		if typ == "" {
			typ = TypeUnavailable
		}
		writeError(w, status, typ, out.message, reqID, out.retryAfterMS)
	}

	// Usage accounting happens after the client response is committed so a
	// slow accounting path can never delay the caller, and a client abort
	// cannot lose the row.
	h.recordUsage(rt, out, clientID)

	duration := h.now().Sub(started)
	statusClass := statusClass(out.status)
	h.metric.Request(clientID, out.provider, model, statusClass)
	h.metric.RequestDuration(out.provider, model, float64(duration)/float64(time.Millisecond))

	keyID := out.keyID
	if keyID == "" {
		keyID = "-"
	}
	h.log.Info("request",
		"request_id", reqID,
		"client", clientID,
		"provider", out.provider,
		"key_id", keyID,
		"model_requested", requestedModel,
		"model", model,
		"status", out.status,
		"attempts", out.attempts,
		"duration_ms", duration.Milliseconds(),
		"inflight", h.inflight.Load(),
	)
}

// recordUsage attributes a successful response's tokens and cost, then
// charges the client and account token budgets. A response without a usage
// block is counted as a miss: Jevlar never guesses token counts.
func (h *Handler) recordUsage(rt *runtime.Runtime, out attemptOutcome, clientID string) {
	if out.kind != outcomeSuccess || h.tracker == nil {
		return
	}
	u, ok := usage.Parse(out.body)
	if !ok {
		h.tracker.RecordMiss()
		h.metric.UsageMissing()
		return
	}
	key := usage.Key{Client: clientID, Provider: out.provider, APIKey: out.keyID, Model: out.model}
	h.tracker.Record(key, u)
	h.tracker.RecordDecision(out.model, usage.ParseDecisions(out.body))
	h.metric.Usage(clientID, out.provider, out.model, u.InputTokens, u.OutputTokens, u.Cost)

	if u.Total() <= 0 || rt.Health == nil {
		return
	}
	// Post-hoc budgets: the client's minute is charged after the fact, so a
	// burst can overshoot by the in-flight requests' tokens. That bound is
	// documented and is the price of not blocking on estimated usage.
	if c := rt.Health.Client(clientID); c != nil {
		if ok, wait := c.TakeTokens(int(u.Total())); !ok {
			c.LimitFor(h.clock().Add(wait))
		}
	}
	// The account's token minute is charged as well: provider limits are per
	// account, so an over-budget account cools for the computed window.
	if p := rt.Providers[out.provider]; p != nil && p.Account != "" {
		if acc := rt.Health.Account(p.Account); acc != nil {
			if ok, wait := acc.TakeTokens(int(u.Total())); !ok {
				acc.CoolFor(wait)
				h.syncAccount(acc)
			}
		}
	}
}

// Tracker returns the usage tracker so the admin plane can render totals.
func (h *Handler) Tracker() *usage.Tracker { return h.tracker }

// countRequest records a request that failed before reaching an upstream.
func (h *Handler) countRequest(clientID, provider, model, status string) {
	if h.metric == nil {
		return
	}
	h.metric.Request(clientID, provider, model, status)
}

// authenticate resolves the presented bearer token to a configured client
// by hashing it once and looking the digest up. No secret is compared in a
// loop and no per-client state is touched here.
func (h *Handler) authenticate(r *http.Request, rt *runtime.Runtime) (*runtime.Client, bool) {
	token, ok := bearerTokenFrom(r.Header.Get("Authorization"))
	if !ok {
		return nil, false
	}
	digest := sha256.Sum256([]byte(token))
	client, found := rt.Clients[digest]
	if !found {
		return nil, false
	}
	return client, true
}

// bearerTokenFrom extracts a bearer credential case-insensitively.
func bearerTokenFrom(header string) (string, bool) {
	const scheme = "bearer "
	if len(header) <= len(scheme) || !equalFold(header[:len(scheme)], scheme) {
		return "", false
	}
	return header[len(scheme):], true
}

// equalFold compares two ASCII strings case-insensitively without
// allocating.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// resolveTargets returns the ordered targets for a requested model. A
// configured alias wins; otherwise every provider is tried in
// configuration order with the requested model unchanged, which is what
// keeps an unconfigured model usable across providers.
func resolveTargets(rt *runtime.Runtime, model string) []runtime.Target {
	if targets, ok := rt.Models[model]; ok && len(targets) > 0 {
		out := make([]runtime.Target, 0, len(targets))
		for _, t := range targets {
			if p := rt.Providers[t.Provider]; p != nil && providerUsable(p) {
				out = append(out, t)
			}
		}
		return out
	}
	out := make([]runtime.Target, 0, len(rt.Config.Providers))
	for i := range rt.Config.Providers {
		p := rt.Providers[rt.Config.Providers[i].Name]
		if p == nil || !providerUsable(p) {
			continue
		}
		out = append(out, runtime.Target{Provider: p.Name, Model: model})
	}
	return out
}

// providerUsable reports whether a provider can serve a request at all.
func providerUsable(p *runtime.Provider) bool {
	if p.Auth == "none" {
		return true
	}
	return len(p.Keys) > 0
}

// attemptsFor returns the attempt budget for a request. A configured alias
// carries its own max_attempts; an unconfigured model may try every
// provider once.
func attemptsFor(rt *runtime.Runtime, model string, targetCount int) int {
	if m, ok := rt.Config.Models[model]; ok && m.MaxAttempts > 0 {
		return m.MaxAttempts
	}
	if targetCount < defaultMaxAttempts {
		return targetCount
	}
	return defaultMaxAttempts
}

// parseRetryAfter parses a Retry-After value in seconds or as an
// HTTP-date. Absent or unparseable input yields 0 so the caller falls back
// to its own backoff.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// rankedResponse reports whether a response may inform the fastest-policy
// latency ranking: successes and request-shaped rejections prove the
// provider works, while rate limits, auth failures and server errors must
// not make a provider look attractive.
func rankedResponse(status int) bool {
	switch {
	case status >= 200 && status < 300:
		return true
	case status == http.StatusBadRequest,
		status == http.StatusNotFound,
		status == http.StatusConflict,
		status == http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

// pinTargets restricts the attempt list to one provider. The provider must
// be a target of this alias; pinning to something else is a client error,
// not a fallback.
func pinTargets(targets []runtime.Target, provider string) ([]runtime.Target, error) {
	for _, t := range targets {
		if t.Provider == provider {
			return []runtime.Target{t}, nil
		}
	}
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.Provider)
	}
	return nil, fmt.Errorf("X-Provider %q is not a target of this model (targets: %s)",
		provider, describeModelIDs(names))
}

// orderTargets applies the alias's routing policy. Under "fastest" the
// health registry's latency telemetry decides the order (with hysteresis
// and deterministic exploration); under "priority" configuration order is
// kept.
func (h *Handler) orderTargets(rt *runtime.Runtime, alias string, targets []runtime.Target) []runtime.Target {
	m, configured := rt.Config.Models[alias]
	if !configured || m.Policy != config.PolicyFastest || len(targets) < 2 || rt.Health == nil {
		return targets
	}
	candidates := make([]string, 0, len(targets))
	byName := make(map[string]runtime.Target, len(targets))
	for _, t := range targets {
		if _, dup := byName[t.Provider]; dup {
			continue
		}
		byName[t.Provider] = t
		candidates = append(candidates, t.Provider)
	}
	if len(candidates) < 2 {
		return targets
	}
	band := time.Duration(m.HysteresisMs) * time.Millisecond
	order := rt.Health.RouteOrder(alias, candidates, band, m.ExploreEvery, m.EwmaAlpha)
	out := make([]runtime.Target, 0, len(targets))
	for _, name := range order {
		if t, ok := byName[name]; ok {
			out = append(out, t)
		}
	}
	return out
}

// metricModel maps a requested model to its bounded metric label: the
// configured alias, or "unknown" for an unconfigured model. Client input
// never becomes a label value, so label cardinality is bounded by
// configuration.
func metricModel(rt *runtime.Runtime, model string) string {
	if _, ok := rt.Models[model]; ok {
		return model
	}
	for i := range rt.Config.Providers {
		for _, m := range rt.Config.Providers[i].Models {
			if m == model {
				return model
			}
		}
	}
	return "unknown"
}

func statusClass(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 400 && status < 500:
		return fmt.Sprint(status)
	case status >= 500 && status < 600:
		return "5xx"
	default:
		return "other"
	}
}

// Models handles GET /v1/models.
//
// The response carries two shapes in one document. `models` is the
// TypeSafe/System One shape the official SDK parses
// ({"models":[{"name","description","release_date"}]}); `data` is the
// OpenAI-style list most other clients expect. The SDK ignores unknown
// fields, so serving both is additive rather than a compatibility
// compromise — a gateway has exactly one list of what it can route, and
// two consumers that disagree on its spelling.
//
// Jevlar never invents provider metadata: an alias reports the description
// and release date configured for it (or a generated description naming
// its providers), and an unknown release date is an empty string rather
// than a plausible-looking fiction.
func (h *Handler) Models(w http.ResponseWriter, r *http.Request) {
	reqID := h.newRequestID()
	w.Header().Set("X-Request-Id", reqID)
	w.Header().Set("X-TypeSafe-Request-Id", reqID)

	rt := h.rt()
	if rt == nil {
		writeError(w, http.StatusServiceUnavailable, TypeUnavailable, "no configuration loaded",
			reqID, saturatedRetryAfterMS)
		return
	}
	// The model list names providers, so it is behind the same inbound
	// authentication as the request path.
	if _, ok := h.authenticate(r, rt); !ok {
		h.countRequest("unknown", "-", "unknown", "401")
		writeError(w, http.StatusUnauthorized, TypeUnauthorized, "missing or unknown client key", reqID, 0)
		return
	}
	type entry struct {
		name        string
		description string
		releaseDate string
	}
	entries := make(map[string]entry)

	// Aliases first: they are Jevlar's own names and carry configured
	// metadata.
	for alias, m := range rt.Config.Models {
		providers := make([]string, 0, len(m.Targets))
		for _, t := range m.Targets {
			providers = append(providers, t.Provider)
		}
		desc := m.Description
		if desc == "" {
			if len(providers) > 0 {
				desc = fmt.Sprintf("%s, routed by Jevlar to %s", alias, strings.Join(providers, ", "))
			} else {
				desc = alias + ", routed by Jevlar"
			}
		}
		entries[alias] = entry{name: alias, description: desc, releaseDate: m.ReleaseDate}
	}
	// Then every model a provider declares, so a caller can discover ids it
	// did not know to alias.
	for i := range rt.Config.Providers {
		p := rt.Config.Providers[i]
		for _, model := range p.Models {
			if _, ok := entries[model]; ok {
				continue
			}
			entries[model] = entry{
				name:        model,
				description: fmt.Sprintf("%s, served by provider %s", model, p.Name),
			}
		}
	}

	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)

	type sdkModel struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		ReleaseDate string `json:"release_date"`
	}
	type openAIModel struct {
		ID     string `json:"id"`
		Object string `json:"object"`
	}
	models := make([]sdkModel, 0, len(names))
	data := make([]openAIModel, 0, len(names))
	for _, name := range names {
		e := entries[name]
		models = append(models, sdkModel{Name: e.name, Description: e.description, ReleaseDate: e.releaseDate})
		data = append(data, openAIModel{ID: e.name, Object: "model"})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"models": models,
		"object": "list",
		"data":   data,
	})
}
