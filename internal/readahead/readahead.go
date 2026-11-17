// Package readahead is the sharded, session-aware chunk cache that sits between
// the torrent pump and FUSE reads.
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

type raBuffer struct {
	start, end     int64
	data           []byte
	lastAccess     int64
	sessionID      int64
	responsiveOnly bool // true if written in non-verified mode (responsive shield active)
}

// Config injects what the cache needs from the daemon. Base and Budget are read
// on every use (config can be reloaded, and is not yet loaded when the cache is
// constructed); all four fields are required.
type Config struct {
	Base   func() int64                     // chunk size (ReadAheadBase), 0 means 16MB
	Budget func() int64                     // total budget in bytes (ReadAheadBudget)
	Logf   func(format string, args ...any) // logger
	Go     func(fn func())                  // panic-safe background goroutine launcher
}

// Cache is a 32-shard LRU cache with session-aware eviction.
type Cache struct {
	cfg       Config
	shards    [32]*raShard
	shardMask uint64
	used      int64
	pool      chan []byte // recycled 16MB chunks

	muContext        sync.Mutex
	activePath       string
	currentSessionID int64
	isEvicting       int32 // atomic flag prevents concurrent global evictions

	// evictions counts chunks Put dropped to stay under budget; only rises.
	evictions atomic.Int64

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
	order   []string
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

// SwitchContext increments SessionID on path change to invalidate stale data.
func (c *Cache) SwitchContext(newPath string) {
	c.muContext.Lock()
	pathChanged := newPath != c.activePath
	if pathChanged {
		c.activePath = newPath
		// Increment session ID: All old buffers (with old ID) become "stale" instantly
		c.currentSessionID++
	}
	activePath := c.activePath
	activeSessionID := c.currentSessionID
	c.muContext.Unlock()

	if pathChanged {
		c.cfg.Go(func() {
			c.triggerGlobalEviction(activePath, activeSessionID)
		})
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
	c.muContext.Lock()
	sessID := c.currentSessionID
	// Active path uses current session ID; stale pumps use sessID=0 so eviction can identify them.
	if p != c.activePath {
		sessID = 0
	}
	c.muContext.Unlock()

	shard := c.getShard(p)
	shard.mu.Lock()
	defer shard.mu.Unlock()

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

	// 1. Account for overwrite
	if old, ok := shard.buffers[key]; ok {
		shard.total -= int64(len(old.data))
		atomic.AddInt64(&c.used, -int64(len(old.data)))
		c.recycle(old.data)
		// Promote to MRU on overwrite to prevent premature eviction of recently refreshed chunks.
		for i, k := range shard.order {
			if k == key {
				shard.order = append(shard.order[:i], shard.order[i+1:]...)
				break
			}
		}
	}
	shard.order = append(shard.order, key)

	// 2. Add new data
	shard.total += dataSize
	newUsed := atomic.AddInt64(&c.used, dataSize)
	responsiveOnly := torrstor.IsResponsive()
	shard.buffers[key] = &raBuffer{start, end, dataCopy, time.Now().UnixNano(), sessID, responsiveOnly}

	// Evict from this shard while over budget; keep last item to avoid evicting the chunk just added.
	for newUsed > globalLimit && len(shard.order) > 1 {
		v := shard.order[0]
		shard.order = shard.order[1:]
		if old, ok := shard.buffers[v]; ok {
			evictedSize := int64(len(old.data))
			shard.total -= evictedSize
			delete(shard.buffers, v)
			newUsed = atomic.AddInt64(&c.used, -evictedSize)
			c.evictions.Add(1)
			c.recycle(old.data)
		}
	}

	// Local shard exhausted: trigger global eviction to free stale data from other shards.
	if newUsed > globalLimit && len(shard.order) <= 1 {
		c.muContext.Lock()
		ap := c.activePath
		sid := c.currentSessionID
		c.muContext.Unlock()
		c.cfg.Go(func() {
			c.triggerGlobalEviction(ap, sid)
		})
	}

	if newUsed > globalLimit && len(shard.order) == 0 {
		// If we reach here, it means even after global eviction we are over budget.
		// However, to allow the new stream to start, we must NOT drop the only chunk it has.
		// So we do nothing and allow a tiny over-budget for the first few chunks.
		c.cfg.Logf("[RaCache] Budget full (%d MB), allowing active chunk to persist for handoff: %s", newUsed/(1024*1024), filepath.Base(p))
	}

	// Periodic compaction of order slice
	if len(shard.order) > 100 && len(shard.order) > len(shard.buffers)*2 {
		newOrder := make([]string, 0, len(shard.buffers))
		for _, v := range shard.order {
			if _, exists := shard.buffers[v]; exists {
				newOrder = append(newOrder, v)
			}
		}
		shard.order = newOrder
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

// triggerGlobalEviction removes stale session data and old chunks from all shards.
func (c *Cache) triggerGlobalEviction(activePath string, activeSessionID int64) {
	// Single-flight: skip if already evicting.
	if !atomic.CompareAndSwapInt32(&c.isEvicting, 0, 1) {
		return
	}
	defer atomic.StoreInt32(&c.isEvicting, 0)

	now := time.Now().UnixNano()
	staleThreshold := (120 * time.Second).Nanoseconds()

	evictShard := func(s *raShard) {
		var newOrder []string
		for _, key := range s.order {
			keep := true
			if buf, ok := s.buffers[key]; ok {
				// 1. Session ID Check (Fastest)
				if buf.sessionID != activeSessionID && !strings.HasPrefix(key, activePath+":") {
					keep = false
				} else {
					threshold := staleThreshold
					if strings.HasPrefix(key, activePath+":") {
						threshold = (60 * time.Second).Nanoseconds()
					}

					lastAcc := atomic.LoadInt64(&buf.lastAccess)
					if now-lastAcc > threshold {
						keep = false
					}
				}

				if !keep {
					size := int64(len(buf.data))
					s.total -= size
					delete(s.buffers, key)
					atomic.AddInt64(&c.used, -size)
					c.recycle(buf.data)
				}
			}

			if keep {
				newOrder = append(newOrder, key)
			}
		}
		s.order = newOrder
	}

	skipped := 0
	for _, s := range c.shards {
		if !s.mu.TryLock() {
			skipped++
			continue
		}
		evictShard(s)
		s.mu.Unlock()
	}

	// If all shards were busy, force blocking eviction on first shard to prevent budget overflow.
	if skipped == len(c.shards) {
		s := c.shards[0]
		s.mu.Lock()
		evictShard(s)
		s.mu.Unlock()
	}
}

// Evictions returns how many chunks Put has dropped to stay under budget since the cache was
// built. Chunks removed by the stale sweep are not counted. Lock-free.
func (c *Cache) Evictions() int64 { return c.evictions.Load() }
