package torrstor

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	torrenttypes "github.com/anacrolix/torrent/types"

	"flodni/internal/gostorm/settings"
)

const keepPieceLen = int64(1 << 20)

// newKeepCache is a 100-piece torrent of 1 MiB pieces whose cache holds capPcs of them. The
// first and last 8 pieces are always kept (isIdInFileBE), so the tests stay in 8..92.
func newKeepCache(t *testing.T, capPcs int64) (*torrent.Torrent, *Cache) {
	t.Helper()
	oldSets := settings.BTsets
	// Registered first, so it runs last: Close starts a background rebuild that reads settings.
	t.Cleanup(func() { settings.BTsets = oldSets })
	settings.BTsets = &settings.BTSets{ReaderReadAHead: 75}
	tor, c := newPriorityTestCacheSized(t, 100, keepPieceLen)
	c.capacity = capPcs * keepPieceLen
	t.Cleanup(func() { _ = c.Close() })
	return tor, c
}

// keepReader adds an open-ended reader at piece pos with a read-ahead of ra pieces. It is
// marked as just used, or the idle sweep parks it as soon as a second reader exists.
func keepReader(c *Cache, tor *torrent.Torrent, pos, ra int64) *Reader {
	r := c.NewReader(tor.Files()[0])
	r.mu.Lock()
	r.offset = pos * keepPieceLen
	r.lastAccess = time.Now().Unix()
	r.mu.Unlock()
	r.SetReadahead(ra * keepPieceLen)
	return r
}

// complete stores piece id whole, with the given Accessed stamp.
func complete(t *testing.T, c *Cache, id int, accessed int64) {
	t.Helper()
	fetch(t, c, id, int(keepPieceLen), accessed)
	c.pieces[id].Complete.Store(true)
}

// fakeClock is the cache's clock in tests. The cleaner and the cleanups a close starts read it
// from their own goroutines, so it is atomic.
type fakeClock struct{ ns atomic.Int64 }

func newFakeClock(c *Cache) *fakeClock {
	f := &fakeClock{}
	f.ns.Store(time.Now().UnixNano())
	c.clock = func() time.Time { return time.Unix(0, f.ns.Load()) }
	return f
}

func (f *fakeClock) advance(d time.Duration) { f.ns.Add(int64(d)) }

// evictable is getRemPieces as the cleaner calls it: under muRemove, which serialises the
// rebuild of the shared pieceInRange bitmap with the one a reader close starts.
func evictable(c *Cache) []*Piece {
	c.muRemove.Lock()
	defer c.muRemove.Unlock()
	return c.getRemPieces()
}

func ids(ps []*Piece) []int {
	out := make([]int, len(ps))
	for i, p := range ps {
		out[i] = p.Id
	}
	return out
}

func stored(c *Cache) (n int64) {
	for _, p := range c.pieces {
		n += atomic.LoadInt64(&p.Size)
	}
	return n
}

// A bounded reader takes no share of the cache: attaching one leaves the open-ended reader's
// protected window as it was, where dividing by every reader halved it. A second open-ended
// reader still splits it, and the bounded reader's own range is capped at the same share.
func TestBoundedReaderDoesNotShrinkOpenEndedWindow(t *testing.T) {
	tor, c := newKeepCache(t, 40) // protected = 34 pieces for one open-ended reader
	open := keepReader(c, tor, 50, 8)
	b0, e0 := open.getOffsetRange()
	if want := int64(34) * keepPieceLen; e0-b0 != want {
		t.Fatalf("setup: window %d bytes, want %d", e0-b0, want)
	}

	bounded := keepReader(c, tor, 10, 8)
	bounded.SetBounded(14 * keepPieceLen)
	if b, e := open.getOffsetRange(); b != b0 || e != e0 {
		t.Errorf("window after a bounded reader attached = [%d, %d], want [%d, %d]", b, e, b0, e0)
	}
	if r := bounded.getPiecesRange(); r.Start != 10 || r.End != 13 {
		t.Errorf("bounded range = pieces %d..%d, want 10..13 (offset to range end)", r.Start, r.End)
	}

	// Past the slack, the range is cut: a huge range cannot pin the cache. Two bounded readers
	// split the 15% the open-ended windows leave: 6 pieces / 2 = 3 each.
	capped := keepReader(c, tor, 10, 8)
	capped.SetBounded(90 * keepPieceLen)
	if r := capped.getPiecesRange(); r.Start != 10 || r.End != 12 {
		t.Errorf("capped bounded range = pieces %d..%d, want 10..12 (3 pieces)", r.Start, r.End)
	}

	// The other side of the rule: an open-ended reader does take a share.
	keepReader(c, tor, 20, 8)
	if b, e := open.getOffsetRange(); e-b != 17*keepPieceLen {
		t.Errorf("window with two open-ended readers = %d bytes, want %d", e-b, 17*keepPieceLen)
	}
}

// Candidates are evicted by class before age: behind every reader first, then ahead of a
// bounded reader only, and the open-ended reader's unread read-ahead last, even when it is the
// oldest in the cache.
func TestEvictionOrderPrefersBehindOverUnreadReadahead(t *testing.T) {
	t.Run("behind before unread", func(t *testing.T) {
		tor, c := newKeepCache(t, 40)
		keepReader(c, tor, 50, 8) // window 24..58
		for i, id := range []int{10, 11, 12, 13, 14} {
			complete(t, c, id, 1000+int64(i)) // behind, newest
		}
		for i, id := range []int{70, 71, 72, 73, 74} {
			complete(t, c, id, 1+int64(i)) // unread read-ahead past the window, oldest
		}
		got := ids(evictable(c))
		want := []int{10, 11, 12, 13, 14, 70, 71, 72, 73, 74}
		if len(got) != len(want) {
			t.Fatalf("evictable = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("eviction order = %v, want %v", got, want)
			}
		}
	})
	t.Run("three classes, age inside a class", func(t *testing.T) {
		tor, c := newKeepCache(t, 40)
		keepReader(c, tor, 80, 8) // window 54..88 (open-ended)
		b := keepReader(c, tor, 30, 8)
		b.SetBounded(32 * keepPieceLen) // protects 30..31
		complete(t, c, 90, 1)           // unread read-ahead of the open-ended reader, oldest
		complete(t, c, 40, 50)          // ahead of the bounded reader only
		complete(t, c, 41, 49)
		complete(t, c, 10, 99) // behind both, newest
		complete(t, c, 11, 98)
		got := ids(evictable(c))
		want := []int{11, 10, 41, 40, 90}
		if len(got) != len(want) {
			t.Fatalf("evictable = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("eviction order = %v, want %v", got, want)
			}
		}
	})
}

// The order is a preference: when every candidate is unread read-ahead, eviction still frees
// the capacity instead of reporting nothing evictable.
func TestEvictsUnreadReadaheadWhenNothingElse(t *testing.T) {
	tor, c := newKeepCache(t, 20)
	keepReader(c, tor, 30, 4) // window 17..34
	for id := 60; id <= 84; id++ {
		complete(t, c, id, int64(id))
	}
	if got := stored(c); got <= c.capacity {
		t.Fatalf("setup: stored %d, want over capacity %d", got, c.capacity)
	}
	c.evictNow()
	if got := stored(c); got > c.capacity {
		t.Errorf("stored %d bytes after eviction, capacity %d: unread read-ahead was kept", got, c.capacity)
	}
}

// graceSetup fills 46 of the 40 pieces the cache holds: the open-ended reader's window 24..58
// (35 pieces, the oldest) and 11 pieces behind it, newer. Without protection the
// window's pieces are the first to go.
func graceSetup(t *testing.T) (*torrent.Torrent, *Cache, *fakeClock) {
	t.Helper()
	tor, c := newKeepCache(t, 40)
	clk := newFakeClock(c)
	for id := 24; id <= 58; id++ {
		complete(t, c, id, int64(id))
	}
	for id := 10; id <= 20; id++ {
		complete(t, c, id, 1000+int64(id))
	}
	// A priority the reader's window holds, as setLoadPriority leaves it.
	c.muPriority.Lock()
	c.localPriority[45] = torrent.PiecePriorityNormal
	tor.SetPiecePriorities(map[int]torrenttypes.PiecePriority{45: torrent.PiecePriorityNormal})
	c.muPriority.Unlock()
	return tor, c, clk
}

func windowIntact(c *Cache) bool {
	for id := 24; id <= 58; id++ {
		if atomic.LoadInt64(&c.pieces[id].Size) == 0 {
			return false
		}
	}
	return true
}

// An open-ended reader closes (a pump reconnect) and a new one attaches at the same offset
// within the grace: its window stays unevictable and keeps its priorities in between.
func TestReconnectGraceKeepsWindowAndPriorities(t *testing.T) {
	tor, c, clk := graceSetup(t)
	old := keepReader(c, tor, 50, 8)
	c.CloseReader(old)

	c.evictNow()
	if !windowIntact(c) {
		t.Fatal("a piece of the closed reader's window was evicted within the grace")
	}
	if stored(c) > c.capacity {
		t.Fatalf("setup: eviction did not free capacity (%d > %d)", stored(c), c.capacity)
	}
	c.clearPriority()
	if got := tor.PieceState(45).Priority; got != torrent.PiecePriorityNormal {
		t.Errorf("engine priority within the grace = %v, want Normal", got)
	}

	clk.advance(reconnectGrace - time.Millisecond) // still inside
	if ids := ids(evictable(c)); contains(ids, 30) {
		t.Error("window piece 30 evictable 1 ms before the grace ends")
	}

	keepReader(c, tor, 50, 8) // the reconnect
	c.evictNow()
	if !windowIntact(c) {
		t.Error("a piece of the window was evicted after the new reader attached")
	}
	c.muPriority.Lock()
	_, kept := c.localPriority[45]
	c.muPriority.Unlock()
	if !kept {
		t.Error("localPriority entry lost across the reconnect")
	}
}

// With no reader coming back, the window is evictable again once the grace has passed, and
// clearPriority drops its priorities.
func TestReconnectGraceExpires(t *testing.T) {
	tor, c, clk := graceSetup(t)
	c.CloseReader(keepReader(c, tor, 50, 8))

	clk.advance(reconnectGrace)
	if ids := ids(evictable(c)); !contains(ids, 30) {
		t.Fatalf("window piece 30 still protected after the grace: evictable = %v", ids)
	}
	c.clearPriority()
	if got := tor.PieceState(45).Priority; got != torrent.PiecePriorityNone {
		t.Errorf("engine priority after the grace = %v, want None", got)
	}
	c.evictNow()
	if windowIntact(c) {
		t.Error("no window piece was evicted after the grace, with the cache over capacity")
	}
}

// The grace list is bounded, drops expired entries when it is written, ignores bounded readers
// and is cleared by Close.
func TestReconnectGraceList(t *testing.T) {
	tor, c := newKeepCache(t, 40)
	clk := newFakeClock(c)

	b := keepReader(c, tor, 20, 4)
	b.SetBounded(22 * keepPieceLen)
	c.CloseReader(b)
	if len(c.grace) != 0 {
		t.Errorf("a bounded reader left %d grace entries", len(c.grace))
	}

	for i := 0; i < graceMax+4; i++ {
		c.CloseReader(keepReader(c, tor, int64(20+i), 4))
	}
	if len(c.grace) != graceMax {
		t.Errorf("grace list = %d entries, want the bound %d", len(c.grace), graceMax)
	}
	if c.grace[len(c.grace)-1].pos != 20+graceMax+3 {
		t.Errorf("newest entry pos = %d, want %d: the oldest should be dropped", c.grace[len(c.grace)-1].pos, 20+graceMax+3)
	}

	clk.advance(reconnectGrace)
	c.CloseReader(keepReader(c, tor, 70, 4))
	if len(c.grace) != 1 {
		t.Errorf("grace list = %d entries after expiry and one close, want 1", len(c.grace))
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if c.grace != nil {
		t.Error("Close left the grace list")
	}
}

func contains(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// Bounded readers share only what the open-ended windows leave (15% of the cache), however long
// their ranges: one open-ended reader and two bounded readers asking for the rest of the file
// must not protect more than the cache holds, or nothing is evictable.
func TestBoundedReadersShareOnlySlack(t *testing.T) {
	tor, c := newKeepCache(t, 40)
	open := keepReader(c, tor, 50, 8)
	var total int64
	b, e := open.getOffsetRange()
	total += e - b // protectedWindow is [begin, begin+protected]: exclusive end
	for _, pos := range []int64{10, 70} {
		r := keepReader(c, tor, pos, 8)
		r.SetBounded(100 * keepPieceLen) // bytes=N-<size-1>
	}
	for r := range c.readers {
		if r.bounded.Load() {
			b, e := r.getOffsetRange()
			total += e - b + 1
		}
	}
	if total > c.capacity {
		t.Errorf("protected bytes = %d (%d pieces), capacity %d (%d pieces)", total, total/keepPieceLen, c.capacity, c.capacity/keepPieceLen)
	}
	for id := 8; id <= 92; id++ {
		complete(t, c, id, int64(id))
	}
	if len(evictable(c)) == 0 {
		t.Error("nothing evictable with one open-ended and two long bounded readers")
	}
}

// A seek is also close-then-open, so the grace ends at the new reader's first Read: a read at
// a far offset frees the old window and its priorities at once, a read at the same offset
// re-protects the window itself, and a bounded reader's read (a probe) ends nothing.
func TestFirstReadEndsGrace(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	read := func(r *Reader) { _, _ = r.ReadContext(cancelled, make([]byte, 1)) }

	t.Run("far offset", func(t *testing.T) {
		tor, c, _ := graceSetup(t)
		c.CloseReader(keepReader(c, tor, 50, 8))
		if contains(ids(evictable(c)), 30) {
			t.Fatal("setup: window not protected by the grace")
		}
		read(keepReader(c, tor, 90, 2)) // the seek's new reader, far from the old window
		if !contains(ids(evictable(c)), 30) {
			t.Error("old window still protected after the new reader's first read")
		}
		deadline := time.Now().Add(2 * time.Second)
		for tor.PieceState(45).Priority != torrent.PiecePriorityNone && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if got := tor.PieceState(45).Priority; got != torrent.PiecePriorityNone {
			t.Errorf("old priority after the new reader's first read = %v, want None", got)
		}
	})
	t.Run("same offset", func(t *testing.T) {
		tor, c, _ := graceSetup(t)
		c.CloseReader(keepReader(c, tor, 50, 8))
		read(keepReader(c, tor, 50, 8))
		c.evictNow()
		if !windowIntact(c) {
			t.Error("a reconnect at the same offset lost its window")
		}
	})
	t.Run("bounded read", func(t *testing.T) {
		tor, c, _ := graceSetup(t)
		c.CloseReader(keepReader(c, tor, 50, 8))
		b := keepReader(c, tor, 90, 2)
		b.SetBounded(91 * keepPieceLen)
		read(b)
		if contains(ids(evictable(c)), 30) {
			t.Error("a bounded reader's read ended the grace")
		}
	})
}

// A parked open-ended reader holds no window, so it takes no share of the cache.
func TestParkedReaderTakesNoShare(t *testing.T) {
	tor, c := newKeepCache(t, 40)
	a := keepReader(c, tor, 50, 8)
	b := keepReader(c, tor, 20, 8)
	if x, y := a.getOffsetRange(); y-x != 17*keepPieceLen {
		t.Fatalf("setup: window %d, want 17 pieces", y-x)
	}
	b.readerOff()
	if x, y := a.getOffsetRange(); y-x != 34*keepPieceLen {
		t.Errorf("window with the other reader parked = %d pieces, want 34", (y-x)/keepPieceLen)
	}
	b.readerOn()
	if x, y := a.getOffsetRange(); y-x != 17*keepPieceLen {
		t.Errorf("window after it resumed = %d pieces, want 17", (y-x)/keepPieceLen)
	}
	c.CloseReader(b)
	if x, y := a.getOffsetRange(); y-x != 34*keepPieceLen {
		t.Errorf("window after it closed = %d pieces, want 34", (y-x)/keepPieceLen)
	}
}

// A bounded reader's engine read-ahead is cut to its remaining protected range (8 MiB floor), so
// the stats tick does not hand it the open-ended share to fetch and evict.
func TestBoundedReadaheadClampedToProtectedRange(t *testing.T) {
	tor, c := newKeepCache(t, 200) // slack = 30 pieces
	settings.BTsets.CacheSize = 200 * keepPieceLen
	open := keepReader(c, tor, 50, 8)
	b := keepReader(c, tor, 10, 8)
	b.SetBounded(90 * keepPieceLen)
	small := keepReader(c, tor, 10, 8)
	small.SetBounded(12 * keepPieceLen)
	c.AdjustRA(200 * keepPieceLen)
	if got, want := b.Readahead(), 15*keepPieceLen; got != want { // 30 pieces / 2 bounded readers
		t.Errorf("long bounded read-ahead = %d MiB, want %d", got>>20, want>>20)
	}
	if got, want := small.Readahead(), int64(minPerReaderReadahead); got != want {
		t.Errorf("short bounded read-ahead = %d MiB, want the %d MiB floor", got>>20, want>>20)
	}
	if got := open.Readahead(); got != perReaderReadahead(200*keepPieceLen, 1) {
		t.Errorf("open-ended read-ahead = %d, want the undivided share", got)
	}
}

// The cleaner's tick drops the priorities of a window whose grace passed with no read to
// trigger a clean pass, and not before.
func TestGraceTickClearsPriorities(t *testing.T) {
	tor, c, clk := graceSetup(t)
	c.CloseReader(keepReader(c, tor, 50, 8))
	c.clearIfGraceOver()
	if got := tor.PieceState(45).Priority; got != torrent.PiecePriorityNormal {
		t.Fatalf("priority dropped inside the grace: %v", got)
	}
	clk.advance(reconnectGrace)
	c.clearIfGraceOver()
	if got := tor.PieceState(45).Priority; got != torrent.PiecePriorityNone {
		t.Errorf("priority after the grace and a tick = %v, want None", got)
	}
}
