package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amscotti/jevlar/internal/usage"
)

// lockedBuffer is a goroutine-safe writer for log capture.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestStartRollups_DisabledIsANoOp pins the disabled path.
func TestStartRollups_DisabledIsANoOp(t *testing.T) {
	t.Parallel()
	// A no-op stop is returned for both disabled shapes.
	startRollups(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil, usage.NewTracker())()
	startRollups(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)),
		&configUsage{Enabled: false, Dir: t.TempDir()}, usage.NewTracker())()
}

// TestStartRollups_WritesOnTickAndFlushesOnCancel pins the loop: a tick
// writes an interval, and cancelling the context flushes and closes.
func TestStartRollups_WritesOnTickAndFlushesOnCancel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tr := usage.NewTracker()
	tr.Record(usage.Key{Client: "app", Provider: "p", Model: "m"}, usage.Usage{InputTokens: 42})

	stop := startRollups(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), &configUsage{
		Enabled: true, Dir: dir, RetentionDays: 1, MaxFileMB: 1, Interval: 10 * time.Millisecond,
	}, tr)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(dir)
		if len(entries) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop() // flushes the final interval and waits for it

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no rollup file written: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"input_tokens":42`) {
		t.Fatalf("rollup missing the recorded tokens:\n%s", body)
	}
}

// TestStartRollups_ReportsUnwritableDirectory pins fail-loud logging
// rather than a silent drop.
func TestStartRollups_ReportsUnwritableDirectory(t *testing.T) {
	t.Parallel()
	buf := &lockedBuffer{}
	tr := usage.NewTracker()
	tr.Record(usage.Key{Client: "app"}, usage.Usage{InputTokens: 1})
	stop := startRollups(context.Background(), slog.New(slog.NewTextHandler(buf, nil)), &configUsage{
		Enabled: true, Dir: filepath.Join(t.TempDir(), "missing"), Interval: 5 * time.Millisecond,
	}, tr)
	time.Sleep(20 * time.Millisecond)
	stop()
	if !strings.Contains(strings.ToLower(buf.String()), "rollup") {
		t.Fatalf("no rollup failure logged:\n%s", buf.String())
	}
}
