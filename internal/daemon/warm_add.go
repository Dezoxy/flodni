package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"flodni/internal/flog"
	"flodni/internal/gostorm/native"
	"flodni/internal/gostorm/torr"
	"flodni/internal/library"
	"flodni/internal/promtext"
	"flodni/internal/warmer"
	"flodni/internal/warmup"
)

// Warm on add (Dezoxy fork, Readiness step 2): the wiring of internal/warmer to the
// library, the engine, the cache and the per-file record. The warmer is built at start
// and idle until the library files a title with warm_on_add on.

// addWarmer is the process-wide warmer; nil-safe, so a test or a build that never
// created it reports zeroed series and ignores removals.
var addWarmer *warmer.Warmer

// warmWake is NativeClient.WakeStored, set at start beside the engine bridge.
var warmWake func(ctx context.Context, hash string) error

// warnWarmOnAddNoDir makes the "no warmup_dir" warning appear once.
var warnWarmOnAddNoDir sync.Once

// warmOnAddActive is the switch the warmer reads at each enqueue: warm_on_add, and the
// index cache (warmup_dir) the windows are written to.
func warmOnAddActive() bool {
	c := gc()
	return c != nil && c.WarmOnAdd && c.WarmupDir != ""
}

// onLibraryAdded is library.Config.OnAdded: it only enqueues.
func onLibraryAdded(hash string, files []library.AddedFile) {
	c := gc()
	if c == nil || !c.WarmOnAdd {
		return
	}
	if c.WarmupDir == "" {
		warnWarmOnAddNoDir.Do(func() {
			logger.Warn("WARNING: warm_on_add is set but warmup_dir is not: the windows have no index cache to live in, so nothing is warmed on add")
		})
		return
	}
	fs := make([]warmer.File, 0, len(files))
	for _, f := range files {
		fs = append(fs, warmer.File{Index: f.FileIndex, Size: f.Size})
	}
	addWarmer.Add(hash, fs)
}

// onHashRemoved is warmup.OnHashRemoved: the warmer stops first, so nothing it still
// holds lands behind the delete of the torrent's records.
func onHashRemoved(w *warmer.Warmer, x *fileStateIndex) func(string) {
	return func(hash string) {
		w.Cancel(hash)
		x.ForgetHash(hash)
	}
}

// addWarmerDeps wires the warmer to the package globals and the file index x.
func addWarmerDeps(x *fileStateIndex, stop <-chan struct{}) warmer.Deps {
	return warmer.Deps{
		Enabled:      warmOnAddActive,
		PlaybackOpen: playbackOpen,
		Live:         func(hash string) bool { return torrentLive(hash) },
		WakeStored: func(ctx context.Context, hash string) error {
			if warmWake == nil {
				return errors.New("no engine client")
			}
			err := warmWake(ctx, hash)
			if errors.Is(err, native.ErrNotStored) {
				// Refused, not busy: the warmer remembers it for the catch-up feeder.
				return fmt.Errorf("%w: %v", warmer.ErrNotStored, err)
			}
			return err
		},
		Policy: x.WarmPolicy,
		Fill: func(req warmup.FillRequest) (warmup.FillResult, error) {
			if err := checkStubSize(req.Key, req.FileSize); err != nil {
				return warmup.FillResult{}, err
			}
			return warmup.DiskWarmup.FillWindows(req) // nil-safe: ErrNotWired
		},
		DeliveredAt:  warmDeliveredAt,
		EvictedSince: x.EvictedSince,
		Record: func(u warmer.Update) {
			noteWarmFailure(u)
			x.RecordWarm(warmUpdate{key: u.Key, readability: u.Readability, cache: u.Cache,
				deliveredAt: u.DeliveredAt, attemptAt: u.AttemptAt, clearQueued: u.ClearQueued})
		},
		Queued: func(keys []warmup.FileKey) {
			now := x.now()
			for _, k := range keys {
				x.RecordWarm(warmUpdate{key: k, queuedAt: now})
			}
		},
		SwarmRead: func(hash string, at time.Time) {
			db := stateDB
			if db == nil {
				return
			}
			if err := db.RecordSwarmRead(hash, at); err != nil {
				logger.Warn(fmt.Sprintf("WARNING: warm on add: cannot stamp the swarm read of %s: %v", hash, err), "hash", hash, "err", err)
			}
		},
		Classify: classifyFailure,
		RemoveCache: func(hash string) {
			if d := warmup.DiskWarmup; d != nil {
				d.RemoveHash(hash) // runs OnHashRemoved: cancel and forget
				return
			}
			x.ForgetHash(hash)
		},
		Log:  func(line string) { flog.Printf(logger, "%s", line) },
		Stop: stop,
	}
}

// engineFileLength is the length of file id in the resident torrent; false when the
// torrent is closed or has no such file. A var so a test can stand in the engine.
var engineFileLength = func(hash string, id int) (int64, bool) {
	return torr.PeekTorrent(hash).FileLength(id) // nil-safe
}

// checkStubSize refuses a fill whose key names, in the engine, a file of another
// length than the stub's: its windows would hold another file's bytes. A closed
// torrent cannot be checked; the warmer wakes it before the real fill, so the check
// runs again where it can.
func checkStubSize(k warmup.FileKey, size int64) error {
	n, ok := engineFileLength(k.Hash, k.FileIdx)
	if !ok || n == size {
		return nil
	}
	return fmt.Errorf("%w: file %d of %s is %d bytes, the stub says %d",
		warmer.ErrIndexMismatch, k.FileIdx, k.Hash, n, size)
}

// addWarmFillSamples is flodni_warm_fills_total{trigger="add"} and {trigger="catchup"}:
// every outcome of each, 0 until it happens. Emitted in the same family as the mount
// series.
func addWarmFillSamples(st warmer.Stats) []promtext.Sample {
	out := make([]promtext.Sample, 0, len(warmer.Triggers)*len(warmer.Outcomes))
	for _, t := range warmer.Triggers {
		for _, o := range warmer.Outcomes {
			out = append(out, promSample(st.ByTrigger[t].Fills[o], lbl("trigger", t), lbl("outcome", o)))
		}
	}
	return out
}

// collectAddWarmProm writes the warmer's other families.
func collectAddWarmProm(w *promtext.Writer, st warmer.Stats) {
	var bytes []promtext.Sample
	for _, t := range warmer.Triggers {
		bytes = append(bytes,
			promSample(st.ByTrigger[t].HeadBytes, lbl("trigger", t), lbl("window", "head")),
			promSample(st.ByTrigger[t].TailBytes, lbl("trigger", t), lbl("window", "tail")))
	}
	w.Counter("flodni_warm_fill_bytes_total",
		"Bytes warm on add and catch-up wrote to the head and tail windows, by trigger, also by fills that did not finish.",
		bytes...)
	cf := make([]promtext.Sample, 0, len(warmer.CacheFailureReasons))
	for _, r := range warmer.CacheFailureReasons {
		cf = append(cf, promSample(st.CacheFailures[r], lbl("reason", r)))
	}
	w.Counter("flodni_warm_cache_failures_total",
		"Warm on add and catch-up attempts that lost a window or could not write it, by reason: lost (windows gone with no quota eviction mark) and write_failed. Retried at 10 minutes, 1 hour and 6 hours.",
		cf...)
	w.Gauge("flodni_warm_queue_files", "Files waiting for warm on add or catch-up.", promSample(int64(st.QueueFiles)))
	paused := int64(0)
	if st.Paused {
		paused = 1
	}
	w.Gauge("flodni_warm_paused", "1 while warm on add and catch-up are paused for a playback.", promSample(paused))
}
