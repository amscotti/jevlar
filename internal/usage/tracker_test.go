package usage

import "testing"

// TestParse_ReadsTheDocumentedPaths pins the exact JSON paths Jevlar
// understands: usage.input_tokens, usage.output_tokens and optional
// usage.cost.
func TestParse_ReadsTheDocumentedPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		body   string
		want   Usage
		wantOK bool
	}{
		{
			name:   "both token counts",
			body:   `{"model":"m","answers":{},"usage":{"input_tokens":312,"output_tokens":47}}`,
			want:   Usage{InputTokens: 312, OutputTokens: 47},
			wantOK: true,
		},
		{
			name:   "cost present",
			body:   `{"usage":{"input_tokens":1,"output_tokens":2,"cost":0.0012}}`,
			want:   Usage{InputTokens: 1, OutputTokens: 2, Cost: 0.0012, HasCost: true},
			wantOK: true,
		},
		{
			name:   "zero cost is still reported",
			body:   `{"usage":{"input_tokens":5,"cost":0}}`,
			want:   Usage{InputTokens: 5, HasCost: true},
			wantOK: true,
		},
		{
			name:   "only output tokens",
			body:   `{"usage":{"output_tokens":9}}`,
			want:   Usage{OutputTokens: 9},
			wantOK: true,
		},
		{
			name:   "negative values are clamped",
			body:   `{"usage":{"input_tokens":-5,"output_tokens":-7,"cost":-1}}`,
			want:   Usage{HasCost: true},
			wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := Parse([]byte(tc.body))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Fatalf("usage = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestParse_MissingUsageIsAMissNotAZero pins the no-guessing rule.
func TestParse_MissingUsageIsAMissNotAZero(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		``,
		`{}`,
		`{"answers":{}}`,
		`{"usage":null}`,
		`{"usage":{}}`,
		`{"usage":"not an object"}`,
		`not json at all`,
	} {
		if _, ok := Parse([]byte(body)); ok {
			t.Fatalf("body %q was reported as having usage", body)
		}
	}
}

// TestParse_IgnoresEverythingElse pins that the parser reads only usage:
// a document with unusual answer shapes still parses.
func TestParse_IgnoresEverythingElse(t *testing.T) {
	t.Parallel()
	body := `{"model":"m","answers":{"a":{"type":"choice","choice":"x","probabilities":{"x":1}}},
		"questions":{"q":{"type":"noul"}},"usage":{"input_tokens":10,"output_tokens":0}}`
	u, ok := Parse([]byte(body))
	if !ok || u.InputTokens != 10 {
		t.Fatalf("usage = %+v ok=%v", u, ok)
	}
}

// TestTracker_RecordsAndRollsUp pins attribution: rows are keyed by
// (client, provider, key, model) and repeated records accumulate.
func TestTracker_RecordsAndRollsUp(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	k := Key{Client: "app", Provider: "typesafe", APIKey: "primary", Model: "jev-latest"}
	tr.Record(k, Usage{InputTokens: 312, OutputTokens: 47})
	tr.Record(k, Usage{InputTokens: 100, OutputTokens: 3, Cost: 0.5, HasCost: true})
	tr.Record(Key{Client: "app", Provider: "ollama", Model: "nimble"}, Usage{InputTokens: 10})
	tr.RecordMiss()

	rows, misses := tr.Snapshot()
	if misses != 1 {
		t.Fatalf("misses = %d, want 1", misses)
	}
	got := rows[k]
	if got.Requests != 2 || got.InputTokens != 412 || got.OutputTokens != 50 || got.Cost != 0.5 {
		t.Fatalf("row = %+v", got)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}

	// The snapshot is a copy: mutating it must not touch the tracker.
	got.Requests = 999
	rows2, _ := tr.Snapshot()
	if rows2[k].Requests != 2 {
		t.Fatal("Snapshot exposed internal state")
	}
}

// TestTracker_Reset pins the test helper.
func TestTracker_Reset(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	tr.Record(Key{Client: "a"}, Usage{InputTokens: 1})
	tr.RecordMiss()
	tr.Reset()
	rows, misses := tr.Snapshot()
	if len(rows) != 0 || misses != 0 {
		t.Fatalf("after reset: %d rows, %d misses", len(rows), misses)
	}
}
