package torrstor

import (
	"sync/atomic"
	"time"
)

// strandedPartialAge is how long an incomplete piece outside every reader window must go
// untouched before it counts as left behind by a seek.
const strandedPartialAge = 20 * time.Second

// strandedSweepEvery spaces the dropStrandedPartials scans.
const strandedSweepEvery = 5 * time.Second

// windowsLocked is the merged protected window of every active reader, built with getPiecesRange
// (so getOffsetRange stays the one definition of the window). The caller holds muReaders. It does
// not call checkReader, which would park idle readers: an idle reader keeps its window here.
func (c *Cache) windowsLocked() []Range {
	ranges := make([]Range, 0, len(c.readers))
	for r := range c.readers {
		r.mu.Lock()
		use := r.isUse
		r.mu.Unlock()
		if use {
			ranges = append(ranges, r.getPiecesRange())
		}
	}
	return mergeRange(ranges)
}

// strandedPartial reports whether a piece is a partial a seek left behind: written, not complete,
// outside every reader window and untouched since cutoff.
func strandedPartial(size int64, complete, inReaderWindow bool, accessed, cutoff int64) bool {
	return size > 0 && !complete && !inReaderWindow && accessed <= cutoff
}

// dropStrandedPartials frees incomplete pieces a seek left behind. Eviction only takes complete
// pieces and nothing requests these any more, so each would hold a whole piece of RAM until the
// torrent closes. The torrent forgets the chunks first (ForgetPartialPiece, which refuses while
// anything could still deliver one), so a later request downloads them again instead of trusting
// a freed buffer. At most every strandedSweepEvery: a piece needs strandedPartialAge untouched
// to qualify, so a full scan on every clean pass would only cost CPU. Adapted from upstream
// Tiramisu e7301dc and c3742fb.
//
// Locks: muReaders for reading while the window is built, released before the first
// ForgetPartialPiece, which takes the engine's client lock. That lock is therefore never taken
// under a cache lock here, and no piece lock is held across it. The buffer is released after
// it, under the piece's own lock, so the order is client lock, then piece lock, never nested.
// Only the cleaner calls this: neither muRemove nor muPriority is held.
func (c *Cache) dropStrandedPartials(now time.Time) {
	if last := c.lastStrandedNS.Load(); last != 0 && now.UnixNano()-last < int64(strandedSweepEvery) {
		return
	}
	c.lastStrandedNS.Store(now.UnixNano())

	c.muReaders.RLock()
	pieces, tor := c.pieces, c.torrent
	if c.isClosed.Load() || pieces == nil {
		c.muReaders.RUnlock()
		return
	}
	forget := c.forgetPartial
	if forget == nil {
		if tor == nil {
			c.muReaders.RUnlock()
			return
		}
		forget = tor.ForgetPartialPiece
	}
	ranges := c.windowsLocked()
	c.muReaders.RUnlock()

	cutoff := now.Unix() - int64(strandedPartialAge/time.Second)
	for id, p := range pieces {
		if !strandedPartial(atomic.LoadInt64(&p.Size), p.Complete.Load(), inRanges(ranges, id),
			atomic.LoadInt64(&p.Accessed), cutoff) {
			continue
		}
		// A chunk landing after this read and before the release leaves the counter changed, and
		// releaseIfUnwritten then keeps the bytes the torrent now believes it has.
		since := p.mPiece.writeCount()
		if forget(id) {
			p.mPiece.releaseIfUnwritten(since)
		}
	}
}
