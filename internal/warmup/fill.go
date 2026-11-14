package warmup

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Dezoxy fork: the fill primitive of Readiness step 2 (warm on add). It completes one
// media file's head and tail windows from a torrent the engine already has, in chunks,
// and writes only verified bytes. Its only caller is the warmer
// (internal/warmer): warm on add, off by default, and catch-up (Readiness step 3),
// which asks for room first (RoomOnly) and is fed by the daemon's passes
// (warm_catchup.go).
//
// Why not WriteChunk or fillIndexTail: WriteChunk is asynchronous and drops silently,
// and creates the head under EvictAny; fillIndexTail allocates a whole window, holds no
// reader so the torrent can expire while it waits, and needs the whole window verified
// in the engine's RAM cache at once.

// TriggerWarmAdd is the trigger of an eviction made to make room for warm on add.
const TriggerWarmAdd = "warm_add"

const (
	// fillChunk is the most one fetch holds: neither window is ever allocated whole.
	fillChunk = 4 << 20
)

// How long one chunk and one file may take. Vars so tests don't wait them out.
var (
	fillChunkTimeout = 60 * time.Second
	fillFileTimeout  = 10 * time.Minute
)

// PiecesVerified, if set, reports how many of the pieces under [off, off+n) of a file
// have passed their checksum, and how many there are. The fill compares it before and
// after a chunk: a piece that was not complete and is now came from the swarm, because
// the engine's piece storage is RAM only. Wired at startup beside RangeVerified; nil
// leaves DeliveredPieces at zero.
var PiecesVerified func(hash string, fileID int, off, n int64) (verified, total int)

var (
	// ErrNoRoom means the quota has no room for the windows and the policy forbids
	// evicting enough. Nothing was fetched or evicted for it.
	ErrNoRoom = errors.New("warmup fill: no room in the cache")
	// ErrStopped means the Stop func asked the fill to end. What it wrote stays.
	ErrStopped = errors.New("warmup fill: stopped")
	// ErrEvicted means a window this fill wrote is gone or was refused: quota
	// eviction, RemoveHash, the reaper dropping a small head, or a write the cache
	// declined. It never re-creates the file, and it is not proof of a quota eviction.
	ErrEvicted = errors.New("warmup fill: evicted while filling")
	// ErrCacheDir means the cache directory is gone or not writable, found before any
	// byte was fetched: a fill would download windows it cannot store.
	ErrCacheDir = errors.New("warmup fill: cache directory missing or not writable")
	// ErrTimeout means a chunk or the file did not verify in time. It wraps the last
	// error of the reader that drove the download, if there was one.
	ErrTimeout = errors.New("warmup fill: timed out")
	// ErrNotWired means RangeVerified or TailFetch is nil: the fill would have to write
	// bytes it cannot verify, or cannot fetch any.
	ErrNotWired = errors.New("warmup fill: verification or fetch not wired")
)

// FillRequest names the file to fill and how the fill may make room.
type FillRequest struct {
	Key      FileKey
	FileSize int64
	Policy   EvictPolicy // EvictNeverPlayed or EvictNone; EvictAny (the zero value) is refused
	Trigger  string      // trigger of the evictions made; empty means TriggerWarmAdd
	// Protect, if set, names files the room check must not evict, such as files in
	// flight and the other files of the add being warmed. It runs under the cache's
	// quota lock and tailMu, like IsPlayed: memory only, never blocking, never calling
	// back into the cache (a cache writer would deadlock).
	Protect func(FileKey) bool
	// Stop, if set, is polled before each chunk and while one waits for its pieces:
	// true ends the fill with ErrStopped. A reader already blocked in a fetch (up to
	// 8 s) finishes first, so the latency is up to one fetch.
	Stop func() bool
	// RoomOnly returns after the already-warm check and the room check, before any
	// fetch or request: nil when there is room, AlreadyWarm in the result when both
	// windows are complete, ErrNoRoom when there is none. It is catch-up's pre-flight,
	// so a torrent is woken only for a file that can be filled. With EvictNone it
	// evicts nothing; with another policy the room check evicts as it does for a fill.
	RoomOnly bool
}

// FillResult is what one call did, also when it returned an error.
type FillResult struct {
	HeadBytes int64 // bytes this call wrote to the head
	TailBytes int64 // bytes this call wrote to the tail
	// DeliveredPieces counts pieces that were not complete before their chunk was
	// requested and were after: the swarm delivered them just now. It is a lower bound:
	// pieces the cache prefetches past a chunk are not counted, and a chunk that timed
	// out counts nothing. Pieces the engine
	// already held do not count.
	DeliveredPieces int
	AlreadyWarm     bool // both windows were complete; nothing was fetched
	Evicted         int  // files evicted to make room
}

// FillWindows completes the head window (up to min(FileSize, req.FileSize) from offset
// 0) and the tail window of one file. It returns AlreadyWarm without fetching when both
// are complete. Otherwise it makes room first, for every missing byte of both windows
// and before any fetch (tail writes never enforce the quota themselves), then works in
// chunks: it asks for the pieces, reads through TailFetch to drive the download and keep
// the torrent alive, waits for RangeVerified, reads again requiring the whole chunk,
// checks RangeVerified once more and only then writes. It never uses WriteChunk's
// EvictAny and never fires OnWarmupStateChange. It adds no peer discovery: it reads a
// torrent the engine already has (hard rule 3).
//
// The room check is advisory, as in processWrite: a writer that lands meanwhile can
// still push the cache over the quota until the next head creation enforces it.
func (d *DiskWarmupCache) FillWindows(req FillRequest) (FillResult, error) {
	var res FillResult
	verify, fetch, request, pieces := RangeVerified, TailFetch, RequestPieces, PiecesVerified
	if d == nil || verify == nil || fetch == nil {
		return res, ErrNotWired
	}
	if req.Key.Hash == "" {
		return res, errors.New("warmup fill: empty hash")
	}
	if req.Policy == EvictAny {
		return res, errors.New("warmup fill: EvictAny is not a fill policy: it evicts played files")
	}
	if req.FileSize <= 0 {
		return res, errors.New("warmup fill: file size must be positive")
	}

	hash, id := req.Key.Hash, req.Key.FileIdx
	hpath, tpath := d.filePath(hash, id), d.tailPath(hash, id)
	headTarget := min(FileSize, req.FileSize)
	tailStart, tailLen := d.tailWindow(tpath, req.FileSize)

	headHave := d.headSize(hpath)
	if headHave >= headTarget && d.TailReady(hash, id) {
		res.AlreadyWarm = true
		return res, nil
	}

	if err := unix.Access(d.dir, unix.W_OK); err != nil {
		return res, fmt.Errorf("%w: %v", ErrCacheDir, err)
	}

	missing := max(0, headTarget-headHave)
	for _, g := range d.tailGaps(tpath, tailLen) {
		missing += g.end - g.start
	}
	trigger := req.Trigger
	if trigger == "" {
		trigger = TriggerWarmAdd
	}
	d.mu.Lock()
	evicted, room := d.enforceQuotaLocked(missing, evictOpts{
		policy: req.Policy, exclude: req.Key, protect: req.Protect, trigger: trigger})
	d.mu.Unlock()
	reportEvictions(evicted) // after the locks: it may write to a database
	res.Evicted = len(evicted)
	if !room {
		return res, ErrNoRoom
	}
	if req.RoomOnly {
		return res, nil
	}

	fileDeadline := time.Now().Add(fillFileTimeout)
	buf := make([]byte, fillChunk)
	stopped := func() bool { return req.Stop != nil && req.Stop() }

	// get leaves the verified bytes of [off, off+n) of the file in buf[:n].
	get := func(off, n int64) error {
		var before int
		if pieces != nil {
			before, _ = pieces(hash, id, off, n)
		}
		deadline := time.Now().Add(fillChunkTimeout)
		if fileDeadline.Before(deadline) {
			deadline = fileDeadline
		}
		counted := pieces == nil
		var lastErr error
		for {
			if stopped() {
				return ErrStopped
			}
			if time.Now().After(deadline) {
				if lastErr != nil {
					return fmt.Errorf("%w: %v", ErrTimeout, lastErr)
				}
				return ErrTimeout
			}
			if !verify(hash, id, off, n) {
				// Ask for whole pieces only while some are missing: a request on a
				// piece the engine holds raises a priority nothing clears.
				if request != nil {
					request(hash, id, off, n)
				}
				// A reader drives the download and keeps the torrent from expiring.
				_, lastErr = fetch(hash, id, off, buf[:n])
				if !verify(hash, id, off, n) {
					// Closing that reader clears the priorities the cache set under it,
					// the request included: ask again.
					if request != nil {
						request(hash, id, off, n)
					}
					time.Sleep(verifyPollInterval)
					continue
				}
			}
			if !counted {
				if after, _ := pieces(hash, id, off, n); after > before {
					res.DeliveredPieces += after - before
				}
				counted = true
			}
			// Verified before and after the read: the bytes come from hashed pieces.
			if got, err := fetch(hash, id, off, buf[:n]); err == nil && int64(got) == n && verify(hash, id, off, n) {
				return nil
			}
			time.Sleep(verifyPollInterval)
		}
	}

	// Head: from what is on disk upward. The size is re-read each loop, so a scan fill
	// that ran meanwhile is skipped over, and a size below what this fill wrote means
	// the file was evicted: it is never re-created.
	var headWrote int64
	for {
		if stopped() {
			return res, ErrStopped
		}
		size := d.headSize(hpath)
		if size < headWrote {
			return res, ErrEvicted
		}
		if size >= headTarget {
			break
		}
		n := min(fillChunk, headTarget-size)
		if err := get(size, n); err != nil {
			return res, err
		}
		d.writeHeadLocked(hpath, size, buf[:n])
		if d.headSize(hpath) < size+n { // evicted or refused between the check and the write
			return res, ErrEvicted
		}
		headWrote = size + n
		res.HeadBytes += n
	}

	// Tail: over the spans no write has covered yet, with the same rule: coverage below
	// what this fill wrote means the file was evicted. Before its first write the tail
	// has no floor, so the head stands in: removal takes a unit's head and tail
	// together, and a head that went while the tail waited means the unit went. Stat,
	// not headSize: a stale size entry can outlive the file.
	headGone := func() bool {
		fi, err := os.Stat(hpath)
		return err != nil || fi.Size() < headTarget
	}
	var tailFloor int64
	for {
		if stopped() {
			return res, ErrStopped
		}
		covered, _ := d.tailCovered(tpath)
		if covered < tailFloor || headGone() {
			return res, ErrEvicted
		}
		gaps := d.tailGaps(tpath, tailLen)
		if len(gaps) == 0 {
			break
		}
		rel, n := gaps[0].start, min(fillChunk, gaps[0].end-gaps[0].start)
		if err := get(tailStart+rel, n); err != nil {
			return res, err
		}
		// The wait can be long: check again before a write that would re-create it.
		if covered, _ := d.tailCovered(tpath); covered < tailFloor || headGone() {
			return res, ErrEvicted
		}
		d.writeTail(hash, id, buf[:n], tailStart+rel, req.FileSize)
		val, ok := d.tailCoverage.Load(tpath)
		if !ok || !val.(*tailRange).covers(rel, rel+n) || headGone() {
			return res, ErrEvicted
		}
		tailFloor, _ = d.tailCovered(tpath)
		res.TailBytes += n
	}
	return res, nil
}

// tailCovered returns the bytes of the tail window written so far, and whether the file
// is tracked at all.
func (d *DiskWarmupCache) tailCovered(tpath string) (int64, bool) {
	val, ok := d.tailCoverage.Load(tpath)
	if !ok {
		return 0, false
	}
	covered, _ := val.(*tailRange).snapshot()
	return covered, true
}

// gaps returns the spans of [0, window) that no write has covered.
func (tr *tailRange) gaps(window int64) []tailSpan {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var out []tailSpan
	var pos int64
	for _, s := range tr.ranges {
		if s.start >= window {
			break
		}
		if s.start > pos {
			out = append(out, tailSpan{pos, s.start})
		}
		pos = max(pos, s.end)
	}
	if pos < window {
		out = append(out, tailSpan{pos, window})
	}
	return out
}

// tailGaps returns the spans of the tail window, relative to its start, that are not
// written yet.
func (d *DiskWarmupCache) tailGaps(tpath string, window int64) []tailSpan {
	if val, ok := d.tailCoverage.Load(tpath); ok {
		return val.(*tailRange).gaps(window)
	}
	return []tailSpan{{0, window}}
}
