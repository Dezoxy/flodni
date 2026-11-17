package readahead

import (
	"bytes"
	"fmt"
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
	return New(Config{
		Base:   func() int64 { return testBase },
		Budget: func() int64 { return budget },
		Logf:   func(string, ...any) {},
		Go:     run,
	})
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

func TestSwitchContextMarksStaleSessions(t *testing.T) {
	c := newTestCache(t, 1<<20)
	c.SwitchContext("a")
	c.Put("a", 0, testBase-1, pattern(testBase, 0))
	c.Put("b", 0, testBase-1, pattern(testBase, 0)) // not the active path: session 0

	c.muContext.Lock()
	id1 := c.currentSessionID
	c.muContext.Unlock()
	if id1 != 1 {
		t.Fatalf("session id = %d, want 1", id1)
	}
	if got := c.getShard("a").buffers["a:0"].sessionID; got != 1 {
		t.Fatalf("active buffer session = %d, want 1", got)
	}
	if got := c.getShard("b").buffers["b:0"].sessionID; got != 0 {
		t.Fatalf("stale buffer session = %d, want 0", got)
	}

	c.SwitchContext("a") // same path: no change
	c.muContext.Lock()
	same := c.currentSessionID
	c.muContext.Unlock()
	if same != id1 {
		t.Fatal("same path must not bump the session")
	}

	c.SwitchContext("b")
	c.muContext.Lock()
	id2 := c.currentSessionID
	c.muContext.Unlock()
	if id2 != 2 {
		t.Fatalf("session id = %d, want 2", id2)
	}
}

// Eviction policy (triggerGlobalEviction): a buffer is dropped when its session is
// not the active one and its path is not the active path, or when it was not read
// within 120 s (60 s for the active path).
func TestTriggerGlobalEviction(t *testing.T) {
	c := newTestCache(t, 1<<20)
	c.SwitchContext("act") // session 1
	c.Put("act", 0, testBase-1, pattern(testBase, 0))
	c.Put("act", testBase, 2*testBase-1, pattern(testBase, 0))
	c.Put("old", 0, testBase-1, pattern(testBase, 0)) // session 0: stale

	age := func(path string, key string, d time.Duration) {
		s := c.getShard(path)
		s.mu.Lock()
		s.buffers[key].lastAccess = time.Now().Add(-d).UnixNano()
		s.mu.Unlock()
	}
	age("act", "act:1", 90*time.Second) // active path, idle > 60 s

	c.triggerGlobalEviction("act", 1)

	if !c.Exists("act", 0) {
		t.Fatal("fresh active chunk must survive")
	}
	if c.Exists("act", testBase) {
		t.Fatal("active chunk idle > 60s must be evicted")
	}
	if c.Exists("old", 0) {
		t.Fatal("stale-session chunk of another path must be evicted")
	}
	if total, _, entries := c.Stats(); total != testBase || entries != 1 {
		t.Fatalf("after eviction total=%d entries=%d", total, entries)
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
