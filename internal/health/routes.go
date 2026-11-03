package health

import (
	"sync"
	"time"
)

// RouteKey identifies one (alias, provider) latency series.
type RouteKey struct {
	Alias    string
	Provider string
}

// routeStat is one exponentially weighted moving average of upstream
// latency, plus the sample count used to know whether it is meaningful.
type routeStat struct {
	ewma  time.Duration
	count int64
}

// routeState is the per-alias selection state: which provider currently
// leads, and how many requests have been considered for exploration.
type routeState struct {
	preferred string
	seq       uint64
	// alpha is the per-alias smoothing factor from the configuration; 0
	// means the default.
	alpha float64
}

// routes holds the process-lifetime routing telemetry that the "fastest"
// policy reads. It lives in the health registry so it survives reloads:
// the latency of a provider does not reset because the config changed.
type routes struct {
	mu    sync.Mutex
	stats map[RouteKey]*routeStat
	state map[string]*routeState
	alpha float64
}

func newRoutes() *routes {
	return &routes{
		stats: make(map[RouteKey]*routeStat),
		state: make(map[string]*routeState),
	}
}

// DefaultEwmaAlpha mirrors the configuration default, so telemetry is
// still sane before a configuration is attached.
const DefaultEwmaAlpha = 0.2

// setAlpha applies a reloaded EWMA smoothing factor.
func (r *routes) setAlpha(alpha float64) {
	if alpha <= 0 || alpha > 1 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alpha = alpha
}

// observe folds one upstream latency sample into the series.
func (r *routes) observe(alias, provider string, d time.Duration) {
	if alias == "" || provider == "" || d <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	alpha := r.alpha
	if st, ok := r.state[alias]; ok && st.alpha > 0 {
		alpha = st.alpha
	}
	if alpha <= 0 {
		alpha = DefaultEwmaAlpha
	}
	key := RouteKey{Alias: alias, Provider: provider}
	st := r.stats[key]
	if st == nil {
		st = &routeStat{}
		r.stats[key] = st
	}
	if st.count == 0 {
		st.ewma = d
	} else {
		st.ewma = time.Duration(float64(st.ewma)*(1-alpha) + float64(d)*alpha)
	}
	st.count++
}

// ewma returns the current estimate and whether any sample exists.
func (r *routes) ewma(alias, provider string) (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.stats[RouteKey{Alias: alias, Provider: provider}]
	if st == nil || st.count == 0 {
		return 0, false
	}
	return st.ewma, true
}

// order returns the candidate providers in the order the fastest policy
// wants them tried.
//
// The leader only changes when a challenger is faster by more than band,
// which is what stops two providers a millisecond apart from flapping on
// every request. Every exploreEvery-th request the leader is demoted for
// one request so a recovering provider can prove itself; the exploration
// is driven by a counter, not a background goroutine.
func (r *routes) order(alias string, candidates []string, band time.Duration, exploreEvery int, alpha float64) []string {
	if len(candidates) < 2 {
		return append([]string(nil), candidates...)
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	st := r.state[alias]
	if st == nil {
		st = &routeState{}
		r.state[alias] = st
	}
	if alpha > 0 {
		st.alpha = alpha
	}
	st.seq++

	// Exploration: replace the leader with a non-leader for one request, so
	// an unlucky leader cannot entrench itself. Rotation is by exploration
	// turn, not by raw sequence, so every challenger gets its turn and the
	// leader is never the explored slot.
	if exploreEvery > 0 && st.seq%uint64(exploreEvery) == 0 {
		return r.explorationOrder(candidates, st, exploreEvery)
	}

	// A provider with no measurement must be tried before a known one is
	// repeated: otherwise a slow incumbent is never displaced, because the
	// challenger never gets a sample. Unmeasured candidates are explored
	// round-robin until each has one.
	unmeasured := make([]string, 0, len(candidates))
	for _, p := range candidates {
		if _, ok := r.lookup(alias, p); !ok {
			unmeasured = append(unmeasured, p)
		}
	}
	if len(unmeasured) > 0 {
		st.preferred = unmeasured[st.seq%uint64(len(unmeasured))]
		return prefixed(st.preferred, candidates)
	}

	// Establish a leader from measurements, honouring the hysteresis band.
	var (
		best      string
		bestEWMA  time.Duration
		bestValid bool
	)
	for _, p := range candidates {
		d, ok := r.lookup(alias, p)
		if !ok {
			continue
		}
		if !bestValid || d < bestEWMA {
			best, bestEWMA, bestValid = p, d, true
		}
	}
	if !bestValid {
		return append([]string(nil), candidates...)
	}
	if st.preferred == "" || !contains(candidates, st.preferred) {
		st.preferred = best
	} else if best != st.preferred {
		cur, ok := r.lookup(alias, st.preferred)
		if !ok || cur-bestEWMA > band {
			st.preferred = best
		}
	}

	return prefixed(st.preferred, candidates)
}

// prefixed returns candidates with first moved to the front, preserving
// the relative order of the rest.
func prefixed(first string, candidates []string) []string {
	out := make([]string, 0, len(candidates))
	out = append(out, first)
	for _, p := range candidates {
		if p != first {
			out = append(out, p)
		}
	}
	return out
}

// explorationOrder returns candidates with a non-leader first. The
// challenger rotates once per exploration turn so every non-leader is
// tried before any repeats, and the leader is never chosen.
func (r *routes) explorationOrder(candidates []string, st *routeState, exploreEvery int) []string {
	challengers := make([]string, 0, len(candidates)-1)
	for _, p := range candidates {
		if p != st.preferred {
			challengers = append(challengers, p)
		}
	}
	if len(challengers) == 0 {
		return append([]string(nil), candidates...)
	}
	turn := st.seq / uint64(max(exploreEvery, 1))
	first := challengers[turn%uint64(len(challengers))]
	return prefixed(first, candidates)
}

// lookup is ewma without taking the lock (callers already hold it).
func (r *routes) lookup(alias, provider string) (time.Duration, bool) {
	st := r.stats[RouteKey{Alias: alias, Provider: provider}]
	if st == nil || st.count == 0 {
		return 0, false
	}
	return st.ewma, true
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ObserveRouteLatency records one upstream latency sample for the fastest
// policy. Callers pass the upstream round-trip, not the client-observed
// duration, so gateway overhead never leaks into the ranking.
func (r *Registry) ObserveRouteLatency(alias, provider string, d time.Duration) {
	r.routes.observe(alias, provider, d)
}

// RouteOrder returns the provider order the fastest policy wants for an
// alias. Candidates are the configured providers of that alias.
func (r *Registry) RouteOrder(alias string, candidates []string, band time.Duration, exploreEvery int, alpha float64) []string {
	return r.routes.order(alias, candidates, band, exploreEvery, alpha)
}

// RouteEWMA reports the current latency estimate for a provider, for tests
// and status output.
func (r *Registry) RouteEWMA(alias, provider string) (time.Duration, bool) {
	return r.routes.ewma(alias, provider)
}

// SetRouteAlpha applies a reloaded EWMA smoothing factor.
func (r *Registry) SetRouteAlpha(alpha float64) { r.routes.setAlpha(alpha) }
