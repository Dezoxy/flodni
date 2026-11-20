package torrstor

import "time"

// What keeps a playing stream's unread read-ahead in the cache (Dezoxy fork): the protected
// window is split across open-ended readers only (Reader.getOffsetRange), eviction prefers
// pieces no open-ended reader is about to read (evictView.class), and the window of an
// open-ended reader that just closed outlives it for a short grace (below).

// reconnectGrace is how long the protected range of a closed open-ended reader still counts as
// protected. The pump reconnects through ServeContent's Seek(End)/Seek(0) before it reads, so
// without it the window is unprotected between the old reader's close and the new one's first
// read, and its pieces, the oldest in the cache, go first.
const reconnectGrace = 2 * time.Second

// graceMax bounds graceRanges. A close past it drops the oldest entry.
const graceMax = 8

// graceRange is the protected range of an open-ended reader that closed, and when it stops
// counting.
type graceRange struct {
	rng   Range
	pos   int // the piece under the reader's offset when it closed
	until time.Time
}

// now is the cache's clock; a seam for tests (clock), time.Now otherwise.
func (c *Cache) now() time.Time {
	if c.clock != nil {
		return c.clock()
	}
	return time.Now()
}

// windowDivisor is what the cache is split by for each reader's protected window and
// read-ahead: the open-ended readers, at least one. Bounded readers protect their own range
// and take no share.
func (c *Cache) windowDivisor() int {
	return max(1, int(c.nOpenEnded.Load()))
}

// rememberClosed records the window of an open-ended reader that is closing. The caller holds
// muReaders for writing and has not closed the reader yet (closing parks it, which empties the
// window). Expired entries are dropped here; readers of the list skip them by time, so nothing
// else needs a timer, a goroutine or a lock of its own: the list changes only under the write
// lock and is read under the read lock.
func (c *Cache) rememberClosed(r *Reader) {
	if r.bounded.Load() {
		return
	}
	r.mu.Lock()
	use := r.isUse
	r.mu.Unlock()
	if !use {
		return // parked: it holds no window
	}
	now := c.now()
	keep := make([]graceRange, 0, graceMax)
	for _, g := range c.grace {
		if now.Before(g.until) {
			keep = append(keep, g)
		}
	}
	if len(keep) >= graceMax {
		keep = keep[len(keep)-graceMax+1:]
	}
	c.grace = append(keep, graceRange{rng: r.getPiecesRange(), pos: r.getReaderPiece(), until: now.Add(reconnectGrace)})
}

// liveGraceLocked returns the grace entries still counting at now. The caller holds muReaders
// (read or write).
func (c *Cache) liveGraceLocked(now time.Time) []graceRange {
	var live []graceRange
	for _, g := range c.grace {
		if now.Before(g.until) {
			live = append(live, g)
		}
	}
	return live
}
