package daemon

import (
	"fmt"
	"path/filepath"
	"sync/atomic"
	"time"

	"flodni/internal/promtext"
)

// The read path starts a pump again when a read misses the cache, and the end of the file or
// an idle timeout drop a pump out from under a player that is served from the cache or from
// demand fetches. The watchdog is the backstop for any such gap: a confirmed playback with a
// live handle and no pump is given its pump back.
//
// pumpWatchdogInterval is the check period: no existing tick fits (the cleanup sweep runs
// every five minutes, the others belong to other subsystems), and a pass is a walk over the
// playback registry and the open handles, off the read path. pumpWatchdogGrace is two periods:
// the normal gaps (a handle handoff, a release followed by a reopen) close well inside it,
// so a pump is missing for longer than that only when nothing is going to start one.
const (
	pumpWatchdogInterval = 5 * time.Second
	pumpWatchdogGrace    = 10 * time.Second
)

// pumpWatchdogRestarts counts the pumps the watchdog started.
var pumpWatchdogRestarts atomic.Int64

// playingWithoutPump lists, per file path, the handle that would start the pump of every
// confirmed playback that has a live primary handle and no registered pump, short of the end. A confirmed playback
// without a handle (restored at start, or between a stop and a reopen) is not playing.
func playingWithoutPump() map[string]*MkvHandle {
	out := map[string]*MkvHandle{}
	app.playbackRegistry.Range(func(k, v interface{}) bool {
		path, _ := k.(string)
		ps, ok := v.(*PlaybackState)
		if !ok || !ps.GetStatus() {
			return true
		}
		if _, live := app.activePumps.Load(path); live {
			return true
		}
		var best *MkvHandle // the primary: a probe is never made the player's handle
		app.activeHandles.Range(func(key, _ interface{}) bool {
			h, ok := key.(*MkvHandle)
			if !ok || h.path != path || h.released.Load() || !h.isPrimaryHandle.Load() {
				return true
			}
			if best == nil || h.lastRead.Load() > best.lastRead.Load() {
				best = h
			}
			return true
		})
		// A player in the last lead has its pump ended for good (pumpAtEnd): nothing is missing.
		if best != nil && pumpAtEnd(best.followOff(), best.size, pumpLeadLimit(gc().ReadAheadBudget, raCache.ChunkSize(path)), raCache.ChunkSize(path)) {
			best = nil
		}
		if best != nil {
			out[path] = best
		}
		return true
	})
	return out
}

// pumpWatchdog remembers since when each playing file has had no pump.
type pumpWatchdog struct {
	since map[string]time.Time
}

func newPumpWatchdog() *pumpWatchdog { return &pumpWatchdog{since: map[string]time.Time{}} }

// tick is one pass at time now: files that have a pump again are forgotten, and a file with
// none for pumpWatchdogGrace gets one through the normal attach and slot path.
func (w *pumpWatchdog) tick(now time.Time) {
	missing := playingWithoutPump()
	for path := range w.since {
		if _, still := missing[path]; !still {
			delete(w.since, path)
		}
	}
	for path, h := range missing {
		first, seen := w.since[path]
		if !seen {
			w.since[path] = now
			continue
		}
		if now.Sub(first) < pumpWatchdogGrace {
			continue
		}
		w.since[path] = now // a refused slot is tried again after another grace period
		logger.Warn(fmt.Sprintf("[Pump] watchdog: no pump for %s during playback for %ds; restarting", filepath.Base(path), int(now.Sub(first).Seconds())), "file", filepath.Base(path))
		if h.hash == "" || h.released.Load() {
			continue
		}
		h.dropOrphanedPumpRef()
		h.startNativePump(h.hash, h.fileID)
		if _, live := app.activePumps.Load(path); live {
			pumpWatchdogRestarts.Add(1)
		}
	}
}

// startPumpWatchdog runs the watchdog every pumpWatchdogInterval until stop closes.
func startPumpWatchdog(stop <-chan struct{}) {
	w := newPumpWatchdog()
	safeGo(func() {
		t := time.NewTicker(pumpWatchdogInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				w.tick(time.Now())
			case <-stop:
				return
			}
		}
	})
}

func collectPumpWatchdogProm(w *promtext.Writer) {
	w.Gauge("flodni_playing_sessions_without_pump",
		"Confirmed playbacks with an open primary handle and no read-ahead pump right now, the player not in the last lead of the file (a pump that ended there ended for good). Alert when it stays above 0 for a few minutes: the watchdog restarts a missing pump after 10 s, so a lasting value means it cannot.",
		promSample(int64(len(playingWithoutPump()))))
	w.Counter("flodni_pump_watchdog_restarts_total",
		"Pumps the watchdog started for a confirmed playback that had none for 10 s.",
		promSample(pumpWatchdogRestarts.Load()))
}
