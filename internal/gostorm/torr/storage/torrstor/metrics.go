package torrstor

import (
	"math"
	"sync"
	"sync/atomic"
)

// Baseline metrics for the readahead cache. They only observe: nothing here evicts, frees or
// reorders anything.

// refetchedBytes counts bytes written into a piece that had completed before and was evicted
// since: bytes the swarm delivered twice. The per-piece mark (Piece.everComplete) is one bool per
// piece, freed with the cache when its torrent closes, so the memory is bounded by the piece count.
var refetchedBytes atomic.Int64

// RefetchedBytes is the bytes fetched again for a piece that had completed and been evicted,
// since the process started. For /metrics/prometheus.
func RefetchedBytes() int64 { return refetchedBytes.Load() }

// Who a refetch is charged to (indexes of refetchedByReader). The writer of a piece is a peer
// connection, not a reader, so the label is the kind of reader the cache had at the moment of the
// write: an open-ended one if any is open, else a bounded one, else none.
const (
	refetchOpenEnded = iota
	refetchBounded
	refetchNone
	numRefetchKinds
)

// RefetchReaderNames are the reader label values of RefetchedBytesByReader, in index order.
var RefetchReaderNames = [numRefetchKinds]string{"open_ended", "bounded", "none"}

var refetchedByReader [numRefetchKinds]atomic.Int64

// RefetchedBytesByReader splits RefetchedBytes by the kind of reader the cache had open when
// the bytes were written, indexed like RefetchReaderNames.
func RefetchedBytesByReader() (out [numRefetchKinds]int64) {
	for i := range out {
		out[i] = refetchedByReader[i].Load()
	}
	return out
}

// countRefetch charges n refetched bytes. It reads two atomics on the cache and takes no lock,
// so the write path (which holds the piece lock) keeps its lock order.
func (c *Cache) countRefetch(n int64) {
	refetchedBytes.Add(n)
	switch {
	case c.nOpenEnded.Load() > 0:
		refetchedByReader[refetchOpenEnded].Add(n)
	case c.nBounded.Load() > 0:
		refetchedByReader[refetchBounded].Add(n)
	default:
		refetchedByReader[refetchNone].Add(n)
	}
}

// Where an evicted piece lay against the readers (indexes of evictions).
const (
	zoneBehind      = iota // wholly before every open reader's current offset
	zoneAheadUnread        // at or after an open-ended reader's current offset
	zoneNoReader           // no reader open, or outside every reader
	numEvictionZones
)

// EvictionZoneNames are the zone label values of EvictionCounts, in index order.
var EvictionZoneNames = [numEvictionZones]string{"behind", "ahead_unread", "no_reader"}

// Readers on the cache at eviction time: 0, 1, 2 or more.
const numReaderBuckets = 3

// EvictionReaderBuckets are the readers label values of EvictionCounts, in index order.
var EvictionReaderBuckets = [numReaderBuckets]string{"0", "1", "2+"}

var evictions [numEvictionZones][numReaderBuckets]atomic.Int64

// EvictionCounts is the number of completed pieces evicted from the in-memory cache since the
// process started, by zone and by readers on the cache at the time. For /metrics/prometheus.
func EvictionCounts() (out [numEvictionZones][numReaderBuckets]int64) {
	for z := range out {
		for b := range out[z] {
			out[z][b] = evictions[z][b].Load()
		}
	}
	return out
}

func readerBucket(n int) int { return min(max(n, 0), numReaderBuckets-1) }

// evictView is what classifying an eviction needs of the readers: how many are open, the lowest
// piece under any reader's current offset and the lowest under an open-ended one's (MaxInt when
// there is none). getRemPieces builds it under the reader read lock that scan already holds.
type evictView struct {
	readers  int
	minPiece int
	minOpen  int
}

func newEvictView() evictView { return evictView{minPiece: math.MaxInt, minOpen: math.MaxInt} }

func (v *evictView) add(piece int, openEnded bool) {
	v.readers++
	v.minPiece = min(v.minPiece, piece)
	if openEnded {
		v.minOpen = min(v.minOpen, piece)
	}
}

// zone classifies a piece. "Unread" means at or after the reader's current offset (its piece
// included), not at or after anything the reader has read. Wholly before means the piece lies
// before the piece under the offset, so the piece holding the offset itself is never behind.
func (v evictView) zone(id int) int {
	switch {
	case v.readers == 0:
		return zoneNoReader
	case id >= v.minOpen:
		return zoneAheadUnread
	case id < v.minPiece:
		return zoneBehind
	}
	return zoneNoReader
}

// Eviction order classes (class): lower goes first.
const (
	classBehind    = iota // behind every reader, or no reader near it
	classAheadSome        // ahead of some reader, but of no open-ended one
	classUnread           // at or after an open-ended reader's current offset
)

// class is the eviction order of a candidate piece, which is already outside every window. The
// unread read-ahead of an open-ended reader goes last: it is what that stream reads next.
func (v evictView) class(id int) int {
	switch {
	case v.readers == 0:
		return classBehind
	case id >= v.minOpen:
		return classUnread
	case id >= v.minPiece:
		return classAheadSome
	}
	return classBehind
}

func countEviction(v evictView, id int) {
	evictions[v.zone(id)][readerBucket(v.readers)].Add(1)
}

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
// and in no active reader's window. Such a piece is never evicted (pieceEvictable); after a seek
// its bytes stay until dropStrandedPartials frees them or the piece is fetched to the end.
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
	ranges := c.windowsLocked()
	var total int64
	for id, p := range c.pieces {
		sz := atomic.LoadInt64(&p.Size)
		if sz > 0 && !p.Complete.Load() && !inRanges(ranges, id) {
			total += sz
		}
	}
	return total
}
