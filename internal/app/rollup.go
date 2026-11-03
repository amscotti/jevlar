package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/amscotti/jevlar/internal/usage"
)

// pruneEvery is how often expired rollup files are removed. Daily is
// enough: retention is measured in days.
const pruneEvery = 24 * time.Hour

// startRollups launches the durable usage writer when it is enabled and
// returns a stop function that cancels the loop and waits for the final
// flush. Callers must invoke it before returning, or the last interval is
// lost on a clean shutdown.
//
// The loop owns no request path: it snapshots the tracker on a ticker, so
// accounting can never slow a caller down. A SIGKILL therefore loses at
// most the interval in progress.
func startRollups(parent context.Context, log *slog.Logger, cfg *configUsage, tracker *usage.Tracker) func() {
	if cfg == nil || !cfg.Enabled {
		return func() {}
	}
	ctx, cancel := context.WithCancel(parent)
	w := usage.NewWriter(usage.WriterOptions{
		Dir:           cfg.Dir,
		RetentionDays: cfg.RetentionDays,
		MaxFileMB:     cfg.MaxFileMB,
		Interval:      cfg.Interval,
	})

	write := func() {
		rows, misses := tracker.Snapshot()
		if err := w.Write(rows, misses, tracker.DecisionsSnapshot()); err != nil {
			log.Error("usage rollup write failed", "error", err)
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(w.Interval())
		defer ticker.Stop()
		pruner := time.NewTicker(pruneEvery)
		defer pruner.Stop()
		for {
			select {
			case <-ctx.Done():
				write()
				if err := w.Close(); err != nil {
					log.Error("usage rollup close failed", "error", err)
				}
				return
			case <-ticker.C:
				write()
			case <-pruner.C:
				if n, err := w.Prune(); err != nil {
					log.Error("usage rollup prune failed", "error", err)
				} else if n > 0 {
					log.Info("usage rollups pruned", "files", n)
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// configUsage is the subset of usage configuration the rollup loop needs,
// kept separate so app does not depend on the config package's shape.
type configUsage struct {
	Enabled       bool
	Dir           string
	RetentionDays int
	MaxFileMB     int
	Interval      time.Duration
}
