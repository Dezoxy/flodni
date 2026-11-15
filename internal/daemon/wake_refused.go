package daemon

import (
	"fmt"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"flodni/internal/gostorm/native"
)

// errnoWakeRefused is what a handle's read returns when the engine refused to wake
// its torrent (native.ErrNotStored), and also when the engine was not up yet
// (native.ErrEngineNotReady; that one is not latched). It is private, like errnoBudgetRefused:
// MkvHandle.Read maps it to EIO before ttffReadFailed and the short-read repair,
// because a torrent the engine does not store is not a swarm that failed to
// deliver, and must never become a dead-swarm verdict.
const errnoWakeRefused = syscall.ENOKEY

// wakeRefusedCount counts refused wakes since start, for flodni_wake_refused_total:
// one per handle at a read (the handle latches the refusal), one per Open that
// found the torrent not stored. 0 from the start.
var wakeRefusedCount atomic.Int64

// noteWakeRefused counts a refused wake, writes the distinct log line and reports it
// to the readability record under its own reason (metadb.NotStoredReason, "not_stored"). It is not a
// failed swarm read: nothing is added to consecutive_failures or to the session's
// failed reads, and no dead-swarm verdict follows.
func noteWakeRefused(path, hash, where string) {
	wakeRefusedCount.Add(1)
	short := hash
	if len(short) > 8 {
		short = short[:8]
	}
	logger.Info(fmt.Sprintf("[VFS] wake refused at %s for %s (%s): the engine does not store this torrent; "+
		"re-add it (not a dead swarm)", where, filepath.Base(path), short), "where", where, "file", filepath.Base(path), "short", short)
	if native.ReachabilityOutcome != nil && hash != "" {
		native.ReachabilityOutcome(native.Outcome{
			Hash: hash, Kind: native.OutcomeNotStored, Source: failSourceMount,
		})
	}
}

// engineNotReadyLogEvery bounds the "engine not ready" line: a wake before the engine
// is up can come from every read and every open at once.
const engineNotReadyLogEvery = 10 * time.Second

var engineNotReadyLast atomic.Int64 // unix nanoseconds of the last line

// noteEngineNotReady logs, rate-limited, that a wake came before the engine was up. It
// is transient and says nothing about the torrent, so it writes no readability or
// metafail record, counts no refusal and latches nothing. It reports whether it logged.
func noteEngineNotReady(path, where string) bool {
	now := time.Now().UnixNano()
	last := engineNotReadyLast.Load()
	if now-last < int64(engineNotReadyLogEvery) || !engineNotReadyLast.CompareAndSwap(last, now) {
		return false
	}
	logger.Info(fmt.Sprintf("[VFS] wake at %s for %s: engine not ready yet; not a stored-record verdict, the torrent is not marked",
		where, filepath.Base(path)), "where", where, "file", filepath.Base(path))
	return true
}
