package torrstor

import (
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// forgetLog stands in for the torrent's ForgetPartialPiece: it records which pieces the cache
// asked about, and answers from refuse. The engine's own guards are tested in the engine package.
type forgetLog struct {
	mu     sync.Mutex
	asked  map[int]int
	refuse func(id int) bool
	during func(id int) // runs inside the call, where a chunk can land mid-forget
}

func newForgetLog() *forgetLog { return &forgetLog{asked: map[int]int{}} }

func (f *forgetLog) forget(id int) bool {
	f.mu.Lock()
	f.asked[id]++
	refuse, during := f.refuse, f.during
	f.mu.Unlock()
	if during != nil {
		during(id)
	}
	return refuse == nil || !refuse(id)
}

func (f *forgetLog) askedAbout(id int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked[id]
}

const old = int64(1) // an access time long past the stranded age

// quiet keeps the background cleaner, which every write wakes, from sweeping before the test does:
// it would free pieces mid-setup. sweep lifts the hold.
func quiet(c *Cache) { c.lastStrandedNS.Store(math.MaxInt64) }

// sweep runs the stranded-partial pass as the cleaner would at some later time, past the throttle.
func sweep(c *Cache) {
	c.lastStrandedNS.Store(0)
	c.dropStrandedPartials(time.Now())
}

func hasBuffer(c *Cache, id int) bool {
	mp := c.pieces[id].mPiece
	mp.mu.RLock()
	defer mp.mu.RUnlock()
	return mp.buffer != nil
}

// Many seeks leave many half-written pieces behind (the gauge climbs); after the clean pass the
// gauge holds only what lies inside the reader's window.
func TestDropStrandedPartialsAfterManySeeks(t *testing.T) {
	const mib = int64(1 << 20)
	tor, c := newMetricsTestCache(t)
	fl := newForgetLog()
	c.forgetPartial = fl.forget
	quiet(c)
	r := c.NewReader(tor.Files()[0])
	t.Cleanup(func() { c.CloseReader(r) })

	// 12 seeks, each abandoning a half-written piece; the last seek is to piece 30.
	var left []int
	for i := 0; i < 12; i++ {
		id := 8 + 2*i // 8, 10, ... 30
		if _, err := r.Seek(int64(id)*mib, 0); err != nil {
			t.Fatal(err)
		}
		fetch(t, c, id, int(mib/2), old)
		left = append(left, id)
	}
	// Seek is the last call: the window is around piece 30 (about 28..32).
	inWindow := map[int]bool{}
	for _, id := range left {
		if inRanges(c.windowsLockedForTest(), id) {
			inWindow[id] = true
		}
	}
	if !inWindow[30] || len(inWindow) >= len(left) {
		t.Fatalf("setup: window holds %v of %v, want piece 30 and not all", inWindow, left)
	}
	if got, want := c.strandedPartialBytes(), int64(len(left)-len(inWindow))*(mib/2); got != want {
		t.Fatalf("stranded before the pass = %d, want %d", got, want)
	}

	sweep(c)

	if got := c.strandedPartialBytes(); got != 0 {
		t.Errorf("stranded after the pass = %d, want 0", got)
	}
	for _, id := range left {
		if inWindow[id] {
			if fl.askedAbout(id) != 0 {
				t.Errorf("piece %d is inside the reader's window and was offered for forgetting", id)
			}
			if !hasBuffer(c, id) || sizeOf(c, id) != mib/2 {
				t.Errorf("piece %d is inside the reader's window and lost its bytes", id)
			}
			continue
		}
		if fl.askedAbout(id) != 1 || hasBuffer(c, id) || sizeOf(c, id) != 0 {
			t.Errorf("piece %d outside the window: asked %d times, buffer held %v, size %d; want 1, false, 0",
				id, fl.askedAbout(id), hasBuffer(c, id), sizeOf(c, id))
		}
	}
}

func sizeOf(c *Cache, id int) int64 { return atomic.LoadInt64(&c.pieces[id].Size) }

// windowsLockedForTest is the window the sweep sees.
func (c *Cache) windowsLockedForTest() []Range {
	c.muReaders.RLock()
	defer c.muReaders.RUnlock()
	return c.windowsLocked()
}

// A partial piece the pass must leave alone: complete, touched recently, or one the torrent
// refuses to forget (a request is in flight, the piece is hashing, ...).
func TestDropStrandedPartialsLeavesWhatItShould(t *testing.T) {
	const mib = int64(1 << 20)
	tor, c := newMetricsTestCache(t)
	fl := newForgetLog()
	fl.refuse = func(id int) bool { return id == 14 }
	c.forgetPartial = fl.forget
	quiet(c)
	r := c.NewReader(tor.Files()[0])
	t.Cleanup(func() { c.CloseReader(r) })
	if _, err := r.Seek(30*mib, 0); err != nil {
		t.Fatal(err)
	}

	fetch(t, c, 10, int(mib), old) // complete
	if err := c.pieces[10].MarkComplete(); err != nil {
		t.Fatal(err)
	}
	fetch(t, c, 12, int(mib/2), time.Now().Unix()) // touched just now
	fetch(t, c, 14, int(mib/2), old)               // the torrent refuses
	fetch(t, c, 16, int(mib/2), old)               // the control: forgotten

	sweep(c)

	for _, id := range []int{10, 12, 14} {
		if !hasBuffer(c, id) {
			t.Errorf("piece %d lost its bytes", id)
		}
	}
	for _, id := range []int{10, 12} {
		if fl.askedAbout(id) != 0 {
			t.Errorf("piece %d was offered for forgetting", id)
		}
	}
	if fl.askedAbout(14) != 1 {
		t.Errorf("piece 14 asked %d times, want 1", fl.askedAbout(14))
	}
	if hasBuffer(c, 16) {
		t.Error("the control piece kept its bytes")
	}
	if !c.pieces[10].Complete.Load() {
		t.Error("the complete piece lost its mark")
	}
}

// A chunk that lands while the torrent is forgetting is dirty again in the torrent, so the cache
// keeps the piece's bytes, the new chunk included.
func TestDropStrandedPartialsKeepsAChunkLandingMidForget(t *testing.T) {
	const mib = int64(1 << 20)
	tor, c := newMetricsTestCache(t)
	fl := newForgetLog()
	chunk := []byte("landed during the forget")
	fl.during = func(id int) {
		if _, err := c.pieces[id].WriteAt(chunk, mib/2); err != nil {
			t.Error(err)
		}
	}
	c.forgetPartial = fl.forget
	quiet(c)
	r := c.NewReader(tor.Files()[0])
	t.Cleanup(func() { c.CloseReader(r) })
	if _, err := r.Seek(30*mib, 0); err != nil {
		t.Fatal(err)
	}
	fetch(t, c, 12, int(mib/2), old)

	sweep(c)

	if fl.askedAbout(12) != 1 {
		t.Fatalf("setup: piece 12 asked %d times", fl.askedAbout(12))
	}
	got := make([]byte, len(chunk))
	if n, err := c.pieces[12].ReadAt(got, mib/2); err != nil || string(got[:n]) != string(chunk) {
		t.Errorf("chunk written mid-forget read back as %q, err %v; want %q", got[:n], err, chunk)
	}
	if sizeOf(c, 12) == 0 {
		t.Error("the piece's size was reset under the chunk")
	}
}

// Writers, readers and the pass race on one piece: under -race this finds an unlocked access, and
// a chunk written after the last pass is always there.
func TestDropStrandedPartialsRacesWithWrites(t *testing.T) {
	const mib = int64(1 << 20)
	tor, c := newMetricsTestCache(t)
	fl := newForgetLog()
	c.forgetPartial = fl.forget
	quiet(c)
	r := c.NewReader(tor.Files()[0])
	t.Cleanup(func() { c.CloseReader(r) })
	if _, err := r.Seek(30*mib, 0); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		b := make([]byte, 4096)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = c.pieces[12].WriteAt(b, int64(i%8)*4096)
			atomic.StoreInt64(&c.pieces[12].Accessed, old)
		}
	}()
	go func() {
		defer wg.Done()
		b := make([]byte, 4096)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = c.pieces[12].ReadAt(b, 0)
		}
	}()
	for i := 0; i < 200; i++ {
		sweep(c)
	}
	close(stop)
	wg.Wait()

	final := []byte("after the last pass")
	if _, err := c.pieces[12].WriteAt(final, 0); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(final))
	if n, _ := c.pieces[12].ReadAt(got, 0); string(got[:n]) != string(final) {
		t.Errorf("a chunk written after the last pass read back as %q", got[:n])
	}
}

// The pass runs on the clean pass, at most every 5 seconds.
func TestDropStrandedPartialsIsThrottledAndWiredIntoCleanPieces(t *testing.T) {
	const mib = int64(1 << 20)
	tor, c := newMetricsTestCache(t)
	fl := newForgetLog()
	c.forgetPartial = fl.forget
	quiet(c)
	r := c.NewReader(tor.Files()[0])
	t.Cleanup(func() { c.CloseReader(r) })
	if _, err := r.Seek(30*mib, 0); err != nil {
		t.Fatal(err)
	}

	// The cleaner the writes below wake may take the first sweep; start from a known state.
	fetch(t, c, 10, int(mib/2), old)
	c.lastStrandedNS.Store(0)
	c.cleanPieces()
	if got := c.strandedPartialBytes(); got != 0 {
		t.Fatalf("cleanPieces left %d stranded bytes", got)
	}

	// A second piece within the interval is left for the next sweep, and taken once it passes.
	fetch(t, c, 12, int(mib/2), old)
	now := time.Now()
	c.lastStrandedNS.Store(now.UnixNano())
	c.dropStrandedPartials(now.Add(strandedSweepEvery - time.Second))
	if got := c.strandedPartialBytes(); got != mib/2 {
		t.Errorf("a sweep inside the interval freed bytes: stranded = %d, want %d", got, mib/2)
	}
	c.dropStrandedPartials(now.Add(strandedSweepEvery))
	if got := c.strandedPartialBytes(); got != 0 {
		t.Errorf("the sweep after the interval left %d stranded bytes", got)
	}
}

// The real path, no seam: a failed hash pends every chunk in the engine but leaves the bytes in
// the cache, and the reader then seeks away. The torrent forgets the piece (nothing to pend), the
// cache frees the buffer, and the gauge reaches zero.
func TestDropStrandedPartialsFreesAFailedHashPieceThroughTheRealTorrent(t *testing.T) {
	const mib = int64(1 << 20)
	tor, c := newMetricsTestCache(t)
	quiet(c)
	r := c.NewReader(tor.Files()[0])
	t.Cleanup(func() { c.CloseReader(r) })
	if _, err := r.Seek(30*mib, 0); err != nil {
		t.Fatal(err)
	}
	fetch(t, c, 12, int(mib/2), old)
	if got := c.strandedPartialBytes(); got != mib/2 {
		t.Fatalf("setup: stranded = %d, want %d", got, mib/2)
	}
	sweep(c)
	if hasBuffer(c, 12) {
		t.Error("the buffer of an abandoned piece the engine holds no chunk of was kept")
	}
	if got := c.strandedPartialBytes(); got != 0 {
		t.Errorf("stranded after the pass = %d, want 0", got)
	}
}

// A seek followed by silence still gets swept: nothing reads, writes or closes, so only the
// cleaner's own timer can run the pass.
func TestStrandedPartialsAreSweptWhileIdle(t *testing.T) {
	const mib = int64(1 << 20)
	prev := strandedTickEvery
	strandedTickEvery = 10 * time.Millisecond
	t.Cleanup(func() { strandedTickEvery = prev })
	tor, c := newMetricsTestCache(t)
	fl := newForgetLog()
	c.forgetPartial = fl.forget
	quiet(c)
	r := c.NewReader(tor.Files()[0])
	t.Cleanup(func() { c.CloseReader(r) })
	if _, err := r.Seek(30*mib, 0); err != nil {
		t.Fatal(err)
	}
	fetch(t, c, 12, int(mib/2), 1)
	// Let the cleaner the writes woke finish (the hold keeps it from sweeping), then let the next
	// pass through. No further call into the cache follows.
	time.Sleep(200 * time.Millisecond)
	c.lastStrandedNS.Store(time.Now().Add(-strandedSweepEvery).UnixNano())
	deadline := time.Now().Add(3 * time.Second)
	for c.strandedPartialBytes() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("stranded = %d after the seek went quiet, want 0", c.strandedPartialBytes())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The sweep runs after eviction on the cleaner's goroutine: a forget that waits on the client lock
// must not hold up eviction.
func TestCleanPiecesEvictsBeforeSweeping(t *testing.T) {
	const mib = int64(1 << 20)
	tor, c := newMetricsTestCache(t)
	entered, release := make(chan int, 8), make(chan struct{})
	c.forgetPartial = func(id int) bool {
		entered <- id
		<-release
		return true
	}
	quiet(c)
	r := c.NewReader(tor.Files()[0])
	t.Cleanup(func() { c.CloseReader(r) })
	if _, err := r.Seek(30*mib, 0); err != nil {
		t.Fatal(err)
	}
	// Eight complete pieces in a 6 MiB cache, written without MarkComplete's own eviction.
	for id := 10; id < 18; id++ {
		fetch(t, c, id, int(mib), int64(id))
		c.pieces[id].Complete.Store(true)
	}
	fetch(t, c, 20, int(mib/2), old)
	c.lastStrandedNS.Store(0)
	c.lastCleanNS.Store(0)
	done := make(chan struct{})
	go func() { c.cleanPieces(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-done
	})
	select {
	case id := <-entered:
		if id != 20 {
			t.Fatalf("forget asked about piece %d, want 20", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the sweep never ran")
	}
	// The sweep is blocked inside the forget: eviction must already be done.
	var kept int
	for id := 10; id < 18; id++ {
		if sizeOf(c, id) > 0 {
			kept++
		}
	}
	if int64(kept)*mib > c.capacity {
		t.Errorf("%d complete pieces still held while the sweep waits, capacity holds %d", kept, c.capacity/mib)
	}
}
