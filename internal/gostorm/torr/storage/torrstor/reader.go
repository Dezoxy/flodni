package torrstor

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anacrolix/torrent"

	"flodni/internal/gostorm/log"
	"flodni/internal/gostorm/settings"
)

type Reader struct {
	torrent.Reader
	offset    int64
	readahead int64
	// rangeEnd is the exclusive end of the bounded HTTP range this reader serves, 0 when
	// open-ended (the pump). Read-ahead never reaches past it (Dezoxy fork).
	rangeEnd int64
	// bounded is set by SetBounded for a reader serving a bounded HTTP range, whether or not
	// BoundedReadahead clamps it. Only the metrics read it (metrics.go).
	bounded atomic.Bool
	// protectEnd is the exclusive file offset where a bounded reader's protected range stops
	// (SetBounded), 0 when unknown. It protects only: SetRangeEnd, which clamps the engine
	// read-ahead, stays tied to BoundedReadahead.
	protectEnd atomic.Int64
	file       *torrent.File

	cache    *Cache
	isClosed bool

	///Preload
	lastAccess int64
	isUse      bool
	// inFlight counts reads and seeks inside the underlying reader; guarded by mu. readerOff
	// must not park a reader while one is in flight: the park seeks the anacrolix reader to the
	// head, which moves the read itself. Backported from TorrServer 9b3ceff0.
	inFlight int
	mu       sync.Mutex

	// Deadline-first scheduling state (deadline.go). Guarded by mu.
	deadlineBytes  int64
	deadlineRate   float64
	deadlineRateAt time.Time
	deadlineSetAt  time.Time
}

func newReader(file *torrent.File, cache *Cache) *Reader {
	r := new(Reader)
	r.file = file
	r.Reader = file.NewReader()
	r.attach(cache)
	return r
}

// attach wires a reader to its cache. The cache and isUse must be set before
// SetReadahead: it returns early without them, and the anacrolix reader kept its default
// 48 MiB read-ahead until the next stats tick (about 1 s), enough with a second reader to
// overrun the cache. The initial value is what that tick would set: the cache read-ahead
// split across the open-ended readers (this one included, it starts open-ended), so it is
// computed after the reader is registered.
func (r *Reader) attach(cache *Cache) {
	// V231: Activate Aggressive mode immediately on reader creation
	// to boost initial buffering before the Plex webhook triggers.
	cache.SetAggressive(true, 0)

	r.cache = cache
	r.isUse = true
	cache.activeReaders.Add(1)

	cache.muReaders.Lock()
	if cache.readers != nil {
		cache.readers[r] = struct{}{}
		cache.nOpenEnded.Add(1)
	}
	cache.muReaders.Unlock()
	r.SetReadahead(perReaderReadahead(cache.Readahead(), cache.windowDivisor()))
}

func (r *Reader) Seek(offset int64, whence int) (n int64, err error) {
	r.mu.Lock()
	if r.isClosed {
		r.mu.Unlock()
		return 0, io.EOF
	}
	switch whence {
	case io.SeekStart:
		r.offset = offset
	case io.SeekCurrent:
		r.offset += offset
	case io.SeekEnd:
		r.offset = r.file.Length() + offset
	}
	r.mu.Unlock()

	r.beginIO()
	defer r.endIO()
	n, err = r.Reader.Seek(offset, whence)

	r.mu.Lock()
	r.offset = n
	r.lastAccess = time.Now().Unix()
	r.mu.Unlock()
	// The old schedule describes pieces the player will no longer reach in that order.
	r.clearDeadlines()
	return
}

func (r *Reader) Read(p []byte) (n int, err error) {
	err = io.EOF
	r.mu.Lock()
	if r.isClosed {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()

	if r.file.Torrent() != nil && r.file.Torrent().Info() != nil {
		r.beginIO()
		defer r.endIO()
		n, err = r.Reader.Read(p)

		r.mu.Lock()
		r.offset += int64(n)
		r.lastAccess = time.Now().Unix()
		r.mu.Unlock()
		r.sampleConsumption(n)
	} else {
		log.TLogln("Torrent closed and readed")
	}
	return
}

// ReadContext behaves like Read but is bounded by ctx: if the underlying torrent read blocks
// waiting on a piece that never arrives (stalled swarm, no seeders for that range), it returns
// ctx.Err() instead of hanging until the whole torrent is closed. Mirrors Read's bookkeeping.
func (r *Reader) ReadContext(ctx context.Context, p []byte) (n int, err error) {
	err = io.EOF
	r.mu.Lock()
	if r.isClosed {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()

	if r.file.Torrent() != nil && r.file.Torrent().Info() != nil {
		r.beginIO()
		defer r.endIO()
		n, err = r.Reader.ReadContext(ctx, p)

		r.mu.Lock()
		r.offset += int64(n)
		r.lastAccess = time.Now().Unix()
		r.mu.Unlock()
		r.sampleConsumption(n)
	} else {
		log.TLogln("Torrent closed and readed")
	}
	return
}

func (r *Reader) SetReadahead(length int64) {
	// V244: Panic Protection
	if r.cache == nil {
		return
	}
	// V255: Removed V244 85% clamp — it reduced prefetch by ~13MB without benefit.
	// Cache eviction is LRU-based and doesn't use readahead boundaries.
	// Unbounded readahead growth is now prevented by maxDefaultReadahead cap in anacrolix fork.
	atomic.StoreInt64(&r.readahead, length) // AdjustRA writes it without mu
	if !r.isUse {
		return
	}
	if !BoundedReadahead {
		r.Reader.SetReadahead(length)
		return
	}
	// Read both values when anacrolix asks, not at install time: AdjustRA may run
	// concurrently with SetRangeEnd, and a fixed value installed by it would drop the clamp.
	r.Reader.SetReadaheadFunc(func(c torrent.ReadaheadContext) int64 {
		ra := atomic.LoadInt64(&r.readahead)
		if end := atomic.LoadInt64(&r.rangeEnd); end > 0 {
			return clampReadahead(ra, end, c.CurrentPos)
		}
		return ra
	})
}

// BoundedReadahead stops a bounded range's read-ahead at the range end. Set at startup
// with pump_on_open off (Dezoxy fork); upstream reads ahead the full window.
var BoundedReadahead bool

// SetBounded marks the reader as serving a bounded range ending at end (exclusive file
// offset, 0 when unknown). The reader then no longer counts in the protected-window divisor and
// protects only its own range, current offset to end (getOffsetRange). It never changes the
// engine read-ahead (SetRangeEnd does, when BoundedReadahead is on). It marks once: a reader
// starts open-ended. Call it right after the reader is created, before it is closed.
func (r *Reader) SetBounded(end int64) {
	c := r.cache
	// Under the write lock, like CloseReader, so a close cannot fall between the flag and the
	// counters and leave them off by one.
	c.muReaders.Lock()
	defer c.muReaders.Unlock()
	if !r.bounded.CompareAndSwap(false, true) {
		return
	}
	r.protectEnd.Store(max(end, 0))
	if _, registered := c.readers[r]; registered {
		c.nOpenEnded.Add(-1)
		c.nBounded.Add(1)
	}
}

// SetRangeEnd bounds the reader to a request ending at end (exclusive, file offset): a
// scanner's 64 KB probe then fetches only the pieces it touches, not the read-ahead the
// stats loop hands every reader. Call before the first read.
func (r *Reader) SetRangeEnd(end int64) {
	if end <= 0 || !BoundedReadahead {
		return
	}
	atomic.StoreInt64(&r.rangeEnd, end)
	r.SetReadahead(atomic.LoadInt64(&r.readahead))
}

// clampReadahead limits a read-ahead to the bytes left before end.
func clampReadahead(readahead, end, pos int64) int64 {
	left := end - pos
	if left < 0 {
		left = 0
	}
	if readahead > left {
		return left
	}
	return readahead
}

func (r *Reader) Offset() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.offset
}

func (r *Reader) Readahead() int64 {
	return atomic.LoadInt64(&r.readahead)
}

func (r *Reader) Close() {
	// file reader close in gotorrent
	// this struct close in cache
	r.mu.Lock()
	if r.isClosed {
		r.mu.Unlock()
		return
	}
	r.isClosed = true
	r.mu.Unlock()

	r.clearDeadlines()
	r.readerOff()

	if len(r.file.Torrent().Files()) > 0 {
		_ = r.Reader.Close() // read-only reader
	}
	r.cache.goBG(func() {
		// Serialised with cleanPieces: both rebuild the shared pieceInRange bitmap.
		r.cache.muRemove.Lock()
		defer r.cache.muRemove.Unlock()
		r.cache.getRemPieces()
	})
}

func (r *Reader) getPiecesRange() Range {
	startOff, endOff := r.getOffsetRange()
	return Range{r.getPieceNum(startOff), r.getPieceNum(endOff), r.file}
}

func (r *Reader) getReaderPiece() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.getPieceNum(r.offset)
}

func (r *Reader) getReaderRAHPiece() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.getPieceNum(r.offset + atomic.LoadInt64(&r.readahead))
}

func (r *Reader) getPieceNum(offset int64) int {
	return int((offset + r.file.Offset()) / r.cache.pieceLength)
}

func (r *Reader) getOffsetRange() (int64, int64) {
	prc := int64(settings.BTsets.ReaderReadAHead)

	r.mu.Lock()
	currentOffset := r.offset
	r.mu.Unlock()

	// V243-Fix: Reader Protected Range Safety Factor (0.85)
	// We MUST leave room (15%) for swappable chunks, otherwise cleanPieces can't remove anything
	// when cache is full, leading to deadlock/OOM.
	safetyFactor := float64(protectedSafetyPct) / 100

	// The share is split across the open-ended readers only: a bounded reader (FetchBlock,
	// FetchAhead, a probe) must not shrink the window of the stream that is playing.
	protected := int64(float64(r.cache.capacity/int64(r.cache.windowDivisor())) * safetyFactor)

	var beginOffset, endOffset int64
	if r.bounded.Load() {
		// A bounded reader protects its own request, from where it is to the end of its range,
		// and never more than one open-ended reader's share. The last byte is end-1: the end is
		// exclusive. Without a known end it protects the piece under its offset.
		beginOffset = currentOffset
		endOffset = currentOffset
		if end := r.protectEnd.Load(); end > 0 {
			endOffset = max(min(end, currentOffset+protected)-1, currentOffset)
		}
	} else {
		beginOffset, endOffset = protectedWindow(currentOffset, protected, prc, atomic.LoadInt64(&r.readahead))
	}

	if beginOffset < 0 {
		beginOffset = 0
	}

	if endOffset > r.file.Length() {
		endOffset = r.file.Length()
	}
	return beginOffset, endOffset
}

func (r *Reader) checkReader() {
	r.dropStaleDeadlines()

	r.mu.Lock()
	accessTime := r.lastAccess
	r.mu.Unlock()

	if time.Now().Unix() > accessTime+60 && len(r.cache.readers) > 1 {
		r.readerOff()
	} else {
		r.readerOn()
	}
}

func (r *Reader) readerOn() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resume()
}

// beginIO claims the reader for one read or seek and resumes it if the idle sweep parked it.
// Registering under mu is what keeps readerOff from parking it until endIO.
func (r *Reader) beginIO() {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Resume before registering the IO: a panic inside resume must not leave inFlight set, which
	// would make readerOff refuse to park this reader for the rest of the session.
	r.resume()
	r.inFlight++
}

func (r *Reader) endIO() {
	r.mu.Lock()
	r.inFlight--
	r.mu.Unlock()
}

// resume must be called with mu held.
func (r *Reader) resume() {
	if !r.isUse {
		if pos, err := r.Reader.Seek(0, io.SeekCurrent); err == nil && pos == 0 {
			_, _ = r.Reader.Seek(r.offset, io.SeekStart) // a non-negative absolute seek cannot fail
		}
		// Mark in use before restoring readahead: SetReadahead only propagates while in use, so
		// restoring it in the other order leaves the underlying reader parked at zero.
		r.isUse = true
		r.SetReadahead(atomic.LoadInt64(&r.readahead))
		r.cache.activeReaders.Add(1)
	}
}

func (r *Reader) readerOff() {
	r.mu.Lock()
	defer r.mu.Unlock()
	// lastAccess is stamped only when a read returns, so a read blocked on the swarm makes a
	// busy reader look idle; seeking the anacrolix reader under it moves that read.
	if r.inFlight > 0 {
		return
	}
	if r.isUse {
		// Park the underlying reader without erasing the desired readahead: SetReadahead(0) would
		// zero r.readahead too, and resume would then restore nothing.
		r.Reader.SetReadahead(0)
		r.isUse = false
		r.cache.activeReaders.Add(-1)
		if r.offset > 0 {
			_, _ = r.Reader.Seek(0, io.SeekStart) // a non-negative absolute seek cannot fail
		}
	}
}
