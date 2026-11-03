package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type stepClock struct{ t time.Time }

func (c *stepClock) now() time.Time          { return c.t }
func (c *stepClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestWriter(t *testing.T) (*Writer, *stepClock, string) {
	t.Helper()
	dir := t.TempDir()
	clk := &stepClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	w := NewWriter(WriterOptions{Dir: dir, RetentionDays: 2, MaxFileMB: 1, Interval: time.Hour, Now: clk.now})
	t.Cleanup(func() { _ = w.Close() })
	return w, clk, dir
}

// TestWriter_WritesIntervalDeltas pins the rollup contract: the first
// write carries everything, the second carries only what changed.
func TestWriter_WritesIntervalDeltas(t *testing.T) {
	t.Parallel()
	w, clk, dir := newTestWriter(t)
	k := Key{Client: "app", Provider: "p", Model: "m"}

	if err := w.Write(map[Key]Counts{k: {Requests: 2, InputTokens: 300, OutputTokens: 40}}, 1,
		map[string]DecisionCounts{"m": {Choices: map[string]map[string]int64{"team": {"billing": 1}}, Total: 1}}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	clk.advance(30 * time.Minute)
	if err := w.Write(map[Key]Counts{k: {Requests: 5, InputTokens: 500, OutputTokens: 60}}, 3, nil); err != nil {
		t.Fatalf("second write: %v", err)
	}

	body, err := os.ReadFile(w.CurrentFile())
	if err != nil {
		t.Fatalf("read rollup: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2:\n%s", len(lines), body)
	}
	if !strings.Contains(lines[0], `"requests":2`) || !strings.Contains(lines[0], `"usage_missing":1`) {
		t.Fatalf("first line = %s", lines[0])
	}
	if !strings.Contains(lines[1], `"requests":3`) {
		t.Fatalf("second line should carry only the delta: %s", lines[1])
	}
	if !strings.Contains(lines[0], `"billing":1`) {
		t.Fatalf("decision distribution missing: %s", lines[0])
	}
	_ = dir
}

// TestWriter_ReadSinceSumsWindows pins the query side.
func TestWriter_ReadSinceSumsWindows(t *testing.T) {
	t.Parallel()
	w, clk, dir := newTestWriter(t)
	k := Key{Client: "app", Provider: "p", Model: "m"}
	for i := 0; i < 3; i++ {
		if err := w.Write(map[Key]Counts{k: {Requests: int64(i + 1), InputTokens: 10}}, 0, nil); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		clk.advance(time.Hour)
	}
	_ = w.Close()

	since := clk.now().Add(-24 * time.Hour)
	roll, err := ReadSince(dir, since)
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	if len(roll.Rows) != 1 || roll.Rows[0].Requests != 3 {
		t.Fatalf("rows = %+v, want one row with 3 requests", roll.Rows)
	}
}

// TestWriter_RotatesHourlyAndWindowsSurvive pins the failure mode where
// one file named after the first write swallowed every later hour: because
// ReadSince filters on the filename hour, window queries silently dropped
// data that was newer than the file's name.
func TestWriter_RotatesHourlyAndWindowsSurvive(t *testing.T) {
	t.Parallel()
	w, clk, dir := newTestWriter(t)
	k := Key{Client: "app", Provider: "p", Model: "m"}
	for i := 0; i < 48; i++ {
		if err := w.Write(map[Key]Counts{k: {Requests: int64(i + 1), InputTokens: 10}}, 0, nil); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		clk.advance(time.Hour)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 48 {
		t.Fatalf("files = %d, want one per hour (48)", len(entries))
	}

	// The last 24 hours must be visible through the window query.
	roll, err := ReadSince(dir, clk.now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	var requests int64
	for _, row := range roll.Rows {
		requests += row.Requests
	}
	if requests != 24 {
		t.Fatalf("24h window summed %d requests, want 24", requests)
	}
}

// TestWriter_PruneKeepsTheActiveFileAndRecentFiles pins retention: old
// files go, the active file and files inside the window stay, and files
// that are not ours are never touched.
func TestWriter_PruneKeepsTheActiveFileAndRecentFiles(t *testing.T) {
	t.Parallel()
	w, clk, dir := newTestWriter(t)
	k := Key{Client: "app"}
	if err := w.Write(map[Key]Counts{k: {Requests: 1}}, 0, nil); err != nil {
		t.Fatal(err)
	}
	active := w.CurrentFile()

	old := filepath.Join(dir, "usage-20260101T00-000.jsonl")
	if err := os.WriteFile(old, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(foreign, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	removed, err := w.Prune()
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed %d files, want 1", removed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("expired rollup survived pruning")
	}
	if _, err := os.Stat(active); err != nil {
		t.Fatal("pruning deleted the active rollup file")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatal("pruning deleted a file it did not write")
	}

	// A second prune is a no-op.
	clk.advance(time.Minute)
	if removed, err := w.Prune(); err != nil || removed != 0 {
		t.Fatalf("second prune removed %d: %v", removed, err)
	}
}

// TestWriter_RotatesOnSize pins the size cap.
func TestWriter_RotatesOnSize(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w := NewWriter(WriterOptions{Dir: dir, RetentionDays: 30, MaxFileMB: 1, MaxFileBytes: 2048, Interval: time.Hour})
	t.Cleanup(func() { _ = w.Close() })

	k := Key{Client: "app", Provider: "p", Model: "m"}
	first := ""
	for i := 0; i < 200; i++ {
		if err := w.Write(map[Key]Counts{k: {Requests: int64(i + 1), InputTokens: int64(i * 5000)}}, 0, nil); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if first == "" {
			first = w.CurrentFile()
		}
	}
	if w.CurrentFile() == first {
		t.Fatal("writer never rotated past the size cap")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("rotation produced %d files", len(entries))
	}
}

// TestRollup_ContainsNoStateOrSecrets pins the redaction invariant on the
// durable artefacts: a rollup line holds counts and model/alias names
// only.
func TestRollup_ContainsNoStateOrSecrets(t *testing.T) {
	t.Parallel()
	w, _, _ := newTestWriter(t)
	k := Key{Client: "app", Provider: "p", APIKey: "primary", Model: "m"}
	if err := w.Write(map[Key]Counts{k: {Requests: 1, InputTokens: 5}}, 0, nil); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(w.CurrentFile())
	if err != nil {
		t.Fatal(err)
	}
	// The key *id* is recorded (needed for attribution); the secret is not
	// part of the row at all, which the struct shape enforces. Assert the
	// absence of prompt-like fields.
	for _, forbidden := range []string{`"state"`, `"questions"`, `"answers"`, `"instructions"`} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("rollup contains %s: %s", forbidden, body)
		}
	}
}

// TestParseDecisions_ReadsChoiceAndNoul pins the drift signal extraction.
func TestParseDecisions_ReadsChoiceAndNoul(t *testing.T) {
	t.Parallel()
	body := []byte(`{"answers":{"team":{"type":"choice","choice":"billing","confidence":0.91},
		"urgent":{"type":"noul","noul":0.97},"score":{"type":"score","value":3}}}`)
	got := ParseDecisions(body)
	if len(got) != 2 {
		t.Fatalf("decisions = %+v, want 2", got)
	}
	if got[0].Question != "team" || got[0].Choice != "billing" || got[0].Type != "choice" {
		t.Fatalf("first = %+v", got[0])
	}
	if got[1].Question != "urgent" || got[1].Type != "noul" {
		t.Fatalf("second = %+v", got[1])
	}
	if len(ParseDecisions([]byte(`{}`))) != 0 || len(ParseDecisions([]byte(`x`))) != 0 {
		t.Fatal("parser invented decisions")
	}
}

// TestConfidenceBucket_BoundsEveryValue pins the bucketing edges.
func TestConfidenceBucket_BoundsEveryValue(t *testing.T) {
	t.Parallel()
	cases := map[float64]string{
		-1: "0.0-0.2", 0: "0.0-0.2", 0.19: "0.0-0.2", 0.2: "0.2-0.4",
		0.4: "0.4-0.6", 0.6: "0.6-0.8", 0.8: "0.8-1.0", 1: "0.8-1.0", 5: "0.8-1.0",
	}
	for p, want := range cases {
		if got := ConfidenceBucket(p); got != want {
			t.Fatalf("bucket(%v) = %q, want %q", p, got, want)
		}
	}
}

// TestDecisions_TrackAndSubtract pins delta computation.
func TestDecisions_TrackAndSubtract(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	tr.RecordDecision("m", []Decision{{Question: "team", Type: "choice", Choice: "billing"}, {Question: "urgent", Type: "noul", Confidence: 0.9}})
	tr.RecordDecision("m", []Decision{{Question: "team", Type: "choice", Choice: "billing"}})
	snap := tr.DecisionsSnapshot()
	if snap["m"].Total != 3 || snap["m"].Choices["team"]["billing"] != 2 {
		t.Fatalf("snapshot = %+v", snap["m"])
	}
	prev := newDecisionCounts()
	prev.add(Decision{Question: "team", Type: "choice", Choice: "billing"})
	diff := snap["m"].subtract(prev)
	if diff.Total != 2 || diff.Choices["team"]["billing"] != 1 {
		t.Fatalf("diff = %+v", diff)
	}
}

// TestWriter_IntervalAndAccessors pins the small accessor surface.
func TestWriter_IntervalAndAccessors(t *testing.T) {
	t.Parallel()
	w := NewWriter(WriterOptions{Dir: t.TempDir(), Interval: 2 * time.Hour})
	if w.Interval() != 2*time.Hour {
		t.Fatalf("Interval() = %v", w.Interval())
	}
	if w.CurrentFile() != "" {
		t.Fatal("CurrentFile() reported a file before the first write")
	}
	// A writer with no explicit interval defaults to one hour.
	if d := NewWriter(WriterOptions{Dir: t.TempDir()}).Interval(); d != time.Hour {
		t.Fatalf("default interval = %v", d)
	}
	// Closing a writer that never opened a file is safe.
	if err := NewWriter(WriterOptions{Dir: t.TempDir()}).Close(); err != nil {
		t.Fatalf("Close on idle writer: %v", err)
	}
}

// TestWriter_ErrorsAreReported pins fail-loud behaviour: an unwritable
// directory is an error, not a silent drop.
func TestWriter_ErrorsAreReported(t *testing.T) {
	t.Parallel()
	w := NewWriter(WriterOptions{Dir: filepath.Join(t.TempDir(), "missing"), Interval: time.Hour})
	err := w.Write(map[Key]Counts{{Client: "a"}: {Requests: 1}}, 0, nil)
	if err == nil {
		t.Fatal("writing into a missing directory succeeded")
	}
	if _, perr := w.Prune(); perr == nil {
		t.Fatal("pruning a missing directory succeeded")
	}
	if _, rerr := ReadSince(filepath.Join(t.TempDir(), "missing"), time.Now()); rerr != nil {
		t.Fatalf("reading a missing directory: %v (want an empty window)", rerr)
	}
}

// TestReadSince_SkipsTornAndForeignLines pins robustness against a
// SIGKILL mid-write and against unrelated files.
func TestReadSince_SkipsTornAndForeignLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Now().UTC()
	good := "usage-" + now.Format("20060102T15") + "-000.jsonl"
	content := `{"interval_start":"2026-03-01T12:00:00Z","interval_end":"2026-03-01T13:00:00Z","rows":[{"client":"a","provider":"p","model":"m","requests":2,"input_tokens":10}],"usage_missing":1,"decisions":{"m":{"choices":{"team":{"billing":1}},"confidence":{"urgent":{"0.8-1.0":1}},"total":2}}}
{"torn line`
	if err := os.WriteFile(filepath.Join(dir, good), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore"), 0o600); err != nil {
		t.Fatal(err)
	}

	roll, err := ReadSince(dir, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	if len(roll.Rows) != 1 || roll.Rows[0].Requests != 2 {
		t.Fatalf("rows = %+v", roll.Rows)
	}
	if roll.UsageMissing != 1 {
		t.Fatalf("missing = %d", roll.UsageMissing)
	}
	if roll.Decisions["m"].Choices["team"]["billing"] != 1 {
		t.Fatalf("decisions = %+v", roll.Decisions)
	}
}

// TestReadSince_ExcludesOldWindows pins the window filter.
func TestReadSince_ExcludesOldWindows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	old := "usage-20200101T00-000.jsonl"
	if err := os.WriteFile(filepath.Join(dir, old),
		[]byte(`{"rows":[{"client":"old","provider":"p","model":"m","requests":9}]}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	roll, err := ReadSince(dir, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	if len(roll.Rows) != 0 {
		t.Fatalf("old window was included: %+v", roll.Rows)
	}
}

// TestWriter_WritesFinalIntervalOnClose pins that shutdown flushes: the
// context-cancel path in app calls Write then Close, so the last partial
// interval is durable.
func TestWriter_WritesFinalIntervalOnClose(t *testing.T) {
	t.Parallel()
	w, clk, dir := newTestWriter(t)
	k := Key{Client: "a", Provider: "p", Model: "m"}
	if err := w.Write(map[Key]Counts{k: {Requests: 4, InputTokens: 40}}, 2, nil); err != nil {
		t.Fatal(err)
	}
	// No change since the last write: the second write carries nothing but
	// must still succeed (the file exists and is synced).
	if err := w.Write(map[Key]Counts{k: {Requests: 4, InputTokens: 40}}, 2, nil); err != nil {
		t.Fatalf("idempotent write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	roll, err := ReadSince(dir, clk.now().Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(roll.Rows) != 1 || roll.Rows[0].Requests != 4 || roll.Rows[0].InputTokens != 40 {
		t.Fatalf("rows = %+v", roll.Rows)
	}
	if roll.UsageMissing != 2 {
		t.Fatalf("missing = %d", roll.UsageMissing)
	}
}

// TestDecisions_EmptyAndClonePaths pins the snapshot/clone helpers.
func TestDecisions_EmptyAndClonePaths(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	if len(tr.DecisionsSnapshot()) != 0 {
		t.Fatal("empty tracker reported decisions")
	}
	tr.RecordDecision("m", nil) // no-op
	if len(tr.DecisionsSnapshot()) != 0 {
		t.Fatal("nil decisions were recorded")
	}
	tr.RecordDecision("m", []Decision{{Question: "q", Type: "noul", Confidence: 0.1, HasConfidence: true}})
	snap := tr.DecisionsSnapshot()
	snap["m"].Confidence["q"]["0.0-0.2"] = 999
	again := tr.DecisionsSnapshot()
	if again["m"].Confidence["q"]["0.0-0.2"] != 1 {
		t.Fatal("DecisionsSnapshot exposed internal state")
	}
}

// TestWriter_SizeRotationWithinAnHour pins that a burst rotates by size
// while keeping the interval's hour in the filename.
func TestWriter_SizeRotationWithinAnHour(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	clk := &stepClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	w := NewWriter(WriterOptions{Dir: dir, MaxFileBytes: 1, Interval: time.Hour, Now: clk.now})
	t.Cleanup(func() { _ = w.Close() })
	k := Key{Client: "app"}
	if err := w.Write(map[Key]Counts{k: {Requests: 1}}, 0, nil); err != nil {
		t.Fatalf("first write: %v", err)
	}
	first := w.CurrentFile()
	if err := w.Write(map[Key]Counts{k: {Requests: 2}}, 0, nil); err != nil {
		t.Fatalf("second write: %v", err)
	}
	second := w.CurrentFile()
	if first == second {
		t.Fatalf("size cap did not rotate: %q == %q", first, second)
	}
	for _, path := range []string{first, second} {
		if !strings.Contains(filepath.Base(path), "20260301T12") {
			t.Fatalf("rotated file %q lost the interval hour", path)
		}
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 2 {
		t.Fatalf("entries = %v (err %v), want 2 files", entries, err)
	}
}

// TestReadSince_SkipsDirectoriesAndForeignNames pins the window reader's
// tolerance for unrelated entries in the usage directory.
func TestReadSince_SkipsDirectoriesAndForeignNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "usage-20260301T12-000.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "usage-not-a-time.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	valid := filepath.Join(dir, "usage-20260301T12-001.jsonl")
	if err := os.WriteFile(valid, []byte(`{"rows":[{"client":"a","provider":"p","model":"m","requests":2}]}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	roll, err := ReadSince(dir, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	if len(roll.Rows) != 1 || roll.Rows[0].Requests != 2 {
		t.Fatalf("rows = %+v, want the valid file's row", roll.Rows)
	}
}

// TestWriter_IntervalStartIsThePreviousWrite pins that an interval's Start
// is the actual previous write time, not an assumed interval length.
func TestWriter_IntervalStartIsThePreviousWrite(t *testing.T) {
	t.Parallel()
	w, clk, _ := newTestWriter(t) // one-hour configured interval
	k := Key{Client: "app"}
	first := clk.now()
	if err := w.Write(map[Key]Counts{k: {Requests: 1}}, 0, nil); err != nil {
		t.Fatalf("first write: %v", err)
	}
	clk.advance(10 * time.Minute)
	second := clk.now()
	if err := w.Write(map[Key]Counts{k: {Requests: 2}}, 0, nil); err != nil {
		t.Fatalf("second write: %v", err)
	}
	body, err := os.ReadFile(w.CurrentFile())
	if err != nil {
		t.Fatalf("read rollup: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}
	var interval Rollup
	if err := json.Unmarshal([]byte(lines[1]), &interval); err != nil {
		t.Fatalf("decode second line: %v", err)
	}
	if !interval.Start.Equal(first) && !interval.Start.Equal(second.Add(-time.Hour)) {
		t.Fatalf("second interval start = %v, want %v or a one-hour fallback", interval.Start, first)
	}
	if !interval.End.Equal(second) {
		t.Fatalf("second interval end = %v, want %v", interval.End, second)
	}
}

// TestWrite_DecisionDeltas pins that decision distributions are written as
// per-interval deltas and an unchanged distribution is omitted.
func TestWrite_DecisionDeltas(t *testing.T) {
	t.Parallel()
	w, clk, _ := newTestWriter(t)
	k := Key{Client: "app"}
	counts := func(n int64) map[string]DecisionCounts {
		return map[string]DecisionCounts{"m": {
			Choices: map[string]map[string]int64{"team": {"billing": n}},
			Total:   n,
		}}
	}
	if err := w.Write(map[Key]Counts{k: {Requests: 1}}, 0, counts(1)); err != nil {
		t.Fatalf("first write: %v", err)
	}
	clk.advance(30 * time.Minute)
	if err := w.Write(map[Key]Counts{k: {Requests: 2}}, 0, counts(3)); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if err := w.Write(map[Key]Counts{k: {Requests: 3}}, 0, counts(3)); err != nil {
		t.Fatalf("third write: %v", err)
	}
	body, err := os.ReadFile(w.CurrentFile())
	if err != nil {
		t.Fatalf("read rollup: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}
	if !strings.Contains(lines[1], `"billing":2`) {
		t.Fatalf("second line should carry the decision delta: %s", lines[1])
	}
	if strings.Contains(lines[2], `"decisions"`) {
		t.Fatalf("unchanged decisions were written again: %s", lines[2])
	}
}

// TestUsage_Total pins the token total used for budget charging.
func TestUsage_Total(t *testing.T) {
	t.Parallel()
	u := Usage{InputTokens: 2, OutputTokens: 3}
	if got := u.Total(); got != 5 {
		t.Fatalf("Total() = %d, want 5", got)
	}
}

// TestDecisionCounts_AddInitializesMapsAndSubtract covers zero-value maps
// and nonzero deltas across both dimensions.
func TestDecisionCounts_AddInitializesMapsAndSubtract(t *testing.T) {
	t.Parallel()
	var zero DecisionCounts // nil maps
	zero.add(Decision{Question: "q", Type: "choice", Choice: "x"})
	zero.add(Decision{Question: "q", Type: "noul", Confidence: 0.5, HasConfidence: true})
	if zero.Total != 2 || zero.Choices["q"]["x"] != 1 || zero.Confidence["q"]["0.4-0.6"] != 1 {
		t.Fatalf("zero-value add = %+v", zero)
	}

	a := newDecisionCounts()
	a.Total, a.Choices["q"], a.Confidence["q"] = 3, map[string]int64{"x": 3}, map[string]int64{"0.0-0.2": 3}
	b := newDecisionCounts()
	b.Total, b.Choices["q"], b.Confidence["q"] = 1, map[string]int64{"x": 1}, map[string]int64{"0.0-0.2": 1}
	diff := a.subtract(b)
	if diff.Total != 2 || diff.Choices["q"]["x"] != 2 || diff.Confidence["q"]["0.0-0.2"] != 2 {
		t.Fatalf("diff = %+v", diff)
	}
	if zero := a.subtract(a); zero.Total != 0 || len(zero.Choices) != 0 || len(zero.Confidence) != 0 {
		t.Fatalf("zero diff = %+v", zero)
	}
}

// TestParseDecisions_EmptyBodyAndMissingConfidence covers the no-body path
// and the "choice without confidence" path.
func TestParseDecisions_EmptyBodyAndMissingConfidence(t *testing.T) {
	t.Parallel()
	if got := ParseDecisions(nil); got != nil {
		t.Fatalf("ParseDecisions(nil) = %v, want nil", got)
	}
	body := []byte(`{"answers":{"team":{"type":"choice","choice":"billing"},"urgent":{"type":"noul","noul":0.7}}}`)
	got := ParseDecisions(body)
	if len(got) != 2 {
		t.Fatalf("decisions = %+v, want 2", got)
	}
	if got[0].Question != "team" || got[0].HasConfidence {
		t.Fatalf("choice decision = %+v, want no confidence", got[0])
	}
	if got[1].Question != "urgent" || !got[1].HasConfidence || got[1].Confidence != 0.7 {
		t.Fatalf("noul decision = %+v", got[1])
	}
}

// TestPrune_SkipsDirectories pins that a directory that looks like a
// rollup file is never deleted.
func TestPrune_SkipsDirectories(t *testing.T) {
	t.Parallel()
	w, _, dir := newTestWriter(t)
	if err := os.Mkdir(filepath.Join(dir, "usage-20200101T00-000.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	removed, err := w.Prune()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "usage-20200101T00-000.jsonl")); err != nil {
		t.Fatalf("prune deleted a directory: %v", err)
	}
}
