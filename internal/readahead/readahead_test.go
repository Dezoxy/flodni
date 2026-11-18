package readahead

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

const testBase = 1024

// newTestCache builds a cache with a 1 KiB chunk size and the given budget.
// Background eviction is suppressed so tests are deterministic; tests call
// triggerGlobalEviction themselves.
func newTestCache(t *testing.T, budget int64) *Cache {
	t.Helper()
	return newCache(budget, func(func()) {})
}

func newCache(budget int64, run func(func())) *Cache {
	return newPlayerCache(budget, run, nil)
}

func newPlayerCache(budget int64, run func(func()), players func(string) (int64, bool)) *Cache {
	c := New(Config{
		Base:      func() int64 { return testBase },
		Budget:    func() int64 { return budget },
		Logf:      func(string, ...any) {},
		Go:        run,
		PlayerOff: players,
	})
	c.nearFloor = 1 // the 32 MiB floor would swallow a test budget of a few KiB
	return c
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return b
}

func TestPutGetRoundTrip(t *testing.T) {
	c := newTestCache(t, 1<<20)
	data := pattern(testBase, 1)
	c.Put("a", 0, testBase-1, data)

	tests := []struct {
		name     string
		off, end int64
		want     []byte
	}{
		{"whole chunk", 0, testBase - 1, data},
		{"middle", 10, 99, data[10:100]},
		{"single byte", 5, 5, data[5:6]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !c.Covered("a", tt.off, tt.end) {
				t.Fatal("Covered = false, want true")
			}
			if got := c.Get("a", tt.off, tt.end); !bytes.Equal(got, tt.want) {
				t.Fatalf("Get mismatch")
			}
			dest := make([]byte, len(tt.want))
			if n := c.CopyTo("a", tt.off, tt.end, dest); n != len(tt.want) || !bytes.Equal(dest, tt.want) {
				t.Fatalf("CopyTo n=%d, mismatch", n)
			}
		})
	}
}

func TestMissAndCopyIsDefensive(t *testing.T) {
	c := newTestCache(t, 1<<20)
	data := pattern(testBase, 3)
	c.Put("a", 0, testBase-1, data)

	if c.Covered("b", 0, 10) || c.Get("b", 0, 10) != nil || c.CopyTo("b", 0, 10, make([]byte, 11)) != 0 {
		t.Fatal("unknown path must miss")
	}
	if c.Covered("a", testBase, testBase+5) || c.Get("a", testBase, testBase+5) != nil {
		t.Fatal("uncached chunk must miss")
	}
	// Mutating a Get result and the Put input must not alter the cache.
	got := c.Get("a", 0, 9)
	got[0] ^= 0xff
	data[1] ^= 0xff
	if again := c.Get("a", 0, 9); again[0] != pattern(10, 3)[0] || again[1] != pattern(10, 3)[1] {
		t.Fatal("cache shares memory with caller")
	}
}

func TestChunkBoundaries(t *testing.T) {
	c := newTestCache(t, 1<<20)
	d0, d1 := pattern(testBase, 0), pattern(testBase, 100)
	c.Put("a", 0, testBase-1, d0)
	c.Put("a", testBase, 2*testBase-1, d1)

	all := append(append([]byte{}, d0...), d1...)
	// A read straddling the boundary is stitched from both chunks.
	off, end := int64(testBase-8), int64(testBase+7)
	want := all[off : end+1]
	if !c.Covered("a", off, end) {
		t.Fatal("straddling read not covered")
	}
	if got := c.Get("a", off, end); !bytes.Equal(got, want) {
		t.Fatal("Get straddle mismatch")
	}
	dest := make([]byte, len(want))
	if n := c.CopyTo("a", off, end, dest); n != len(want) || !bytes.Equal(dest, want) {
		t.Fatal("CopyTo straddle mismatch")
	}

	// Second chunk missing: the straddle must miss.
	c2 := newTestCache(t, 1<<20)
	c2.Put("a", 0, testBase-1, d0)
	if c2.Covered("a", off, end) || c2.Get("a", off, end) != nil || c2.CopyTo("a", off, end, dest) != 0 {
		t.Fatal("straddle with absent neighbour must miss")
	}
}

func TestExistsAndMaxCachedOffset(t *testing.T) {
	c := newTestCache(t, 1<<20)
	if c.Exists("a", 0) || c.MaxCachedOffset("a") != 0 {
		t.Fatal("empty cache")
	}
	c.Put("a", 0, testBase-1, pattern(testBase, 0))
	c.Put("a", 2*testBase, 3*testBase-1, pattern(testBase, 0))
	if !c.Exists("a", 0) || !c.Exists("a", 5) || c.Exists("a", testBase) || !c.Exists("a", 2*testBase) {
		t.Fatal("Exists wrong")
	}
	if got := c.MaxCachedOffset("a"); got != 3*testBase-1 {
		t.Fatalf("MaxCachedOffset = %d", got)
	}
	if c.MaxCachedOffset("b") != 0 {
		t.Fatal("other path must be 0")
	}
}

func TestOverwriteAccounting(t *testing.T) {
	c := newTestCache(t, 1<<20)
	c.Put("a", 0, testBase-1, pattern(testBase, 0))
	c.Put("a", 0, testBase-1, pattern(testBase, 9))
	total, _, entries := c.Stats()
	if total != testBase || entries != 1 {
		t.Fatalf("total=%d entries=%d, want %d/1", total, entries, testBase)
	}
	if got := c.Get("a", 0, 0); got[0] != 9 {
		t.Fatal("overwrite did not replace data")
	}
}

func TestPieceLenChunkSize(t *testing.T) {
	c := newTestCache(t, 1<<20)
	tests := []struct {
		name string
		pl   int64
		want int64
	}{
		{"unset falls back to base", -1, testBase},
		{"piece divides base", 256, testBase},
		{"piece fits non-evenly", 300, 900}, // largest multiple of pl <= base
		{"piece larger than base", 4096, testBase},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.SetPieceLen("p", tt.pl)
			if got := c.ChunkSize("p"); got != tt.want {
				t.Fatalf("ChunkSize = %d, want %d", got, tt.want)
			}
		})
	}
	c.SetPieceLen("p", 300)
	c.ClearPieceLen("p")
	if c.ChunkSize("p") != testBase {
		t.Fatal("ClearPieceLen did not reset")
	}
}

func TestPutKeepsTotalUnderBudget(t *testing.T) {
	const budget = 4 * testBase
	c := newTestCache(t, budget)
	c.SwitchContext("a")
	// Same path hashes to one shard, so this exercises the in-shard LRU eviction.
	for i := int64(0); i < 20; i++ {
		c.Put("a", i*testBase, (i+1)*testBase-1, pattern(testBase, byte(i)))
		if total, _, _ := c.Stats(); total > budget {
			t.Fatalf("after put %d: total %d > budget %d", i, total, budget)
		}
	}
	// Oldest chunks go first; the newest survives.
	if c.Exists("a", 0) {
		t.Fatal("oldest chunk must have been evicted")
	}
	if !c.Exists("a", 19*testBase) {
		t.Fatal("newest chunk must survive")
	}
}

func TestPutKeepsOnlyChunkWhenOverBudget(t *testing.T) {
	c := newTestCache(t, testBase/2) // budget smaller than a single chunk
	c.Put("a", 0, testBase-1, pattern(testBase, 0))
	if !c.Exists("a", 0) {
		t.Fatal("the only chunk must persist for handoff")
	}
}

func TestAnchorPoolToConfig(t *testing.T) {
	tests := []struct {
		name   string
		budget int64
		want   int
	}{
		{"floor", 2 * testBase, 16},
		{"scaled", 20 * testBase, 40},
		{"ceiling", 1000 * testBase, 64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestCache(t, tt.budget)
			c.AnchorPoolToConfig()
			if got := cap(c.pool); got != tt.want {
				t.Fatalf("pool cap = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestConcurrentPutGet(t *testing.T) {
	c := newCache(16*testBase, func(fn func()) { go fn() })
	c.SwitchContext("p0")
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			path := fmt.Sprintf("p%d", g%4)
			dest := make([]byte, 64)
			for i := int64(0); i < 200; i++ {
				off := (i % 32) * testBase
				c.Put(path, off, off+testBase-1, pattern(testBase, byte(i)))
				c.Covered(path, off, off+63)
				c.Get(path, off, off+63)
				c.CopyTo(path, off, off+63, dest)
				c.Exists(path, off)
				c.MaxCachedOffset(path)
				c.Stats()
				if i%50 == 0 {
					c.SwitchContext(path)
				}
			}
		}(g)
	}
	wg.Wait()
	time.Sleep(50 * time.Millisecond) // let background evictions settle
	if total, _, _ := c.Stats(); total < 0 {
		t.Fatalf("negative accounting: %d", total)
	}
}

func TestEvictionsCountsPutDrops(t *testing.T) {
	const budget = 4 * testBase
	c := newTestCache(t, budget)
	c.SwitchContext("a")
	for i := int64(0); i < 4; i++ {
		c.Put("a", i*testBase, (i+1)*testBase-1, pattern(testBase, byte(i)))
	}
	if got := c.Evictions(); got != 0 {
		t.Fatalf("under budget: evictions = %d, want 0", got)
	}
	for i := int64(4); i < 10; i++ {
		c.Put("a", i*testBase, (i+1)*testBase-1, pattern(testBase, byte(i)))
	}
	if got := c.Evictions(); got != 6 {
		t.Fatalf("after 6 puts over budget: evictions = %d, want 6", got)
	}
	// An overwrite is not an eviction.
	c.Put("a", 9*testBase, 10*testBase-1, pattern(testBase, 99))
	if got := c.Evictions(); got != 6 {
		t.Fatalf("overwrite changed evictions to %d", got)
	}
}

func TestCoveredRun(t *testing.T) {
	c := newTestCache(t, 1<<20)
	c.Put("a", 0, testBase-1, pattern(testBase, 1))
	c.Put("a", testBase, 2*testBase-1, pattern(testBase, 2))
	c.Put("a", 3*testBase, 3*testBase+99, pattern(100, 3)) // after a gap, partly filled

	tests := []struct {
		name       string
		off, limit int64
		want       int64
	}{
		{"across two chunks to the gap", 10, 10 * testBase, 2*testBase - 10},
		{"capped by limit", 10, 100, 90},
		{"inside the second chunk", testBase + 5, 10 * testBase, testBase - 5},
		{"a gap gives nothing", 2 * testBase, 10 * testBase, 0},
		{"a partly filled chunk ends at its end", 3 * testBase, 10 * testBase, 100},
		{"unknown path", 0, 10, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := "a"
			if tt.name == "unknown path" {
				p = "b"
			}
			if got := c.CoveredRun(p, tt.off, tt.limit); got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}

// Asking never refreshes the LRU stamp: the metrics scrape must not keep chunks alive.
func TestCoveredRunLeavesLastAccess(t *testing.T) {
	c := newTestCache(t, 1<<20)
	c.Put("a", 0, testBase-1, pattern(testBase, 1))
	s := c.getShard("a")
	b := s.buffers[c.chunkKey("a", 0)]
	b.lastAccess = 42
	c.CoveredRun("a", 0, testBase)
	if b.lastAccess != 42 {
		t.Fatalf("lastAccess changed to %d", b.lastAccess)
	}
}

// players is a settable player table for tests.
type players struct {
	mu sync.Mutex
	m  map[string]int64
}

func (p *players) set(path string, off int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]int64{}
	}
	p.m[path] = off
}

func (p *players) get(path string) (int64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	off, ok := p.m[path]
	return off, ok
}

func syncRun(fn func()) { fn() }

func putChunk(c *Cache, path string, i int64) {
	c.Put(path, i*testBase, (i+1)*testBase-1, pattern(testBase, byte(i)))
}

// touch sets the last access of every chunk of path to now-age.
func touch(c *Cache, path string, age time.Duration) {
	for _, s := range c.shards {
		s.mu.Lock()
		for k, b := range s.buffers {
			if strings.HasPrefix(k, path+":") {
				b.lastAccess = time.Now().Add(-age).UnixNano()
			}
		}
		s.mu.Unlock()
	}
}

// touchChunk sets the last access of chunk i of path.
func touchChunk(c *Cache, path string, i int64, age time.Duration) {
	s := c.getShard(path)
	s.mu.Lock()
	s.buffers[c.chunkKey(path, i*testBase)].lastAccess = time.Now().Add(-age).UnixNano()
	s.mu.Unlock()
}

func present(c *Cache, path string, from, to int64) []int64 {
	var out []int64
	for i := from; i <= to; i++ {
		if c.Exists(path, i*testBase) {
			out = append(out, i)
		}
	}
	return out
}

func wantChunks(t *testing.T, c *Cache, path string, from, to int64, want ...int64) {
	t.Helper()
	got := present(c, path, from, to)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s chunks present = %v, want %v", path, got, want)
	}
}

// Opening file B must not drop file A's chunks while A is being read; once A is idle past
// the window, the next open sweeps it.
func TestOpenDoesNotDropAliveOtherPath(t *testing.T) {
	c := newCache(1<<20, syncRun)
	c.SwitchContext("A")
	for i := int64(0); i < 4; i++ {
		putChunk(c, "A", i)
	}
	c.Get("A", 0, 9) // A is being read
	c.SwitchContext("B")
	putChunk(c, "B", 0)
	wantChunks(t, c, "A", 0, 5, 0, 1, 2, 3)

	// A's old chunks inside a live path stay too: the player will still reach them.
	touchChunk(c, "A", 3, time.Second)
	touchChunk(c, "A", 0, 200*time.Second)
	c.SwitchContext("C")
	wantChunks(t, c, "A", 0, 5, 0, 1, 2, 3)

	// A goes idle: the next open drops it, and B (alive) stays.
	touch(c, "A", 200*time.Second)
	c.SwitchContext("D")
	wantChunks(t, c, "A", 0, 5)
	wantChunks(t, c, "B", 0, 1, 0)
	if total, _, _ := c.Stats(); total != testBase {
		t.Fatalf("total = %d, want %d", total, testBase)
	}
}

// Over budget, idle paths go first even when a live path has chunks behind its player.
func TestEvictIdlePathFirst(t *testing.T) {
	var pl players
	c := newPlayerCache(6*testBase, syncRun, pl.get)
	pl.set("A", 3*testBase)
	for i := int64(0); i < 3; i++ {
		putChunk(c, "A", i) // all behind A's player
	}
	for i := int64(0); i < 3; i++ {
		putChunk(c, "idle", i)
	}
	touch(c, "idle", 300*time.Second)
	putChunk(c, "A", 3)
	if got := c.EvictionsByReason(); got != [3]int64{1, 0, 0} {
		t.Fatalf("by reason = %v, want one idle", got)
	}
	wantChunks(t, c, "A", 0, 5, 0, 1, 2, 3)
	if n := len(present(c, "idle", 0, 5)); n != 2 {
		t.Fatalf("idle chunks left = %d, want 2", n)
	}
}

// Two streams over budget: consumed chunks go first, then the farthest-ahead chunk of the
// stream holding the most bytes; the chunks just ahead of each player survive.
func TestTwoStreamsEvictBehindThenFarthestAhead(t *testing.T) {
	var pl players
	c := newPlayerCache(10*testBase, syncRun, pl.get)
	pl.set("A", 3*testBase+5) // inside chunk 3: chunks 0..2 are consumed
	pl.set("B", 5)
	for i := int64(0); i < 8; i++ {
		putChunk(c, "A", i)
		touchChunk(c, "A", i, time.Duration(20-i)*time.Second) // chunk 0 least recently touched
	}
	putChunk(c, "B", 0)
	putChunk(c, "B", 1)

	putChunk(c, "B", 2)
	putChunk(c, "B", 3)
	putChunk(c, "B", 4)
	wantChunks(t, c, "A", 0, 7, 3, 4, 5, 6, 7) // the three consumed chunks went
	wantChunks(t, c, "B", 0, 4, 0, 1, 2, 3, 4)
	if got := c.EvictionsByReason(); got != [3]int64{0, 3, 0} {
		t.Fatalf("by reason = %v, want 3 behind", got)
	}

	// Nothing is consumed now. A holds 5, B 5: B's new chunk makes B the larger, so B
	// gives up its farthest-ahead chunk that is not the new one.
	putChunk(c, "B", 5)
	wantChunks(t, c, "B", 0, 5, 0, 1, 2, 3, 5)
	wantChunks(t, c, "A", 0, 7, 3, 4, 5, 6, 7)

	// A grows past B: its farthest chunk other than the new one goes each time, the ones
	// at its player stay.
	putChunk(c, "A", 8)
	putChunk(c, "A", 9)
	wantChunks(t, c, "A", 0, 9, 3, 4, 5, 6, 9)
	if got := c.EvictionsByReason(); got[ReasonAhead] != 3 || got[ReasonIdle] != 0 {
		t.Fatalf("by reason = %v", got)
	}
	if total, _, _ := c.Stats(); total > 10*testBase {
		t.Fatalf("total %d over budget", total)
	}
}

// The regression seen live: with the budget full, stream B's pump writing ahead used to
// evict B's own oldest chunk, the one at its player, so the contiguous run from the player
// collapsed to nothing. Both runs must stay, and no eviction may land near a player.
func TestFullBudgetPumpKeepsRunFromPlayer(t *testing.T) {
	var pl players
	c := newPlayerCache(10*testBase, syncRun, pl.get)
	pl.set("A", 0)
	pl.set("B", 0)
	for i := int64(0); i < 4; i++ {
		putChunk(c, "A", i)
		putChunk(c, "B", i)
	}
	for i := int64(4); i < 40; i++ {
		putChunk(c, "B", i) // B pumps on; its player does not move
		if run := c.CoveredRun("B", 0, 50*testBase); run < 4*testBase {
			t.Fatalf("after B chunk %d: run from B's player = %d", i, run)
		}
		if run := c.CoveredRun("A", 0, 50*testBase); run != 4*testBase {
			t.Fatalf("after B chunk %d: run from A's player = %d, want %d", i, run, 4*testBase)
		}
	}
	if n := c.NearPlayerEvictions(); n != 0 {
		t.Fatalf("near-player evictions = %d, want 0", n)
	}
	if c.EvictionsByReason()[ReasonAhead] == 0 {
		t.Fatal("expected ahead evictions")
	}
}

// A chunk dropped inside the player's fair-share window is counted as harmful.
func TestNearPlayerEvictionCounted(t *testing.T) {
	var pl players
	c := newPlayerCache(4*testBase, syncRun, pl.get)
	pl.set("A", 0)
	for i := int64(0); i < 4; i++ {
		putChunk(c, "A", i)
	}
	putChunk(c, "A", 4) // alone on the cache: evicts chunk 3, 3 chunks from the player < share (4)
	if n := c.NearPlayerEvictions(); n != 1 {
		t.Fatalf("near-player evictions = %d, want 1", n)
	}
}

// One stream: chunks behind the player go oldest first and everything from the player on is
// kept, exactly as the oldest-written rule did for a player that keeps ahead of its pump.
func TestSingleStreamEvictsBehindKeepsAhead(t *testing.T) {
	for _, tc := range []struct {
		name string
		pl   func(string) (int64, bool)
		move func(*players, int64)
	}{
		{"player known", nil, nil},
		{"player unknown", func(string) (int64, bool) { return 0, false }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pl players
			get := tc.pl
			if get == nil {
				get = pl.get
			}
			c := newPlayerCache(5*testBase, syncRun, get)
			for i := int64(0); i < 20; i++ {
				pl.set("a", (i-2)*testBase+1) // the player trails the pump by 2 chunks
				putChunk(c, "a", i)
				if total, _, _ := c.Stats(); total > 5*testBase {
					t.Fatalf("put %d: total %d over budget", i, total)
				}
			}
			wantChunks(t, c, "a", 0, 19, 15, 16, 17, 18, 19)
			if got := c.Evictions(); got != 15 {
				t.Fatalf("evictions = %d, want 15", got)
			}
		})
	}
}

// Parallel Put/Get/SwitchContext with players; accounting must stay exact.
func TestConcurrentMultiStream(t *testing.T) {
	var pl players
	var bg sync.WaitGroup
	c := newPlayerCache(12*testBase, func(fn func()) {
		bg.Add(1)
		go func() { defer bg.Done(); fn() }()
	}, pl.get)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			path := fmt.Sprintf("p%d", g%4)
			dest := make([]byte, 64)
			for i := int64(0); i < 300; i++ {
				pl.set(path, (i/2)*testBase)
				putChunk(c, path, i%40)
				c.Covered(path, 0, 63)
				c.Get(path, (i/2%40)*testBase, (i/2%40)*testBase+63)
				c.CopyTo(path, (i%40)*testBase, (i%40)*testBase+63, dest)
				c.CoveredRun(path, 0, 40*testBase)
				c.Stats()
				if i%25 == 0 {
					c.SwitchContext(path)
				}
			}
		}(g)
	}
	wg.Wait()
	bg.Wait()
	var sum int64
	for _, s := range c.shards {
		s.mu.RLock()
		var shardSum int64
		for _, b := range s.buffers {
			shardSum += int64(len(b.data))
		}
		if shardSum != s.total {
			t.Errorf("shard total %d != sum %d", s.total, shardSum)
		}
		sum += shardSum
		s.mu.RUnlock()
	}
	if used := c.used; used != sum {
		t.Fatalf("used %d != sum of chunks %d", used, sum)
	}
	if sum > 12*testBase {
		t.Fatalf("total %d over budget", sum)
	}
}

// A path with a player is being watched, however long it has gone without a read: a paused
// viewer's chunks survive another file's open and an over-budget eviction.
func TestPausedViewerIsNeverIdle(t *testing.T) {
	var pl players
	c := newPlayerCache(6*testBase, syncRun, pl.get)
	pl.set("paused", 0)
	for i := int64(0); i < 3; i++ {
		putChunk(c, "paused", i)
	}
	touch(c, "paused", 600*time.Second)
	c.SwitchContext("other")
	wantChunks(t, c, "paused", 0, 5, 0, 1, 2)

	for i := int64(0); i < 3; i++ { // a scan-like path with no player, then one chunk over
		putChunk(c, "scan", i)
	}
	putChunk(c, "scan", 3)
	wantChunks(t, c, "paused", 0, 5, 0, 1, 2)
	if got := c.EvictionsByReason(); got[ReasonIdle] != 0 {
		t.Fatalf("by reason = %v: the paused path must not be idle", got)
	}

	// Once the player is gone it is idle like any other path.
	pl.mu.Lock()
	delete(pl.m, "paused")
	pl.mu.Unlock()
	c.SwitchContext("third")
	wantChunks(t, c, "paused", 0, 5)
}

// Over budget a path with no player (a scan) gives up chunks before a viewer, even when the
// viewer's path holds more bytes.
func TestNoPlayerPathLosesChunksBeforeAViewer(t *testing.T) {
	var pl players
	c := newPlayerCache(10*testBase, syncRun, pl.get)
	pl.set("viewer", 0)
	for i := int64(0); i < 6; i++ {
		putChunk(c, "viewer", i)
	}
	for i := int64(0); i < 4; i++ {
		putChunk(c, "scan", i)
	}
	for i := int64(4); i < 7; i++ {
		putChunk(c, "scan", i)
	}
	wantChunks(t, c, "viewer", 0, 9, 0, 1, 2, 3, 4, 5)
	if n := len(present(c, "scan", 0, 9)); n != 4 {
		t.Fatalf("scan chunks = %d, want 4", n)
	}
}

// The order differs from oldest-written-first in two places, pinned here.
func TestEvictionOrderDiffersFromFIFO(t *testing.T) {
	t.Run("a cached tail chunk goes before the chunks at the player", func(t *testing.T) {
		var pl players
		c := newPlayerCache(4*testBase, syncRun, pl.get)
		pl.set("a", 0)
		for _, i := range []int64{0, 1, 2, 50, 3} { // the tail is written fourth, not first
			putChunk(c, "a", i)
		}
		wantChunks(t, c, "a", 0, 60, 0, 1, 2, 3)
	})
	t.Run("a backward re-read keeps the re-read chunk behind the player", func(t *testing.T) {
		var pl players
		c := newPlayerCache(4*testBase, syncRun, pl.get)
		pl.set("a", 3*testBase+1)
		for i := int64(0); i < 4; i++ {
			putChunk(c, "a", i)
			touchChunk(c, "a", i, time.Duration(10-i)*time.Second)
		}
		c.Get("a", 0, 9) // the viewer seeks back into chunk 0: it is touched now
		putChunk(c, "a", 4)
		wantChunks(t, c, "a", 0, 4, 0, 2, 3, 4)
	})
}

// The near-player window is the budget over the paths with a player, floored (the lead
// floor), and never under one path.
func TestNearPlayerWindowIsBudgetOverPlayersWithAFloor(t *testing.T) {
	var pl players
	c := newPlayerCache(100*testBase, syncRun, pl.get)
	c.nearFloor = 40 * testBase
	for _, p := range []string{"a", "b", "c"} {
		pl.set(p, 0)
	}
	for i := int64(0); i < 38; i++ { // a 38, b 31, c 30: 99 chunks
		putChunk(c, "a", i)
	}
	for i := int64(0); i < 31; i++ {
		putChunk(c, "b", i)
	}
	for i := int64(0); i < 30; i++ {
		putChunk(c, "c", i)
	}
	putChunk(c, "a", 38) // 100: fits
	if n := c.NearPlayerEvictions(); n != 0 {
		t.Fatalf("near = %d before any eviction", n)
	}
	// 101: a's farthest chunk other than the new one, chunk 38, is 38 chunks ahead. The
	// share is 100/3 = 33, floored to 40, so it counts; without the floor it would not.
	putChunk(c, "a", 39)
	if n := c.NearPlayerEvictions(); n != 1 {
		t.Fatalf("near = %d, want 1", n)
	}
	if got := c.EvictionsByReason(); got[ReasonAhead] != 1 {
		t.Fatalf("by reason = %v", got)
	}
}
