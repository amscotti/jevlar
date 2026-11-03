// Package usage turns an upstream `usage` block into attributed counters
// and answers "what did this client spend?".
//
// Parsing is read-only and defensive: a response without a usage block is
// counted as a miss, never guessed at. The tracker is a small in-memory
// attribution table keyed by (client, provider, key, model); the durable
// rollup writer consumes Snapshot.
package usage

import (
	"encoding/json"
	"sync"
)

// Usage is one response's reported accounting.
type Usage struct {
	// InputTokens is usage.input_tokens.
	InputTokens int64
	// OutputTokens is usage.output_tokens.
	OutputTokens int64
	// Cost is usage.cost when the provider reports it (OpenRouter does).
	Cost float64
	// HasCost distinguishes a reported zero cost from an absent one.
	HasCost bool
}

// Total returns input plus output tokens.
func (u Usage) Total() int64 { return u.InputTokens + u.OutputTokens }

// wire is the minimal JSON shape the parser reads.
type wire struct {
	Usage *struct {
		InputTokens  *int64   `json:"input_tokens"`
		OutputTokens *int64   `json:"output_tokens"`
		Cost         *float64 `json:"cost"`
	} `json:"usage"`
}

// Parse extracts the usage block from a response body. It reports false
// when the body has no usage object or the object is not an object at
// all, so the caller can count a miss rather than record zeros.
//
// Only the `usage` member is decoded; the rest of the document is skipped
// by the JSON decoder and never retained.
func Parse(body []byte) (Usage, bool) {
	if len(body) == 0 {
		return Usage{}, false
	}
	var w wire
	if err := json.Unmarshal(body, &w); err != nil || w.Usage == nil {
		return Usage{}, false
	}
	u := Usage{}
	if w.Usage.InputTokens != nil {
		u.InputTokens = *w.Usage.InputTokens
	}
	if w.Usage.OutputTokens != nil {
		u.OutputTokens = *w.Usage.OutputTokens
	}
	if w.Usage.Cost != nil {
		u.Cost = *w.Usage.Cost
		u.HasCost = true
	}
	if w.Usage.InputTokens == nil && w.Usage.OutputTokens == nil && w.Usage.Cost == nil {
		return Usage{}, false
	}
	if u.InputTokens < 0 {
		u.InputTokens = 0
	}
	if u.OutputTokens < 0 {
		u.OutputTokens = 0
	}
	if u.Cost < 0 {
		u.Cost = 0
	}
	return u, true
}

// Key identifies one attribution row.
type Key struct {
	// Client is the inbound client id.
	Client string
	// Provider is the provider name that served the request.
	Provider string
	// APIKey is the provider key id that served the request; empty for
	// auth = "none" providers.
	APIKey string
	// Model is the model id actually sent upstream.
	Model string
}

// Counts is one row's accumulated totals.
type Counts struct {
	// Requests is the number of successful requests attributed to the row.
	Requests int64 `json:"requests"`
	// InputTokens is the accumulated input tokens.
	InputTokens int64 `json:"input_tokens"`
	// OutputTokens is the accumulated output tokens.
	OutputTokens int64 `json:"output_tokens"`
	// Cost is the accumulated reported cost.
	Cost float64 `json:"cost"`
}

// Tracker is the in-memory attribution table. Create one per process and
// share it; it is safe for concurrent use.
type Tracker struct {
	mu        sync.Mutex
	rows      map[Key]*Counts
	misses    int64
	decisions map[string]*DecisionCounts
}

// NewTracker returns an empty tracker.
func NewTracker() *Tracker {
	return &Tracker{
		rows:      make(map[Key]*Counts),
		decisions: make(map[string]*DecisionCounts),
	}
}

// Record adds one successful request to the row.
func (t *Tracker) Record(k Key, u Usage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	row := t.rows[k]
	if row == nil {
		row = &Counts{}
		t.rows[k] = row
	}
	row.Requests++
	row.InputTokens += u.InputTokens
	row.OutputTokens += u.OutputTokens
	if u.HasCost {
		row.Cost += u.Cost
	}
}

// RecordMiss counts one successful response that carried no usage block.
func (t *Tracker) RecordMiss() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.misses++
}

// Snapshot returns a copy of the current totals, keyed identically.
func (t *Tracker) Snapshot() (map[Key]Counts, int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[Key]Counts, len(t.rows))
	for k, v := range t.rows {
		out[k] = *v
	}
	return out, t.misses
}

// Reset clears the tracker. It exists for tests.
func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rows = make(map[Key]*Counts)
	t.misses = 0
	t.decisions = make(map[string]*DecisionCounts)
}
