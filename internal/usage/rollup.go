package usage

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Rollup is one interval's durable record. It contains counts only: no
// request state, no question text and no key material.
type Rollup struct {
	// Start is the interval start (UTC).
	Start time.Time `json:"interval_start"`
	// End is the interval end (UTC).
	End time.Time `json:"interval_end"`
	// Rows are the per-attribution deltas observed in the interval.
	Rows []RollupRow `json:"rows,omitempty"`
	// UsageMissing counts successful responses without a usage block.
	UsageMissing int64 `json:"usage_missing"`
	// Decisions are the per-model answer distribution deltas.
	Decisions map[string]DecisionCounts `json:"decisions,omitempty"`
}

// RollupRow is one attribution row's delta.
type RollupRow struct {
	Client       string  `json:"client"`
	Provider     string  `json:"provider"`
	APIKey       string  `json:"key,omitempty"`
	Model        string  `json:"model"`
	Requests     int64   `json:"requests"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	Cost         float64 `json:"cost"`
}

// WriterOptions configures a Writer.
type WriterOptions struct {
	// Dir is the directory rollups are written to; it must exist.
	Dir string
	// RetentionDays is how long rollup files are kept.
	RetentionDays int
	// MaxFileMB rotates the active file once it grows past this size.
	MaxFileMB int
	// MaxFileBytes overrides MaxFileMB when positive. It exists so tests
	// can exercise rotation without writing a megabyte.
	MaxFileBytes int64
	// Interval is the rollup period; it defaults to one hour.
	Interval time.Duration
	// Now is injectable for tests.
	Now func() time.Time
}

// Writer appends interval deltas to a JSONL file and prunes old files.
// It is single-writer by design: one process owns its usage directory.
type Writer struct {
	dir           string
	retentionDays int
	maxFileBytes  int64
	interval      time.Duration
	now           func() time.Time

	last     map[Key]Counts
	lastMiss int64
	lastDec  map[string]DecisionCounts
	// lastWrite is when the previous interval was recorded, so the next
	// interval's Start is the real time since then rather than an assumed
	// interval length (ticker drift, sleep, shutdown flush).
	lastWrite time.Time

	file    *os.File
	fileSeq int
	started time.Time
}

// NewWriter returns a Writer. It does not touch the filesystem until the
// first Write.
func NewWriter(opts WriterOptions) *Writer {
	interval := opts.Interval
	if interval <= 0 {
		interval = time.Hour
	}
	retention := opts.RetentionDays
	if retention <= 0 {
		retention = 30
	}
	maxMB := opts.MaxFileMB
	if maxMB <= 0 {
		maxMB = 256
	}
	maxBytes := opts.MaxFileBytes
	if maxBytes <= 0 {
		maxBytes = int64(maxMB) * 1024 * 1024
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Writer{
		dir:           opts.Dir,
		retentionDays: retention,
		maxFileBytes:  maxBytes,
		interval:      interval,
		now:           now,
		last:          map[Key]Counts{},
		lastDec:       map[string]DecisionCounts{},
	}
}

// Interval reports the configured rollup period.
func (w *Writer) Interval() time.Duration { return w.interval }

// Write appends one interval line built from the tracker's current totals
// minus whatever was already written, fsyncs it, and rotates the file when
// the hour changes or it grew past the size cap.
func (w *Writer) Write(rows map[Key]Counts, misses int64, decisions map[string]DecisionCounts) error {
	now := w.now().UTC()
	start := now.Add(-w.interval)
	if !w.lastWrite.IsZero() {
		start = w.lastWrite
	}
	delta := Rollup{Start: start, End: now, Decisions: map[string]DecisionCounts{}}

	for k, c := range rows {
		prev := w.last[k]
		d := RollupRow{
			Client:       k.Client,
			Provider:     k.Provider,
			APIKey:       k.APIKey,
			Model:        k.Model,
			Requests:     c.Requests - prev.Requests,
			InputTokens:  c.InputTokens - prev.InputTokens,
			OutputTokens: c.OutputTokens - prev.OutputTokens,
			Cost:         c.Cost - prev.Cost,
		}
		if d.Requests <= 0 && d.InputTokens <= 0 && d.OutputTokens <= 0 && d.Cost <= 0 {
			continue
		}
		delta.Rows = append(delta.Rows, d)
	}
	sort.Slice(delta.Rows, func(i, j int) bool {
		a, b := delta.Rows[i], delta.Rows[j]
		if a.Client != b.Client {
			return a.Client < b.Client
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.APIKey != b.APIKey {
			return a.APIKey < b.APIKey
		}
		return a.Model < b.Model
	})
	delta.UsageMissing = misses - w.lastMiss
	for model, d := range decisions {
		prev, ok := w.lastDec[model]
		if !ok {
			prev = newDecisionCounts()
		}
		if diff := d.subtract(prev); diff.Total != 0 {
			delta.Decisions[model] = diff
		}
	}

	line, err := json.Marshal(delta)
	if err != nil {
		return fmt.Errorf("encode rollup: %w", err)
	}
	if err := w.append(line, now); err != nil {
		return err
	}

	w.last = make(map[Key]Counts, len(rows))
	for k, v := range rows {
		w.last[k] = v
	}
	w.lastMiss = misses
	w.lastDec = cloneDecisions(decisions)
	w.lastWrite = now
	return nil
}

// append writes one line to the active file, rotating first when the hour
// changed (each file's name encodes its interval hour, which is what
// ReadSince filters on) or when the file grew past the size cap.
func (w *Writer) append(line []byte, now time.Time) error {
	if w.file == nil {
		if err := w.open(now); err != nil {
			return err
		}
	} else if !now.Truncate(time.Hour).Equal(w.started.Truncate(time.Hour)) {
		if err := w.rotate(now); err != nil {
			return err
		}
	}
	if info, err := w.file.Stat(); err == nil && info.Size() > w.maxFileBytes {
		if err := w.rotate(now); err != nil {
			return err
		}
	}
	if _, err := w.file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write rollup: %w", err)
	}
	if err := w.file.Sync(); err != nil {
		return fmt.Errorf("fsync rollup: %w", err)
	}
	return nil
}

// open opens (or creates) the active file for the current interval.
func (w *Writer) open(now time.Time) error {
	w.started = now
	name := fmt.Sprintf("usage-%s-%03d.jsonl", w.started.Format("20060102T15"), w.fileSeq)
	path := filepath.Join(w.dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // path is built from the operator-configured usage directory and a generated filename
	if err != nil {
		return fmt.Errorf("open rollup file: %w", err)
	}
	w.file = f
	return nil
}

// rotate closes the active file and starts a new sequence number, so a
// burst cannot produce one unbounded file. The sequence resets when the
// hour changes; within one hour it disambiguates size rotations.
func (w *Writer) rotate(now time.Time) error {
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return fmt.Errorf("close rollup file: %w", err)
		}
		w.file = nil
	}
	if now.Truncate(time.Hour).Equal(w.started.Truncate(time.Hour)) {
		w.fileSeq++
	} else {
		w.fileSeq = 0
	}
	return w.open(now)
}

// Close flushes and closes the active file.
func (w *Writer) Close() error {
	if w.file == nil {
		return nil
	}
	err := w.file.Sync()
	if cerr := w.file.Close(); err == nil {
		err = cerr
	}
	w.file = nil
	return err
}

// CurrentFile returns the path of the active file, or "" when none is
// open. It exists so tests and operators can name the live file, which
// retention must never delete.
func (w *Writer) CurrentFile() string {
	if w.file == nil {
		return ""
	}
	return w.file.Name()
}

// ErrBadRollupName reports a file in the usage directory that does not
// follow the rollup naming scheme. Such files are ignored, never deleted.
var ErrBadRollupName = errors.New("not a rollup file")

// rollupTime parses the timestamp out of a rollup filename.
func rollupTime(name string) (time.Time, error) {
	if !strings.HasPrefix(name, "usage-") || !strings.HasSuffix(name, ".jsonl") {
		return time.Time{}, ErrBadRollupName
	}
	base := strings.TrimSuffix(strings.TrimPrefix(name, "usage-"), ".jsonl")
	// base is YYYYMMDDTHH-NNN. The sequence suffix is optional.
	if i := strings.LastIndexByte(base, '-'); i > 0 {
		base = base[:i]
	}
	t, err := time.ParseInLocation("20060102T15", base, time.UTC)
	if err != nil {
		return time.Time{}, ErrBadRollupName
	}
	return t, nil
}

// Prune deletes rollup files whose interval start is older than the
// retention window. It never deletes the file currently being written.
func (w *Writer) Prune() (int, error) {
	cutoff := w.now().UTC().AddDate(0, 0, -w.retentionDays)
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return 0, fmt.Errorf("read usage dir: %w", err)
	}
	current := w.CurrentFile()
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(w.dir, e.Name())
		if current != "" && path == current {
			continue
		}
		t, err := rollupTime(e.Name())
		if err != nil {
			continue // not ours; never delete what we did not write
		}
		if t.Before(cutoff) {
			if err := os.Remove(path); err != nil {
				return removed, fmt.Errorf("prune %s: %w", e.Name(), err)
			}
			removed++
		}
	}
	return removed, nil
}

// ReadSince sums every rollup file whose interval start is at or after
// since. It is what /admin/usage?window= uses.
func ReadSince(dir string, since time.Time) (Rollup, error) {
	var total Rollup
	total.Decisions = map[string]DecisionCounts{}
	rows := map[Key]Counts{}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// Rollups were never enabled (or have not started); an empty
			// window is the honest answer, not a filesystem error.
			return total, nil
		}
		return Rollup{}, fmt.Errorf("read usage dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, err := rollupTime(e.Name()); err != nil {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		t, _ := rollupTime(name)
		if t.Before(since.Truncate(time.Hour)) {
			continue
		}
		if err := readRollupFile(filepath.Join(dir, name), rows, &total); err != nil {
			return Rollup{}, err
		}
	}
	for k, c := range rows {
		total.Rows = append(total.Rows, RollupRow{
			Client: k.Client, Provider: k.Provider, APIKey: k.APIKey, Model: k.Model,
			Requests: c.Requests, InputTokens: c.InputTokens, OutputTokens: c.OutputTokens, Cost: c.Cost,
		})
	}
	sort.Slice(total.Rows, func(i, j int) bool {
		a, b := total.Rows[i], total.Rows[j]
		if a.Client != b.Client {
			return a.Client < b.Client
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Model < b.Model
	})
	return total, nil
}

// readRollupFile folds one file into the running totals.
func readRollupFile(path string, rows map[Key]Counts, total *Rollup) error {
	f, err := os.Open(path) //nolint:gosec // path comes from ReadDir over the operator-configured usage directory
	if err != nil {
		return fmt.Errorf("open rollup: %w", err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Rollup
		if err := json.Unmarshal(line, &r); err != nil {
			// A malformed line (a torn write after SIGKILL, or local
			// corruption) is skipped rather than failing the whole window:
			// one bad record must not hide every good one.
			continue
		}
		for _, row := range r.Rows {
			k := Key{Client: row.Client, Provider: row.Provider, APIKey: row.APIKey, Model: row.Model}
			c := rows[k]
			c.Requests += row.Requests
			c.InputTokens += row.InputTokens
			c.OutputTokens += row.OutputTokens
			c.Cost += row.Cost
			rows[k] = c
		}
		total.UsageMissing += r.UsageMissing
		for model, d := range r.Decisions {
			acc, ok := total.Decisions[model]
			if !ok {
				acc = newDecisionCounts()
			}
			acc.Total += d.Total
			for q, m := range d.Choices {
				if acc.Choices[q] == nil {
					acc.Choices[q] = map[string]int64{}
				}
				for choice, n := range m {
					acc.Choices[q][choice] += n
				}
			}
			for q, m := range d.Confidence {
				if acc.Confidence[q] == nil {
					acc.Confidence[q] = map[string]int64{}
				}
				for bucket, n := range m {
					acc.Confidence[q][bucket] += n
				}
			}
			total.Decisions[model] = acc
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read rollup %s: %w", filepath.Base(path), err)
	}
	return nil
}
