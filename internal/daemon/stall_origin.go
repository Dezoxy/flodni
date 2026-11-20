package daemon

import (
	"time"

	"flodni/internal/gostorm/torr"
)

// The engine side of the stall origin. It lives apart from confirmed_counters.go, which the
// keep-alive hold predicate shares and which must not import the engine.

// stallRefetched asks the engine whether a piece covering [off, off+n) of file fileIdx of the
// torrent completed again at or after since. A var so tests can answer without a torrent.
var stallRefetched = func(hash string, fileIdx int, off int64, n int, since time.Time) bool {
	return torr.PeekTorrent(hash).RefetchedRangeSince(fileIdx, off, int64(n), since)
}

// stallOriginOf classifies a stall. The engine is asked only when the session gives a hash
// and a file index and the read returned bytes; a stall is rare, so the lookup (one snapshot of
// the torrent's cache under its reader read lock) is not on the path of a read that is fast.
func stallOriginOf(path, hash string, fileIdx int, off int64, n int, d time.Duration) int {
	if hash != "" && fileIdx > 0 && n > 0 && stallRefetched(hash, fileIdx, off, n, ttffNow().Add(-d)) {
		return stallOriginEvicted
	}
	ps := pumpStateFor(path)
	if ps == nil {
		return stallOriginNoPump
	}
	if ps.pumpOff.Load() <= off {
		return stallOriginPumpBehind
	}
	return stallOriginSwarm
}
