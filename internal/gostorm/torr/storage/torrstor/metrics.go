package torrstor

import (
	"sync"
	"sync/atomic"
)

// Baseline metrics for the readahead cache. Both only observe: nothing here evicts, frees or
// reorders anything.

// refetchedBytes counts bytes written into a piece that had completed before and was evicted
// since: bytes the swarm delivered twice. The per-piece mark (Piece.everComplete) is one bool per
// piece, freed with the cache when its torrent closes, so the memory is bounded by the piece count.
var refetchedBytes atomic.Int64

// RefetchedBytes is the bytes fetched again for a piece that had completed and been evicted,
// since the process started. For /metrics/prometheus.
func RefetchedBytes() int64 { return refetchedBytes.Load() }

// openCaches holds every initialised, not yet closed cache, so the gauge can sample them without
// going through the engine, its client lock or the server's mutex.
var openCaches sync.Map // *Cache -> struct{}

// StrandedPartialBytes is the bytes held by partially written pieces that lie outside every
// reader's protected window, summed over the open caches. For /metrics/prometheus.
func StrandedPartialBytes() int64 {
	var total int64
	openCaches.Range(func(k, _ any) bool {
		total += k.(*Cache).strandedPartialBytes()
		return true
	})
	return total
}

// strandedPartialBytes sums the Size of pieces that are partly written (Size > 0, not complete)
// and in no active reader's window. Such a piece can never be evicted (pieceEvictable), so after a
// seek its bytes stay in the cache until the same piece is fetched to the end.
//
// Locks: this cache's muReaders for reading, held for the whole sample (the same read lock the
// read path takes in refreshPriorities and getRemPieces; it waits only behind Close, CloseReader
// or a reader attach), and each reader's own mu, briefly, inside getPiecesRange. It takes neither
// the engine's client lock nor muRemove, muPriority or any piece lock: sizes and completion are
// atomic loads, so a piece being written is read at whichever size it has. It does not call
// checkReader, which would park idle readers; the window is the one the evictor would use now.
func (c *Cache) strandedPartialBytes() int64 {
	c.muReaders.RLock()
	defer c.muReaders.RUnlock()
	if c.isClosed.Load() || c.pieces == nil {
		return 0
	}
	ranges := make([]Range, 0, len(c.readers))
	for r := range c.readers {
		r.mu.Lock()
		use := r.isUse
		r.mu.Unlock()
		if use {
			ranges = append(ranges, r.getPiecesRange())
		}
	}
	ranges = mergeRange(ranges)
	var total int64
	for id, p := range c.pieces {
		sz := atomic.LoadInt64(&p.Size)
		if sz > 0 && !p.Complete.Load() && !inRanges(ranges, id) {
			total += sz
		}
	}
	return total
}
