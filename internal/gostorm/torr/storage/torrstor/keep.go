package torrstor

import (
	"time"

	"github.com/anacrolix/torrent"
)

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
// read-ahead: the open-ended readers in use, at least one. Bounded readers protect their own
// range (boundedShare) and a parked reader holds no window, so neither takes a share.
func (c *Cache) windowDivisor() int {
	return max(1, int(c.nActiveOpen.Load()))
}

// boundedShare is what one bounded reader may protect: the slack the open-ended windows leave
// (100 - protectedSafetyPct of the cache) split across the bounded readers. Open-ended windows
// plus bounded ranges then never exceed the cache, however many long ranges are open.
func (c *Cache) boundedShare() int64 {
	return c.capacity * (100 - protectedSafetyPct) / 100 / int64(max(1, int(c.nBounded.Load())))
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
	until := now.Add(reconnectGrace)
	c.grace = append(keep, graceRange{rng: r.getPiecesRange(), pos: r.getReaderPiece(), until: until})
	c.graceUntil.Store(max(c.graceUntil.Load(), until.UnixNano()))
}

// endGrace drops the grace entries of a file whose reader has come back to read: the window is
// the new reader's now, or the player seeked and the old one must go. The priorities follow in
// the background, as after a close. A read-lock-free check first: most reads find no grace.
func (c *Cache) endGrace(file *torrent.File) {
	if u := c.graceUntil.Load(); u == 0 || c.now().UnixNano() >= u {
		return
	}
	c.muReaders.Lock()
	if c.readers == nil {
		c.muReaders.Unlock()
		return
	}
	keep := make([]graceRange, 0, len(c.grace))
	latest := int64(0)
	for _, g := range c.grace {
		if g.rng.File == file {
			continue
		}
		keep = append(keep, g)
		latest = max(latest, g.until.UnixNano())
	}
	changed := len(keep) != len(c.grace)
	c.grace = keep
	c.graceUntil.Store(latest)
	c.muReaders.Unlock()
	if changed {
		c.goBG(c.clearPriority)
	}
}

// clearIfGraceOver is the cleaner tick's step: once the last grace has passed it drops the
// priorities it kept, which on a silent swarm nothing else would (no piece completes, so no
// clean pass runs). It takes the locks clearPriority always takes, none of its own.
func (c *Cache) clearIfGraceOver() {
	u := c.graceUntil.Load()
	if u == 0 || c.now().UnixNano() < u {
		return
	}
	if c.graceUntil.CompareAndSwap(u, 0) {
		c.clearPriority()
	}
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
