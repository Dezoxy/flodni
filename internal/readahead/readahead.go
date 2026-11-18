// Package readahead is the sharded chunk cache that sits between the torrent pump and FUSE
// reads. One budget is shared by every stream that is being read.
package readahead

import (
	torrstor "flodni/internal/gostorm/torr/storage/torrstor"
	"fmt"
	"github.com/cespare/xxhash/v2"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// staleWindow is how long a path may go without a chunk being read or written before it
// counts as idle: no stream is reading it, so its chunks are the first to go.
const staleWindow = 120 * time.Second

// nearLogEvery is the least time between two "evicted near the player" log lines for one path.
const nearLogEvery = 30 * time.Second

// Why a chunk was evicted over budget, in the order the cache tries them.
const (
	ReasonIdle   = 0 // its path had no access within staleWindow
	ReasonBehind = 1 // entirely behind its stream's player (already consumed)
	ReasonAhead  = 2 // farthest ahead of the player, on the path holding the most bytes
	numReasons   = 3
)

// ReasonNames labels the Reason* indexes of EvictionsByReason.
var ReasonNames = [numReasons]string{"idle", "behind", "ahead"}

type raBuffer struct {
	path           string
	start, end     int64
	data           []byte
	lastAccess     int64
	seq            int64 // write order, for the eviction of a path whose player is unknown
	responsiveOnly bool  // true if written in non-verified mode (responsive shield active)
}

// Config injects what the cache needs from the daemon. Base and Budget are read
// on every use (config can be reloaded, and is not yet loaded when the cache is
// constructed); Base, Budget, Logf and Go are required, PlayerOff is optional.
type Config struct {
	Base   func() int64                     // chunk size (ReadAheadBase), 0 means 16MB
	Budget func() int64                     // total budget in bytes (ReadAheadBudget)
	Logf   func(format string, args ...any) // logger
	Go     func(fn func())                  // panic-safe background goroutine launcher
	// PlayerOff returns the offset a path's player is reading at, false when no player is
	// known. It is called with no cache lock held. Without it, or when it says false, a path's
	// chunks are evicted oldest-written first, as for a single unaware stream.
	PlayerOff func(path string) (int64, bool)
}

// Cache is a 32-shard chunk cache shared by every stream.
//
// Lock order (outermost first): muEvict, then ONE shard lock. No code holds two shard locks,
// and none takes muEvict while holding a shard lock (Put releases its shard before it
// evicts). muContext is a leaf: never held while taking another lock. cfg.PlayerOff, cfg.Go
// and cfg.Logf are never called under a shard lock.
type Cache struct {
	cfg       Config
	shards    [32]*raShard
	shardMask uint64
	used      int64
	pool      chan []byte // recycled 16MB chunks
	seq       atomic.Int64

	muContext  sync.Mutex
	activePath string // last opened path; only used to notice a change

	// muEvict serializes eviction decisions (Put over budget, the idle sweep) and guards the
	// two fields below.
	muEvict  sync.Mutex
	scratch  map[string]pathStat
	lastNear map[string]time.Time

	// evictions counts chunks Put dropped to stay under budget, by reason; only rises.
	evictions [numReasons]atomic.Int64
	// nearPlayer counts evictions of a chunk inside the player's fair-share window.
	nearPlayer atomic.Int64

	pieceLens sync.Map // path → int64 adaptive chunk size (aligned to piece boundary)
}

// SetPieceLen stores the aligned chunk size for path derived from piece length.
// Called at pump start once pieceLen is known; both pump and raCache share this value.
func (c *Cache) SetPieceLen(path string, pl int64) {
	base := int64(c.cfg.Base())
	if base == 0 {
		base = 16 * 1024 * 1024
	}
	if pl <= 0 {
		c.pieceLens.Delete(path)
		return
	}
	if n := base / pl; n > 0 {
		// pl fits within base: use the largest multiple of pl that's <= base, so chunk
		// boundaries always land on piece boundaries.
		c.pieceLens.Store(path, n*pl)
		return
	}
	// pl > base: chunk boundaries coincide with piece boundaries only at multiples of
	// lcm(base, pl) - with power-of-two pl and base (mandatory in BEP52/v2, merely
	// conventional in BEP3/v1, and the default ReadAheadBase) that's simply every
	// (pl/base)-th boundary. ~25% of the library uses non-power-of-two piece lengths
	// (see recycle() below), for which alignment is rarer still. Either way alignment
	// here is best-effort - reads are byte-range based regardless. Stored explicitly
	// (instead of falling through to ChunkSize's own base fallback) so pieceLens always
	// has an entry once pieceLen is known, and re-reads pick up base config changes at
	// pump start.
	c.pieceLens.Store(path, base)
}

// ClearPieceLen forgets the adaptive chunk size for path (pump teardown).
func (c *Cache) ClearPieceLen(path string) {
	c.pieceLens.Delete(path)
}

// ChunkSize returns the adaptive chunk size for path (falls back to ReadAheadBase).
func (c *Cache) ChunkSize(path string) int64 {
	if v, ok := c.pieceLens.Load(path); ok {
		return v.(int64)
	}
	base := int64(c.cfg.Base())
	if base == 0 {
		base = 16 * 1024 * 1024
	}
	return base
}

type raShard struct {
	mu      sync.RWMutex
	buffers map[string]*raBuffer
	total   int64
}

func New(cfg Config) *Cache {
	// Static 32-buffer default: New runs at package init (var raCache),
	// BEFORE config is loaded - Base/Budget read a nil config here, so nothing config-dependent may be
	// touched. The pool is re-anchored to the budget once config exists via
	// AnchorPoolToConfig() (called from main after loading).
	c := &Cache{
		cfg:       cfg,
		shardMask: 31,
		pool:      make(chan []byte, 32), // Cap at 32 chunks (512MB max pool)
		scratch:   make(map[string]pathStat),
		lastNear:  make(map[string]time.Time),
	}
	for i := range c.shards {
		c.shards[i] = &raShard{
			buffers: make(map[string]*raBuffer),
		}
	}
	return c
}

// AnchorPoolToConfig re-sizes the recycled-buffer pool to the configured budget. Idle buffers
// are only ever needed to absorb eviction/overwrite storms, whose size scales with how much
// live content the budget allows - previously a fixed 32 (512MB max pool) regardless of
// budget: wasteful at 128MB (8 live chunks, 512MB parked), and at 512MB (32 live chunks) the
// 32-cap was 1:1 with live content, leaving no margin for the 32-shard concurrent-pop burst
// (every Put pops one buffer before its eviction recycle lands - with all shards popping
// simultaneously the pool must hold live-count buffers or allocations churn).
// clamp(budget/base*2, 16, 64): 2x live content so the margin holds across the whole range
// (256MB->32, 512MB->64), floor 16 to avoid allocation churn on tiny budgets, ceiling 64 to
// bound parking. The ceiling is nearly free: the pool only ever retains buffers that were
// already allocated, it never pre-allocates.
func (c *Cache) AnchorPoolToConfig() {
	base := c.cfg.Base()
	if base <= 0 {
		base = 16 * 1024 * 1024
	}
	budget := c.cfg.Budget()
	if budget <= 0 {
		budget = 256 * 1024 * 1024
	}
	poolCap := int(budget / base * 2)
	if poolCap < 16 {
		poolCap = 16
	}
	if poolCap > 64 {
		poolCap = 64
	}
	if poolCap == cap(c.pool) {
		return
	}
	c.cfg.Logf("[RaCache] Pool cap anchored to budget: %d buffers (%dMB max, budget %dMB, base %dMB)",
		poolCap, poolCap*int(base)/1024/1024, budget/1024/1024, base/1024/1024)
	c.pool = make(chan []byte, poolCap)
}

func (c *Cache) getShard(path string) *raShard {
	return c.shards[xxhash.Sum64String(path)&c.shardMask]
}

// allocChunk allocates a chunk buffer with capacity rounded up to base so recycle() can pool
// it regardless of the file's adaptive chunk size. Sizes above base (shouldn't happen for
// pump chunks) fall back to an exact, unpoolable allocation.
func allocChunk(size, base int64) []byte {
	if size < base {
		return make([]byte, size, base)
	}
	return make([]byte, size)
}

func (c *Cache) recycle(b []byte) {
	chunkSize := int(16 * 1024 * 1024)
	if c.cfg.Base() > 0 {
		chunkSize = int(c.cfg.Base())
	}
	// Match on cap, not len: adaptive chunks (see SetPieceLen) have len < base for torrents
	// whose piece length doesn't divide base evenly (~25% of the library uses non-power-of-two
	// piece lengths), but Put always allocates them with cap == base so they stay poolable.
	if cap(b) == chunkSize {
		select {
		case c.pool <- b[:chunkSize]:
		default:
			// Pool full, let GC handle it
		}
	}
}

// MaxCachedOffset returns the highest cached byte end for a given path.
func (c *Cache) MaxCachedOffset(p string) int64 {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	prefix := p + ":"
	var maxEnd int64
	for key, b := range s.buffers {
		if strings.HasPrefix(key, prefix) && b.end > maxEnd {
			maxEnd = b.end
		}
	}
	return maxEnd

}

// CoveredRun returns how many bytes starting at off the cache holds without a gap, up to
// limit (exclusive end), 0 when off itself is not cached. Read only: unlike Covered and Get it
// does not refresh lastAccess, so asking never keeps a chunk alive. It holds one shard read
// lock for at most limit/chunk map lookups.
func (c *Cache) CoveredRun(p string, off, limit int64) int64 {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	pos := off
	for pos < limit {
		b, ok := s.buffers[c.chunkKey(p, pos)]
		if !ok || pos < b.start || pos > b.end {
			break
		}
		pos = b.end + 1
	}
	return max(min(pos, limit)-off, 0)
}

// chunkKey returns a compound key using the per-path adaptive chunk size.
func (c *Cache) chunkKey(path string, offset int64) string {
	return fmt.Sprintf("%s:%d", path, offset/c.ChunkSize(path))
}

// SwitchContext is called when a file is opened. It never drops a chunk of another path that
// is still being read: a path counts as alive while any of its chunks was read or written
// within staleWindow. Only when the opened path changes it sweeps the paths that are idle.
func (c *Cache) SwitchContext(newPath string) {
	c.muContext.Lock()
	pathChanged := newPath != c.activePath
	c.activePath = newPath
	c.muContext.Unlock()

	if pathChanged {
		c.cfg.Go(c.sweepIdle)
	}
}

// Covered reports whether the cache fully covers [off, end] without copying any bytes —
// use this for presence/coverage checks on the hot path; call Get separately only once the
// actual data is needed, to avoid an up-to-16MB defensive copy on every check.
func (c *Cache) Covered(p string, off, end int64) bool {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := c.chunkKey(p, off)
	if b, ok := s.buffers[key]; ok && off >= b.start && off <= b.end {
		if end <= b.end {
			atomic.StoreInt64(&b.lastAccess, time.Now().UnixNano())
			return true
		}
		if b2, ok2 := s.buffers[c.chunkKey(p, end)]; ok2 && b2.start == b.end+1 && b2.end >= end {
			atomic.StoreInt64(&b.lastAccess, time.Now().UnixNano())
			atomic.StoreInt64(&b2.lastAccess, time.Now().UnixNano())
			return true
		}
	}
	return false
}

func (c *Cache) Get(p string, off, end int64) []byte {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := c.chunkKey(p, off)
	if b, ok := s.buffers[key]; ok && off >= b.start && off <= b.end {
		atomic.StoreInt64(&b.lastAccess, time.Now().UnixNano())
		if end <= b.end {
			// Defensive copy: pool evicts buffers immediately; sub-slice reference causes use-after-free.
			src := b.data[off-b.start : end-b.start+1]
			out := make([]byte, len(src))
			copy(out, src)
			return out
		}
		// Cross-boundary read: stitch two adjacent chunks to avoid FetchBlock on chunk boundary straddles.
		if b2, ok2 := s.buffers[c.chunkKey(p, end)]; ok2 && b2.start == b.end+1 && b2.end >= end {
			atomic.StoreInt64(&b2.lastAccess, time.Now().UnixNano())
			out := make([]byte, end-off+1)
			n1 := copy(out, b.data[off-b.start:])
			copy(out[n1:], b2.data[:end-b2.start+1])
			return out
		}
	}
	return nil
}

// Exists checks if a chunk is present in cache without allocating.
func (c *Cache) Exists(p string, off int64) bool {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := c.chunkKey(p, off)
	_, found := s.buffers[key]
	return found
}

// CopyTo copies data directly into dest, avoiding an intermediate allocation in the FUSE Read hot path.
func (c *Cache) CopyTo(p string, off, end int64, dest []byte) int {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := c.chunkKey(p, off)
	if b, ok := s.buffers[key]; ok && off >= b.start && off <= b.end {
		atomic.StoreInt64(&b.lastAccess, time.Now().UnixNano())
		if end <= b.end {
			// Fast path: entirely within one chunk.
			src := b.data[off-b.start : end-b.start+1]
			return copy(dest, src)
		}
		// Cross-boundary read: same logic as Get().
		if b2, ok2 := s.buffers[c.chunkKey(p, end)]; ok2 && b2.start == b.end+1 && b2.end >= end {
			atomic.StoreInt64(&b2.lastAccess, time.Now().UnixNano())
			n1 := copy(dest, b.data[off-b.start:])
			n2 := copy(dest[n1:], b2.data[:end-b2.start+1])
			return n1 + n2
		}
	}
	return 0
}

func (c *Cache) Put(p string, start, end int64, d []byte) {
	shard := c.getShard(p)
	key := c.chunkKey(p, start)
	dataSize := int64(len(d))

	// Pooled buffers all have cap == ReadAheadBase; adaptive chunk sizes are always <= base
	// (guaranteed by SetPieceLen), so a pooled buffer resliced to dataSize serves any chunk.
	// Fresh allocations get cap rounded up to base too, so recycle() can pool them later —
	// previously a size mismatch both discarded the pooled buffer and allocated an
	// unpoolable one, permanently draining the pool on non-power-of-two-piece torrents.
	poolCap := int64(c.cfg.Base())
	if poolCap == 0 {
		poolCap = 16 * 1024 * 1024
	}
	var dataCopy []byte
	select {
	case buf := <-c.pool:
		if int64(cap(buf)) >= dataSize {
			dataCopy = buf[:dataSize]
		} else {
			// Buffer predates a smaller base config; too small now, let GC take it.
			dataCopy = allocChunk(dataSize, poolCap)
		}
	default:
		dataCopy = allocChunk(dataSize, poolCap)
	}
	copy(dataCopy, d)

	globalLimit := c.cfg.Budget()
	if globalLimit <= 0 {
		globalLimit = 256 * 1024 * 1024 // Fail-safe default
	}
	responsiveOnly := torrstor.IsResponsive()

	shard.mu.Lock()
	// Account for an overwrite; the new chunk gets a fresh write sequence.
	if old, ok := shard.buffers[key]; ok {
		shard.total -= int64(len(old.data))
		atomic.AddInt64(&c.used, -int64(len(old.data)))
		c.recycle(old.data)
	}
	fresh := &raBuffer{p, start, end, dataCopy, time.Now().UnixNano(), c.seq.Add(1), responsiveOnly}
	shard.buffers[key] = fresh
	shard.total += dataSize
	newUsed := atomic.AddInt64(&c.used, dataSize)
	shard.mu.Unlock()

	if newUsed > globalLimit {
		c.evictOverBudget(fresh, globalLimit)
	}
}

// pathStat is what one eviction scan learns about a path.
type pathStat struct {
	bytes     int64
	lastAcc   int64
	player    int64
	hasPlayer bool
	idle      bool
}

// victim is the best eviction candidate found so far.
type victim struct {
	reason int
	st     pathStat
	shard  *raShard
	key    string
	buf    *raBuffer
}

// better reports whether a should be evicted before b (same-reason candidates compare by the
// reason's own rule).
func better(a, b victim) bool {
	if a.reason != b.reason {
		return a.reason < b.reason
	}
	switch a.reason {
	case ReasonIdle, ReasonBehind:
		// Least recently touched first.
		return atomic.LoadInt64(&a.buf.lastAccess) < atomic.LoadInt64(&b.buf.lastAccess)
	}
	// Ahead: the path holding the most bytes gives one up first.
	if a.st.bytes != b.st.bytes {
		return a.st.bytes > b.st.bytes
	}
	if a.st.hasPlayer && b.st.hasPlayer {
		return a.buf.start > b.buf.start // farthest ahead of its player
	}
	return a.buf.seq < b.buf.seq // player unknown: oldest written, as for a lone stream
}

// scanPaths fills c.scratch with per-path stats, one shard read lock at a time, then asks
// PlayerOff (no lock held) and marks idle paths. It returns the fair share per live path.
// Caller holds muEvict.
func (c *Cache) scanPaths(now int64, limit int64) int64 {
	clear(c.scratch)
	for _, s := range c.shards {
		s.mu.RLock()
		for _, b := range s.buffers {
			st := c.scratch[b.path]
			st.bytes += int64(len(b.data))
			if la := atomic.LoadInt64(&b.lastAccess); la > st.lastAcc {
				st.lastAcc = la
			}
			c.scratch[b.path] = st
		}
		s.mu.RUnlock()
	}
	live := int64(0)
	for path, st := range c.scratch {
		st.idle = now-st.lastAcc > staleWindow.Nanoseconds()
		if !st.idle {
			live++
		}
		if c.cfg.PlayerOff != nil {
			st.player, st.hasPlayer = c.cfg.PlayerOff(path)
		}
		c.scratch[path] = st
	}
	return limit / max(live, 1)
}

// removeLocked drops b from s. Caller holds s.mu for writing.
func (c *Cache) removeLocked(s *raShard, key string, b *raBuffer) {
	size := int64(len(b.data))
	s.total -= size
	delete(s.buffers, key)
	atomic.AddInt64(&c.used, -size)
	c.recycle(b.data)
}

// evictOverBudget drops chunks until the cache fits its budget, never fresh (the chunk just
// written). Order: chunks of idle paths; chunks entirely behind their player; then the chunk
// farthest ahead of its player on the path holding the most bytes (a path whose player is
// unknown gives up its oldest-written chunk instead). It scans every chunk once per eviction,
// which is cheap beside the multi-MiB Put that triggers it. It holds no two locks at once.
func (c *Cache) evictOverBudget(fresh *raBuffer, limit int64) {
	c.muEvict.Lock()
	defer c.muEvict.Unlock()

	for atomic.LoadInt64(&c.used) > limit {
		now := time.Now().UnixNano()
		share := c.scanPaths(now, limit)

		var best victim
		found := false
		for _, s := range c.shards {
			s.mu.RLock()
			for key, b := range s.buffers {
				if b == fresh {
					continue
				}
				st := c.scratch[b.path]
				cand := victim{shard: s, key: key, buf: b, st: st, reason: ReasonAhead}
				if st.idle {
					cand.reason = ReasonIdle
				} else if st.hasPlayer && b.end < st.player {
					cand.reason = ReasonBehind
				}
				if !found || better(cand, best) {
					best, found = cand, true
				}
			}
			s.mu.RUnlock()
		}
		if !found {
			// Only the chunk just written is left: let it persist so a new stream can start.
			c.cfg.Logf("[RaCache] Budget full (%d MB), allowing active chunk to persist for handoff: %s",
				atomic.LoadInt64(&c.used)/(1024*1024), filepath.Base(fresh.path))
			return
		}

		best.shard.mu.Lock()
		if cur, ok := best.shard.buffers[best.key]; !ok || cur != best.buf {
			best.shard.mu.Unlock() // changed since the scan: look again
			continue
		}
		c.removeLocked(best.shard, best.key, best.buf)
		best.shard.mu.Unlock()
		c.evictions[best.reason].Add(1)

		st := best.st
		if st.hasPlayer && best.buf.start >= st.player && best.buf.start < st.player+share {
			c.nearPlayer.Add(1)
			last, seen := c.lastNear[best.buf.path]
			if !seen || time.Since(last) >= nearLogEvery {
				c.lastNear[best.buf.path] = time.Now()
				c.cfg.Logf("[RaCache] evicted a chunk %d MiB ahead of the player: %s reason=%s",
					(best.buf.start-st.player)/(1024*1024), filepath.Base(best.buf.path), ReasonNames[best.reason])
			}
		}
	}
}

// Stats returns read-ahead cache statistics for metrics endpoint
func (c *Cache) Stats() (totalBytes int64, activeBytes int64, entries int) {
	totalBytes = atomic.LoadInt64(&c.used)

	now := time.Now().UnixNano()
	staleThreshold := (120 * time.Second).Nanoseconds()

	for _, shard := range c.shards {
		shard.mu.RLock()
		entries += len(shard.buffers)
		for _, buf := range shard.buffers {
			if now-atomic.LoadInt64(&buf.lastAccess) < staleThreshold {
				activeBytes += int64(len(buf.data))
			}
		}
		shard.mu.RUnlock()
	}

	return totalBytes, activeBytes, entries
}

// sweepIdle drops every chunk of a path with no access within staleWindow. A path that is
// being read, however old some of its chunks are, is left alone.
func (c *Cache) sweepIdle() {
	c.muEvict.Lock()
	defer c.muEvict.Unlock()

	c.scanPaths(time.Now().UnixNano(), 1)
	for _, s := range c.shards {
		s.mu.Lock()
		for key, b := range s.buffers {
			if c.scratch[b.path].idle {
				c.removeLocked(s, key, b)
			}
		}
		s.mu.Unlock()
	}
	for path := range c.lastNear {
		if st, ok := c.scratch[path]; !ok || st.idle {
			delete(c.lastNear, path)
		}
	}
}

// Evictions returns how many chunks Put has dropped to stay under budget since the cache was
// built, over all reasons. Chunks removed by the idle sweep are not counted. Lock-free.
func (c *Cache) Evictions() int64 {
	var n int64
	for i := range c.evictions {
		n += c.evictions[i].Load()
	}
	return n
}

// EvictionsByReason splits Evictions by ReasonIdle, ReasonBehind and ReasonAhead.
func (c *Cache) EvictionsByReason() [numReasons]int64 {
	var out [numReasons]int64
	for i := range c.evictions {
		out[i] = c.evictions[i].Load()
	}
	return out
}

// NearPlayerEvictions counts evictions of a chunk that starts within [player, player + the
// path's fair share) for a path whose player is known: the harmful kind. Lock-free.
func (c *Cache) NearPlayerEvictions() int64 { return c.nearPlayer.Load() }
