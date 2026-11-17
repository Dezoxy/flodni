package torrstor

import (
	"sort"

	"github.com/anacrolix/torrent"

	"flodni/internal/gostorm/settings"
)

type Range struct {
	Start, End int
	File       *torrent.File
}

func inRanges(ranges []Range, ind int) bool {
	i := sort.Search(len(ranges), func(i int) bool {
		return ranges[i].End >= ind
	})
	return i < len(ranges) && ranges[i].Start <= ind
}

// fillPieceInRange resets dst and marks the pieces covered by the readers'
// windows. Range.Start/End are absolute piece indices: marking the whole file
// instead would protect every piece and stop eviction entirely.
func fillPieceInRange(dst []bool, ranges []Range, pieceCount int) {
	for i := range dst {
		dst[i] = false
	}
	for _, rng := range ranges {
		start, end := rng.Start, rng.End
		if start < 0 {
			start = 0
		}
		if end >= pieceCount {
			end = pieceCount - 1
		}
		for i := start; i <= end; i++ {
			dst[i] = true
		}
	}
}

// pieceEvictable reports whether a cached piece may be dropped. A partially
// written piece must stay: anacrolix keeps its own chunk bookkeeping, so
// dropping one fails the hash check and bans the peer that supplied it.
func pieceEvictable(size int64, complete, inReaderWindow bool) bool {
	return size > 0 && complete && !inReaderWindow
}

func mergeRange(ranges []Range) []Range {
	if len(ranges) <= 1 {
		return ranges
	}
	// copy ranges
	merged := append([]Range(nil), ranges...)

	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Start < merged[j].Start {
			return true
		}
		if merged[i].Start == merged[j].Start && merged[i].End < merged[j].End {
			return true
		}
		return false
	})

	j := 0
	for i := 1; i < len(merged); i++ {
		if merged[j].End >= merged[i].Start {
			if merged[j].End < merged[i].End {
				merged[j].End = merged[i].End
			}
		} else {
			j++
			merged[j] = merged[i]
		}
	}
	return merged[:j+1]
}

// prefetchBudgetPct is the share of the cache the in-flight prefetch front may pin. At the
// production 128MB/4MB configuration it yields the count of 12 that ran clean, so piece sizes
// that already fit keep their behaviour and only oversized ones are trimmed.
const prefetchBudgetPct = 40

// pieceBudgetCount caps a prefetch count by the bytes it pins rather than the pieces it names.
// An incomplete piece cannot be evicted - anacrolix keeps its own chunk bookkeeping, so dropping
// one fails the hash check and bans the peer that supplied it - and piece length varies 1MB..8MB
// across a library, so a fixed count reserves 8x more cache on one torrent than on another. Only
// ever lowers count: raising it would widen the request front on small-piece torrents and trade
// sequential focus for breadth.
func pieceBudgetCount(count int, capacity, pieceLength int64) int {
	if count <= 1 || capacity <= 0 || pieceLength <= 0 {
		return count
	}
	max := int(capacity * prefetchBudgetPct / 100 / pieceLength)
	if max < 1 {
		max = 1
	}
	if max < count {
		return max
	}
	return count
}

// minPerReaderReadahead keeps a scan burst from starving the reader that is actually playing.
// Matches the floor updateRA already applies to the undivided value.
const minPerReaderReadahead = 8 << 20

// CacheReadahead is the whole-cache read-ahead the stats tick hands AdjustRA: the cache size
// (64 MiB when unset) times the read-ahead percentage (95 when unset), at least 8 MiB.
func CacheReadahead() int64 {
	cacheSize := settings.BTsets.CacheSize
	if cacheSize == 0 {
		cacheSize = 64 << 20 // 64 MB default
	}
	readAheadPct := int64(settings.BTsets.ReaderReadAHead)
	if readAheadPct == 0 {
		readAheadPct = 95 // 95% default
	}
	adj := cacheSize * readAheadPct / 100
	if adj < 8<<20 {
		adj = 8 << 20 // minimum 8 MB
	}
	return adj
}

// protectedSafetyPct is the share of the cache that getOffsetRange lets the readers' windows
// cover; the rest stays swappable so cleanPieces always has something to evict.
const protectedSafetyPct = 85

// perReaderReadahead splits a whole-cache readahead across the active readers, the same divisor
// getOffsetRange uses for the protected window, and applies the same protectedSafetyPct. Without
// the factor the engine fetches about 0.75 of the cache ahead while only 0.6375 of it is
// protected, and what arrives in the gap is evictable on arrival and requested again. Undivided,
// a second reader asks the torrent to prefetch more than the cache promises to keep.
func perReaderReadahead(total int64, readers int) int64 {
	if readers < 1 {
		readers = 1
	}
	ra := total / int64(readers) * protectedSafetyPct / 100
	if ra < minPerReaderReadahead {
		ra = minPerReaderReadahead
	}
	if ra > total {
		ra = total
	}
	return ra
}

// protectedWindow returns a reader's eviction-protected byte range around cur. The forward side
// covers the readahead actually requested: protecting less let the band between the two be
// fetched with deadlines and then evicted as the oldest complete pieces (about 11% re-download
// with 4-8 MiB pieces). The whole window is capped at protected, the share of the cache the
// reader may pin, so protection never exceeds what the cache can hold; what the readahead
// leaves of it goes behind cur. Until the first readahead is set (0) the percentage prc applies.
// Adapted from upstream Tiramisu 42658e1.
func protectedWindow(cur, protected, prc, readahead int64) (begin, end int64) {
	forward := protected * prc / 100
	if readahead > 0 {
		forward = readahead
	}
	if forward > protected {
		forward = protected
	}
	if forward < 0 {
		forward = 0
	}
	return cur - (protected - forward), cur + forward
}
