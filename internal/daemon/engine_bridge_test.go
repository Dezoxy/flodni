package daemon

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"flodni/internal/gostorm/native"
	"flodni/internal/ratelimit"
	"flodni/internal/warmup"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// A fake engine behind the engineBridge interface drives the read path's fetch,
// singleflight, prefetch, warmup and pump branches without a torrent. Like the other
// read-path tests it swaps package globals and restores them, so none runs in parallel.

const testHash = "0123456789abcdef0123456789abcdef01234567"

// fillPattern writes the file's synthetic content for [off, off+len(p)): byte i is i%251.
func fillPattern(p []byte, off int64) {
	for i := range p {
		p[i] = byte((off + int64(i)) % 251)
	}
}

func wantPattern(n int, off int64) []byte {
	b := make([]byte, n)
	fillPattern(b, off)
	return b
}

type blockCall struct {
	hash   string
	fileID int
	off    int64
	n      int
}

type aheadCall struct {
	hash            string
	fileID          int
	off             int64
	bufLen, destLen int
}

// fakeEngine is a scriptable engineBridge. With no script a block or ahead fetch returns
// the synthetic pattern; a script replaces that for call number i (0-based).
type fakeEngine struct {
	mu      sync.Mutex
	blocks  []blockCall
	aheads  []aheadCall
	readers []*fakeReader
	woke    []string
	wakeFn  func(ctx context.Context, magnet string, fileIdx int) error // scripts Wake's answer; nil = success

	blockFn func(i int, off int64, p []byte) (int, error)
	aheadFn func(i int, off int64, buf, dest []byte, onFill func(int, bool, error)) (int, error)
	newRead func() *fakeReader

	torrents  []native.TorrentStats
	hashesCut int
}

func (f *fakeEngine) FetchBlock(hash string, fileID int, off int64, p []byte) (int, error) {
	f.mu.Lock()
	i := len(f.blocks)
	f.blocks = append(f.blocks, blockCall{hash, fileID, off, len(p)})
	fn := f.blockFn
	f.mu.Unlock()
	if fn != nil {
		return fn(i, off, p)
	}
	fillPattern(p, off)
	return len(p), nil
}

func (f *fakeEngine) FetchAhead(hash string, fileID int, off int64, buf, dest []byte, onFill func(int, bool, error)) (int, error) {
	f.mu.Lock()
	i := len(f.aheads)
	f.aheads = append(f.aheads, aheadCall{hash, fileID, off, len(buf), len(dest)})
	fn := f.aheadFn
	f.mu.Unlock()
	if fn != nil {
		return fn(i, off, buf, dest, onFill)
	}
	return fillAhead(off, buf, dest, onFill)
}

// fillAhead is the default FetchAhead: the whole window arrives at once.
func fillAhead(off int64, buf, dest []byte, onFill func(int, bool, error)) (int, error) {
	fillPattern(buf, off)
	copy(dest, buf)
	onFill(len(buf), true, nil)
	return len(dest), nil
}

func (f *fakeEngine) NewStreamReader(string, int, int64) streamReader {
	r := &fakeReader{}
	if f.newRead != nil {
		r = f.newRead()
	}
	f.mu.Lock()
	f.readers = append(f.readers, r)
	f.mu.Unlock()
	return r
}

func (f *fakeEngine) Wake(ctx context.Context, magnet string, fileIdx int) error {
	f.mu.Lock()
	f.woke = append(f.woke, magnet)
	fn := f.wakeFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, magnet, fileIdx)
	}
	return nil
}

func (f *fakeEngine) wakeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.woke)
}

func (f *fakeEngine) ListTorrents() ([]native.TorrentStats, error) { return f.torrents, nil }

func (f *fakeEngine) CleanupHashes() int { return f.hashesCut }

func (f *fakeEngine) blockCalls() []blockCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]blockCall(nil), f.blocks...)
}

func (f *fakeEngine) aheadCalls() []aheadCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]aheadCall(nil), f.aheads...)
}

func (f *fakeEngine) readerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.readers)
}

type readCall struct {
	off int64
	n   int
}

// fakeReader is a scriptable streamReader; unscripted it serves the synthetic pattern.
type fakeReader struct {
	mu         sync.Mutex
	reads      []readCall
	readFn     func(i int, p []byte, off int64) (int, error)
	interrupts atomic.Int32
	closes     atomic.Int32
	pieceLen   atomic.Int64
}

func (r *fakeReader) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	i := len(r.reads)
	r.reads = append(r.reads, readCall{off, len(p)})
	fn := r.readFn
	r.mu.Unlock()
	if fn != nil {
		return fn(i, p, off)
	}
	fillPattern(p, off)
	return len(p), nil
}

func (r *fakeReader) Close() error        { r.closes.Add(1); return nil }
func (r *fakeReader) Interrupt()          { r.interrupts.Add(1) }
func (r *fakeReader) SetPieceLen(n int64) { r.pieceLen.Store(n) }
func (r *fakeReader) readCalls() []readCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]readCall(nil), r.reads...)
}

// useEngineReadPath is useFreshReadPath with a fake engine, sizes small enough for a fast
// test (4 MiB chunks, 8 MiB first window, 64 MiB budget), a pump slot pool, a rate
// limiter and a cleanup manager, all restored on cleanup.
func useEngineReadPath(t *testing.T) *fakeEngine {
	newTestApp(t)
	t.Helper()
	useFreshReadPath(t)
	fe := &fakeEngine{}
	nativeBridge = fe

	cfg := *gc()
	cfg.ReadAheadBase, cfg.ReadAheadInitial, cfg.ReadAheadBudget = 4*mb, 8*mb, 64*mb
	cfg.MasterConcurrencyLimit = 10
	globalConfig.Store(&cfg) // useTestGlobals restores the previous config

	prevPool, prevSem, prevCM, prevRL := readBufferPool, masterDataSemaphore, globalCleanupManager, globalRateLimiter
	readBufferPool = &sync.Pool{New: func() any { b := make([]byte, 8*mb); return &b }}
	masterDataSemaphore = make(chan struct{}, 10)
	globalCleanupManager = NewCleanupManager(logger, nil, fe)
	globalRateLimiter = ratelimit.NewRateLimiter(1000, time.Minute)
	t.Cleanup(func() {
		globalRateLimiter.Stop()
		readBufferPool, masterDataSemaphore, globalCleanupManager, globalRateLimiter = prevPool, prevSem, prevCM, prevRL
	})
	// Registered last, so it runs first: background prefetches read the globals restored above.
	t.Cleanup(func() {
		waitUntil(t, "background prefetches to end", func() bool {
			idle := true
			app.inFlightPrefetches.Range(func(_, _ any) bool { idle = false; return false })
			return idle
		})
	})
	return fe
}

// engineHandle is a readHandle bound to the test torrent.
func engineHandle(path string, size int64) *MkvHandle {
	h := readHandle(path, size)
	h.hash, h.fileID = testHash, 3
	return h
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// cachedAcross reports whether every 4 MiB chunk in [start, end] is cached: Covered only
// looks across one chunk boundary.
func cachedAcross(path string, start, end int64) bool {
	for o := start; o <= end; o = (o/(4*mb) + 1) * 4 * mb {
		if !raCache.Covered(path, o, min(end, (o/(4*mb)+1)*4*mb-1)) {
			return false
		}
	}
	return true
}

func flightFree(path string, chunkStart int64) func() bool {
	return func() bool {
		_, busy := app.inFlightPrefetches.Load(fmt.Sprintf("%s:%d", path, chunkStart))
		return !busy
	}
}

func readBytes(t *testing.T, h *MkvHandle, size int, off int64) []byte {
	t.Helper()
	res, errno := h.Read(context.Background(), make([]byte, size), off)
	if errno != 0 || res == nil {
		t.Fatalf("Read(%d, %d) = (%v, %v)", size, off, res, errno)
	}
	got, _ := res.Bytes(nil)
	return got
}

// --- nil semantics ---

func TestNativeBridgeIsNilWhenNoEngineIsConfigured(t *testing.T) {
	useFreshReadPath(t)
	if nativeBridge != nil {
		t.Fatal("with no engine configured nativeBridge must be a nil interface")
	}
	if _, _, err := resolveTargetFile("x?link="+testHash+"&index=2", 1, "/p.mkv"); err == nil || err.Error() != "nativeBridge is nil" {
		t.Errorf("resolveTargetFile took the wrong branch: %v", err)
	}
	// A handle that cannot resolve a hash stays hashless and fails the read, never a nil call.
	h := readHandle("/test/nobridge.mkv", 10*mb)
	h.url = "x?link=" + testHash + "&index=2"
	if res, errno := h.Read(context.Background(), make([]byte, 256*1024), 5*mb); res != nil || errno != syscall.EIO {
		t.Errorf("got (%v, %v), want (nil, EIO)", res, errno)
	}
	if h.hash != "" {
		t.Error("a hash was recovered without an engine")
	}
	if cm := NewCleanupManager(logger, nil, nativeBridge); cm.nativeBridge != nil {
		t.Error("the cleanup manager must see a nil bridge as nil, so it skips CleanupHashes")
	}
}

// The trap the startup assignment avoids: a nil client wrapped in the adapter is a
// non-nil interface, and every nativeBridge == nil guard would pass it.
func TestAWrappedNilClientIsNotANilBridge(t *testing.T) {
	var c *native.NativeClient
	var b engineBridge = nativeEngine{c}
	if b == nil {
		t.Fatal("expected a non-nil interface around a nil client")
	}
	if got := (nativeEngine{native.NewNativeClient()}).NewStreamReader(testHash, 1, 10); got == nil {
		t.Error("the adapter must hand the pump a usable reader")
	}
}

// --- fetch path ---

func TestReadInnerMissFetchesTheWindowAndCachesIt(t *testing.T) {
	fe := useEngineReadPath(t)
	const path = "/test/miss-fetch.mkv"
	h := engineHandle(path, 100*mb)
	off := int64(50 * mb) // 2 MiB from the next chunk: no predictive prefetch

	got := readBytes(t, h, 256*1024, off)
	if !bytes.Equal(got, wantPattern(256*1024, off)) {
		t.Error("the read returned bytes that differ from the engine's")
	}
	calls := fe.aheadCalls()
	if len(calls) != 1 || calls[0] != (aheadCall{testHash, 3, off, int(8 * mb), 256 * 1024}) {
		t.Fatalf("FetchAhead calls = %+v, want one for the first-block window (8 MiB buf, 256 KiB dest)", calls)
	}
	if !raCache.Covered(path, off, off+8*mb-1) {
		t.Error("the whole fetched window must be in raCache")
	}
	if _, busy := app.inFlightFetches.Load(fmt.Sprintf("%s:%d", path, 48*mb)); busy {
		t.Error("the flight must be released once the fill is done")
	}
	if len(fe.blockCalls()) != 0 {
		t.Error("a plain miss must not use FetchBlock")
	}
}

func TestReadInnerWindowSizeFollowsFirstBlockAndEOF(t *testing.T) {
	fe := useEngineReadPath(t)
	h := engineHandle("/test/window.mkv", 100*mb)
	h.lastActivityTime = time.Now() // not a first block: ReadAheadBase, not ReadAheadInitial
	readBytes(t, h, 256*1024, 50*mb)

	tail := engineHandle("/test/window-eof.mkv", 50*mb+300*1024)
	tail.lastActivityTime = time.Now()
	readBytes(t, tail, 256*1024, 50*mb)

	calls := fe.aheadCalls()
	if len(calls) != 2 || calls[0].bufLen != int(4*mb) || calls[1].bufLen != 300*1024 {
		t.Fatalf("FetchAhead calls = %+v, want a 4 MiB window then one clamped to the 300 KiB left", calls)
	}
}

func TestReadInnerRetriesFetchAheadThenSucceeds(t *testing.T) {
	fe := useEngineReadPath(t)
	failures := []error{errors.New("no peers"), nil} // an error, then a silent empty fetch
	fe.aheadFn = func(i int, off int64, buf, dest []byte, onFill func(int, bool, error)) (int, error) {
		if i < len(failures) {
			onFill(0, true, failures[i]) // the engine reports done even when the head fails
			return 0, failures[i]
		}
		return fillAhead(off, buf, dest, onFill)
	}
	h := engineHandle("/test/retry.mkv", 100*mb)

	start := time.Now()
	got := readBytes(t, h, 256*1024, 50*mb)
	if !bytes.Equal(got, wantPattern(256*1024, 50*mb)) {
		t.Error("the third attempt's bytes must be returned")
	}
	if n := len(fe.aheadCalls()); n != 3 {
		t.Errorf("FetchAhead calls = %d, want 3", n)
	}
	if d := time.Since(start); d < 900*time.Millisecond {
		t.Errorf("took %v: two 500ms back-offs expected between the attempts", d)
	}
}

func TestReadInnerGivesUpWithEIOAfterThreeFetchAheadAttempts(t *testing.T) {
	fe := useEngineReadPath(t)
	fe.aheadFn = func(_ int, _ int64, _, _ []byte, onFill func(int, bool, error)) (int, error) {
		err := errors.New("stalled")
		onFill(0, true, err)
		return 0, err
	}
	const path = "/test/retry-exhausted.mkv"
	h := engineHandle(path, 100*mb)

	res, errno := h.Read(context.Background(), make([]byte, 256*1024), 50*mb)
	if res != nil || errno != syscall.EIO {
		t.Fatalf("got (%v, %v), want (nil, EIO)", res, errno)
	}
	if n := len(fe.aheadCalls()); n != 3 {
		t.Errorf("FetchAhead calls = %d, want exactly 3 (bounded)", n)
	}
	if _, busy := app.inFlightFetches.Load(fmt.Sprintf("%s:%d", path, 48*mb)); busy {
		t.Error("a failed fetch must release its flight so the next read can lead")
	}
}

func TestReadInnerStopsRetryingWhenTheContextEnds(t *testing.T) {
	fe := useEngineReadPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	fe.aheadFn = func(_ int, _ int64, _, _ []byte, onFill func(int, bool, error)) (int, error) {
		cancel() // the kernel gave up during the fetch
		onFill(0, true, nil)
		return 0, errors.New("interrupted")
	}
	h := engineHandle("/test/retry-cancel.mkv", 100*mb)

	res, errno := h.Read(ctx, make([]byte, 256*1024), 50*mb)
	if res != nil || errno != syscall.EINTR {
		t.Fatalf("got (%v, %v), want (nil, EINTR)", res, errno)
	}
	if n := len(fe.aheadCalls()); n != 1 {
		t.Errorf("FetchAhead calls = %d, want 1: no retry after cancellation", n)
	}
}

func TestReadInnerLateHashRecoveryResolvesFromTheURL(t *testing.T) {
	fe := useEngineReadPath(t)
	h := readHandle("/test/late-hash.mkv", 100*mb)
	h.url = "x?link=" + testHash + "&index=2"

	readBytes(t, h, 256*1024, 50*mb)
	h.pumpOnce.Do(func() {}) // waits for the pump start the recovery spawned
	calls := fe.aheadCalls()
	if h.hash != testHash || h.fileID != 2 {
		t.Errorf("recovered (%q, %d), want (%q, 2)", h.hash, h.fileID, testHash)
	}
	if len(calls) != 1 || calls[0].hash != testHash || calls[0].fileID != 2 {
		t.Errorf("FetchAhead calls = %+v, want one for the recovered hash and file", calls)
	}
}

func TestReadInnerLateHashRecoveryFailureFailsTheRead(t *testing.T) {
	fe := useEngineReadPath(t)
	h := readHandle("/test/late-hash-fail.mkv", 100*mb)
	h.url = "no-link-here"

	if res, errno := h.Read(context.Background(), make([]byte, 256*1024), 50*mb); res != nil || errno != syscall.EIO {
		t.Fatalf("got (%v, %v), want (nil, EIO)", res, errno)
	}
	if len(fe.aheadCalls())+len(fe.blockCalls()) != 0 {
		t.Error("a handle without a hash must not reach the engine")
	}
}

func TestSecondReadOfTheSameChunkJoinsTheLeadersFetch(t *testing.T) {
	fe := useEngineReadPath(t)
	entered, release := make(chan struct{}), make(chan struct{})
	fe.aheadFn = func(_ int, off int64, buf, dest []byte, onFill func(int, bool, error)) (int, error) {
		close(entered)
		<-release
		return fillAhead(off, buf, dest, onFill)
	}
	const path = "/test/singleflight.mkv"
	leader, follower := engineHandle(path, 100*mb), engineHandle(path, 100*mb)
	dedup0 := fetchFlightDedupCount.Load()

	type result struct {
		got []byte
		off int64
	}
	results := make(chan result, 2)
	go func() { results <- result{readBytes(t, leader, 256*1024, 50*mb), 50 * mb} }()
	<-entered // the leader owns the flight and is inside FetchAhead
	go func() { results <- result{readBytes(t, follower, 256*1024, 50*mb+512*1024), 50*mb + 512*1024} }()
	time.Sleep(100 * time.Millisecond) // the follower is polling the cache by now
	close(release)

	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			if !bytes.Equal(r.got, wantPattern(256*1024, r.off)) {
				t.Errorf("read at %d returned the wrong bytes", r.off)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("a read never returned")
		}
	}
	if n := len(fe.aheadCalls()); n != 1 {
		t.Errorf("FetchAhead calls = %d, want 1: the follower must not fetch", n)
	}
	if d := fetchFlightDedupCount.Load() - dedup0; d != 1 {
		t.Errorf("fetchFlightDedupCount delta = %d, want 1", d)
	}
}

func TestFollowerFetchesItselfWhenTheLeaderFailsWithoutCoveringIt(t *testing.T) {
	fe := useEngineReadPath(t)
	entered, release := make(chan struct{}), make(chan struct{})
	fe.aheadFn = func(i int, off int64, buf, dest []byte, onFill func(int, bool, error)) (int, error) {
		if i == 0 {
			close(entered)
			<-release
			onFill(0, true, nil) // done, nothing cached
			return 0, errors.New("leader failed")
		}
		return fillAhead(off, buf, dest, onFill)
	}
	const path = "/test/singleflight-fail.mkv"
	leader, follower := engineHandle(path, 100*mb), engineHandle(path, 100*mb)

	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		h := leader
		h.Read(context.Background(), make([]byte, 256*1024), 50*mb) // fails after its retries
	}()
	<-entered
	followerGot := make(chan []byte, 1)
	go func() { followerGot <- readBytes(t, follower, 256*1024, 50*mb+512*1024) }()
	time.Sleep(100 * time.Millisecond)
	close(release)

	select {
	case got := <-followerGot:
		if !bytes.Equal(got, wantPattern(256*1024, 50*mb+512*1024)) {
			t.Error("the follower returned the wrong bytes")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the follower never returned")
	}
	<-leaderDone
	if n := len(fe.aheadCalls()); n < 2 {
		t.Errorf("FetchAhead calls = %d, want the follower to fetch after the leader's failure", n)
	}
}

// --- FetchBlock prefetch ---

func TestReadInnerPrefetchesTheNextChunkNearTheBoundary(t *testing.T) {
	fe := useEngineReadPath(t)
	const path = "/test/prefetch.mkv"
	h := engineHandle(path, 100*mb)
	raCache.Put(path, 48*mb, 52*mb-1, wantPattern(int(4*mb), 48*mb))
	off := int64(51*mb + 512*1024) // last quarter of the chunk

	got := readBytes(t, h, 256*1024, off)
	if !bytes.Equal(got, wantPattern(256*1024, off)) {
		t.Error("the cached read returned the wrong bytes")
	}
	waitUntil(t, "the prefetch", flightFree(path, 52*mb))
	calls := fe.blockCalls()
	if len(calls) != 1 || calls[0] != (blockCall{testHash, 3, 52 * mb, int(4 * mb)}) {
		t.Fatalf("FetchBlock calls = %+v, want one for the next 4 MiB chunk", calls)
	}
	if !raCache.Covered(path, 52*mb, 56*mb-1) {
		t.Error("the prefetched chunk must land in raCache")
	}
	if len(fe.aheadCalls()) != 0 {
		t.Error("a cache hit must not call FetchAhead")
	}
}

func TestReadInnerPrefetchErrorCachesNothing(t *testing.T) {
	fe := useEngineReadPath(t)
	fe.blockFn = func(int, int64, []byte) (int, error) { return 0, errors.New("timeout") }
	const path = "/test/prefetch-err.mkv"
	h := engineHandle(path, 100*mb)
	raCache.Put(path, 48*mb, 52*mb-1, wantPattern(int(4*mb), 48*mb))

	readBytes(t, h, 256*1024, 51*mb+512*1024)
	waitUntil(t, "the prefetch attempt", flightFree(path, 52*mb))
	if len(fe.blockCalls()) != 1 {
		t.Fatalf("FetchBlock calls = %d, want 1", len(fe.blockCalls()))
	}
	if raCache.Exists(path, 52*mb) {
		t.Error("a failed prefetch must not cache anything")
	}
}

func TestReadInnerPrefetchesAfterAFetchWhenThePumpIsAbsent(t *testing.T) {
	fe := useEngineReadPath(t)
	const path = "/test/post-fetch-prefetch.mkv"
	h := engineHandle(path, 100*mb)
	h.hasSlot.Store(false) // a probe without a pump: reads on a temporary slot
	h.lastActivityTime = time.Now()
	off := int64(51*mb + 512*1024) // 512 KiB from the boundary, nothing cached ahead

	readBytes(t, h, 256*1024, off)
	waitUntil(t, "the post-fetch prefetch", func() bool { return len(fe.blockCalls()) == 1 && flightFree(path, 52*mb)() })
	if c := fe.blockCalls()[0]; c.off != 52*mb || c.n != int(4*mb) {
		t.Errorf("prefetch = %+v, want the next 4 MiB chunk at 52 MiB", c)
	}
}

func TestNewHandleReadingPastTheHeadWarmsTheLandingChunk(t *testing.T) {
	fe := useEngineReadPath(t)
	const path = "/test/warm-landing.mkv"
	h := engineHandle(path, 100*mb) // lastOff -1: a new handle resuming deep in the file
	off := int64(96*mb + 256*1024)

	got := readBytes(t, h, 256*1024, off)
	if !bytes.Equal(got, wantPattern(256*1024, off)) {
		t.Error("wrong bytes")
	}
	waitUntil(t, "the landing fetch", func() bool { return len(fe.blockCalls()) >= 1 && flightFree(path, 96*mb)() })
	if c := fe.blockCalls()[0]; c != (blockCall{testHash, 3, 96 * mb, int(4 * mb)}) {
		t.Errorf("landing fetch = %+v, want the chunk at 96 MiB", c)
	}
	if h.state.Load() != stateStreaming {
		t.Error("a first read past the head must switch the handle to STREAMING")
	}
}

func TestSeekInterruptsThePumpReaderAndWarmsTheTarget(t *testing.T) {
	cases := []struct {
		name        string
		pendingFlag bool
		wantInts    int32
		wantLanding bool
	}{
		{"first handle to see the seek fires", false, 1, true},
		{"cascade guard: a pending interrupt is not repeated", true, 0, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fe := useEngineReadPath(t)
			path := fmt.Sprintf("/test/seek-%d.mkv", i)
			h := engineHandle(path, 200*mb)
			rd := &fakeReader{}
			h.nativeReader = rd
			h.lastOff = 10 * mb
			ps := &NativePumpState{}
			ps.interruptPending.Store(c.pendingFlag)
			app.activePumps.Store(path, ps)
			t.Cleanup(func() { app.activePumps.Delete(path) })
			off := int64(10*mb + 64*mb + mb) // one MiB past the budget

			readBytes(t, h, 256*1024, off)
			if got := rd.interrupts.Load(); got != c.wantInts {
				t.Errorf("Interrupt calls = %d, want %d", got, c.wantInts)
			}
			if c.wantLanding {
				waitUntil(t, "the landing fetch", func() bool { return len(fe.blockCalls()) == 1 && flightFree(path, 72*mb)() })
				if b := fe.blockCalls()[0]; b.off != 72*mb || b.n != int(4*mb) {
					t.Errorf("landing fetch = %+v, want the chunk at 72 MiB", b)
				}
			}
			if h.state.Load() != stateStreaming && c.wantInts == 1 {
				t.Error("a seek must switch the handle to STREAMING")
			}
		})
	}
}

// --- SSD warmup ---

var (
	warmOnce   sync.Once
	warmDir    string
	sharedDisk *warmup.DiskWarmupCache
	warmSeq    atomic.Int64
)

func TestMain(m *testing.M) {
	app = newApp() // fallback for tests that reach the read path without newTestApp
	code := m.Run()
	if warmDir != "" {
		_ = os.RemoveAll(warmDir)
	}
	os.Exit(code)
}

// useDiskWarmup installs an SSD warmup cache and returns a hash no other test uses. The
// cache is started once per process, with the default window sizes: its writer goroutine
// reads the window globals, so restarting it per test would race with the previous one.
func useDiskWarmup(t *testing.T) string {
	t.Helper()
	warmOnce.Do(func() {
		dir, err := os.MkdirTemp("", "flodni-warmup-test-")
		if err != nil {
			t.Fatal(err)
		}
		warmDir = dir
		warmup.InitDiskWarmup(warmup.Options{QuotaGB: 1, Dir: dir, HeadMB: 64, TailMB: 16})
		sharedDisk = warmup.DiskWarmup
	})
	if sharedDisk == nil {
		t.Fatal("index-mode warmup did not start")
	}
	warmup.DiskWarmup = sharedDisk
	t.Cleanup(func() { warmup.DiskWarmup = nil })
	return fmt.Sprintf("%x", sha1.Sum([]byte(fmt.Sprintf("%s#%d", t.Name(), warmSeq.Add(1)))))
}

func TestReadInnerServesTheHeadFromTheSSDWarmup(t *testing.T) {
	fe := useEngineReadPath(t)
	hash := useDiskWarmup(t)
	h := engineHandle("/test/ssd-head.mkv", 100*mb)
	h.hash, h.usesWarmup = hash, true
	warmup.DiskWarmup.WriteChunk(hash, 3, wantPattern(int(mb), 0), 0)
	waitUntil(t, "the head write", func() bool { return warmup.DiskWarmup.GetAvailableRange(hash, 3) >= mb })

	got := readBytes(t, h, 4096, 4096)
	if !bytes.Equal(got, wantPattern(4096, 4096)) {
		t.Error("the head read returned the wrong bytes")
	}
	if len(fe.aheadCalls())+len(fe.blockCalls()) != 0 {
		t.Error("a head hit must not touch the engine")
	}
}

func TestReadInnerFillsTheHeadWarmupFromAFetch(t *testing.T) {
	fe := useEngineReadPath(t)
	hash := useDiskWarmup(t)
	h := engineHandle("/test/ssd-head-fill.mkv", 100*mb)
	h.hash, h.usesWarmup = hash, true

	readBytes(t, h, 256*1024, 0) // empty SSD: network fetch, then mirrored into the head file
	waitUntil(t, "the head file", func() bool { return warmup.DiskWarmup.GetAvailableRange(hash, 3) > 0 })
	if len(fe.aheadCalls()) != 1 {
		t.Errorf("FetchAhead calls = %d, want 1", len(fe.aheadCalls()))
	}
}

func TestReadInnerTailMissFetchesBlockThenServesFromTheSSD(t *testing.T) {
	fe := useEngineReadPath(t)
	hash := useDiskWarmup(t)
	const size = 100 * mb
	h := engineHandle("/test/ssd-tail.mkv", size)
	h.hash, h.usesWarmup = hash, true
	h.lastOff = size - 16*mb // the handle is already reading the end
	off := int64(size - 16*mb + 4096)

	got := readBytes(t, h, 4096, off)
	if !bytes.Equal(got, wantPattern(4096, off)) {
		t.Error("the tail miss returned the wrong bytes")
	}
	if calls := fe.blockCalls(); len(calls) != 1 || calls[0] != (blockCall{hash, 3, off, 4096}) {
		t.Fatalf("FetchBlock calls = %+v, want one stateless fetch of the requested range", calls)
	}
	got = readBytes(t, h, 4096, off)
	if !bytes.Equal(got, wantPattern(4096, off)) || len(fe.blockCalls()) != 1 {
		t.Errorf("the repeat read must come from the SSD tail (FetchBlock calls = %d)", len(fe.blockCalls()))
	}
	if len(fe.aheadCalls()) != 0 {
		t.Error("the tail path must not start a read-ahead window")
	}
}

func TestReadInnerTailFetchBlockErrorFallsBackToFetchAhead(t *testing.T) {
	cases := []struct {
		name      string
		confirmed bool
		wantTail  bool
	}{
		{"discovery: the tail is mirrored", false, true},
		{"confirmed playback: the frozen tail is left alone", true, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fe := useEngineReadPath(t)
			hash := useDiskWarmup(t)
			fe.blockFn = func(int, int64, []byte) (int, error) { return 0, errors.New("no peers") }
			const size = 100 * mb
			path := fmt.Sprintf("/test/ssd-tail-fallback-%d.mkv", i)
			if c.confirmed {
				app.playbackRegistry.Store(path, &PlaybackState{Path: path, ConfirmedAt: time.Now()})
				t.Cleanup(func() { app.playbackRegistry.Delete(path) })
			}
			h := engineHandle(path, size)
			h.hash, h.usesWarmup = hash, true
			h.lastOff = size - 16*mb
			off := int64(size - 16*mb + 4096)

			got := readBytes(t, h, 4096, off)
			if !bytes.Equal(got, wantPattern(4096, off)) {
				t.Error("the fallback returned the wrong bytes")
			}
			if len(fe.blockCalls()) != 1 || len(fe.aheadCalls()) != 1 {
				t.Errorf("calls: block=%d ahead=%d, want 1 and 1", len(fe.blockCalls()), len(fe.aheadCalls()))
			}
			n, _ := warmup.DiskWarmup.ReadTail(hash, 3, make([]byte, 4096), off, size)
			if (n > 0) != c.wantTail {
				t.Errorf("SSD tail holds the range = %v, want %v", n > 0, c.wantTail)
			}
		})
	}
}

// --- pump ---

func pumpHandle(path string, size int64) (*MkvHandle, *NativePumpState) {
	h := engineHandle(path, size)
	h.lastActivityTime = time.Now()
	h.hasSlot.Store(false)
	ps := &NativePumpState{path: path, refCount: 1}
	return h, ps
}

func takeSlot(h *MkvHandle) {
	masterDataSemaphore <- struct{}{}
	h.hasSlot.Store(true)
}

func TestNativePumpWithoutAReaderReturnsItsSlot(t *testing.T) {
	useEngineReadPath(t)
	h, ps := pumpHandle("/test/pump-nil.mkv", 10*mb)
	takeSlot(h)

	h.nativePump(context.Background(), nil, 0, ps)
	if h.hasSlot.Load() || len(masterDataSemaphore) != 0 {
		t.Error("the early return must still free the master slot")
	}
}

func TestNativePumpFillsTheCacheUpToEOFAndTearsDown(t *testing.T) {
	useEngineReadPath(t)
	const path = "/test/pump-eof.mkv"
	h, ps := pumpHandle(path, 10*mb) // chunks at 0, 4 and 8 MiB (2 MiB last)
	h.hash = ""                      // exercises the late-resolution warning (no link in the URL)
	rd := &fakeReader{}
	takeSlot(h)
	app.activePumps.Store(path, ps)
	t.Cleanup(func() { app.activePumps.Delete(path) })

	h.nativePump(context.Background(), rd, 0, ps)

	want := []readCall{{0, int(4 * mb)}, {4 * mb, int(4 * mb)}, {8 * mb, int(2 * mb)}}
	got := rd.readCalls()
	if len(got) != len(want) {
		t.Fatalf("reads = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("read %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if !cachedAcross(path, 0, 10*mb-1) {
		t.Error("the pump must have cached the whole file")
	}
	if rd.closes.Load() != 1 {
		t.Errorf("reader closed %d times, want 1", rd.closes.Load())
	}
	if _, ok := app.activePumps.Load(path); ok {
		t.Error("the pump must deregister itself at EOF")
	}
	if h.hasSlot.Load() || len(masterDataSemaphore) != 0 {
		t.Error("the master slot must be returned")
	}
}

func TestNativePumpLeavesAReplacementPumpRegistered(t *testing.T) {
	useEngineReadPath(t)
	const path = "/test/pump-replaced.mkv"
	h, ps := pumpHandle(path, 1*mb)
	rd := &fakeReader{}
	takeSlot(h)
	replacement := &NativePumpState{path: path}
	app.activePumps.Store(path, replacement)
	t.Cleanup(func() { app.activePumps.Delete(path) })

	h.nativePump(context.Background(), rd, 0, ps)
	if v, ok := app.activePumps.Load(path); !ok || v != replacement {
		t.Error("an old pump's exit must not deregister the pump that replaced it")
	}
	if rd.closes.Load() != 1 {
		t.Error("the old pump still closes its own reader")
	}
}

func TestNativePumpStopsOnCancelAndOnIdle(t *testing.T) {
	cancelled, c := context.WithCancel(context.Background())
	c()
	cases := []struct {
		name  string
		ctx   context.Context
		setup func(h *MkvHandle)
	}{
		{"handle released: context cancelled", cancelled, func(*MkvHandle) {}},
		{"no activity for longer than the scan timeout", context.Background(), func(h *MkvHandle) {
			h.lastActivityTime = time.Now().Add(-time.Hour)
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useEngineReadPath(t)
			h, ps := pumpHandle(fmt.Sprintf("/test/pump-stop-%d.mkv", i), 100*mb)
			tc.setup(h)
			rd := &fakeReader{}
			takeSlot(h)

			h.nativePump(tc.ctx, rd, 0, ps)
			if n := len(rd.readCalls()); n != 0 {
				t.Errorf("reads = %d, want none", n)
			}
			if rd.closes.Load() != 1 || h.hasSlot.Load() || len(masterDataSemaphore) != 0 {
				t.Error("the reader must be closed and the slot freed")
			}
		})
	}
}

func TestNativePumpRetriesAChunkAfterAReaderErrorAndResetsTheInterruptFlag(t *testing.T) {
	useEngineReadPath(t)
	const path = "/test/pump-error.mkv"
	h, ps := pumpHandle(path, 4*mb)
	ps.interruptPending.Store(true) // a seek interrupted the read
	rd := &fakeReader{readFn: func(i int, p []byte, off int64) (int, error) {
		fillPattern(p, off)
		if i == 0 {
			return int(mb), io.ErrClosedPipe // interrupted part way
		}
		return len(p), nil
	}}
	takeSlot(h)

	start := time.Now()
	h.nativePump(context.Background(), rd, 0, ps)

	got := rd.readCalls()
	if len(got) != 2 || got[0].off != 0 || got[1].off != 0 {
		t.Fatalf("reads = %+v, want the same chunk twice", got)
	}
	if ps.interruptPending.Load() {
		t.Error("the retry must re-arm the interrupt flag")
	}
	if !raCache.Covered(path, 0, 4*mb-1) {
		t.Error("the retried chunk must be cached")
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Errorf("took %v: the retry waits ~200ms", d)
	}
}

func TestNativePumpSnapsToThePlayerAfterASeek(t *testing.T) {
	cases := []struct {
		name          string
		start, player int64
	}{
		{"player jumped far ahead of the pump", 0, 500 * mb},
		{"pump stranded far ahead of the player", 800 * mb, 100 * mb},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			useEngineReadPath(t)
			path := fmt.Sprintf("/test/pump-snap-%d.mkv", i)
			h, ps := pumpHandle(path, 1*gb)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rd := &fakeReader{readFn: func(_ int, p []byte, off int64) (int, error) {
				cancel() // one chunk is enough to see where the pump went
				fillPattern(p, off)
				return len(p), nil
			}}
			primary := engineHandle(path, 1*gb)
			primary.isPrimaryHandle.Store(true)
			primary.lastOff = c.player
			registerHandle(t, primary)
			takeSlot(h)

			h.nativePump(ctx, rd, c.start, ps)
			got := rd.readCalls()
			if len(got) != 1 || got[0].off != c.player {
				t.Errorf("reads = %+v, want one at the player's chunk %d", got, c.player)
			}
		})
	}
}

func TestNativePumpChunk(t *testing.T) {
	type chunkCase struct {
		name       string
		size       int64
		offset     int64
		playerOff  int64
		cached     bool
		readN      int
		readErr    error
		wantStop   bool
		wantNext   int64
		wantReads  int
		wantCached bool
	}
	const chunk = 4 * mb
	cases := []chunkCase{
		{name: "past EOF stops", size: 8 * mb, offset: 8 * mb, wantStop: true, wantNext: 8 * mb},
		{name: "a lead beyond the limit waits without reading", size: 1 * gb, offset: 300 * mb, playerOff: 0, wantNext: 300 * mb},
		{name: "an already cached chunk is skipped", size: 1 * gb, offset: 100 * mb, playerOff: 100 * mb, cached: true, wantNext: 100*mb + chunk},
		{name: "a full read advances one chunk", size: 1 * gb, offset: 100 * mb, playerOff: 100 * mb, readN: int(chunk), wantNext: 100*mb + chunk, wantReads: 1, wantCached: true},
		{name: "a short read without an error snaps to the next chunk boundary", size: 1 * gb, offset: 100 * mb, playerOff: 100 * mb, readN: int(mb), wantNext: 100*mb + chunk, wantReads: 1, wantCached: false},
		{name: "an error stops and keeps the bytes that arrived", size: 1 * gb, offset: 100 * mb, playerOff: 100 * mb, readN: int(mb), readErr: io.ErrUnexpectedEOF, wantStop: true, wantNext: 100*mb + mb, wantReads: 1},
		{name: "the last chunk is clamped to EOF", size: 102 * mb, offset: 100 * mb, playerOff: 100 * mb, readN: int(2 * mb), wantNext: 100*mb + 2*mb, wantReads: 1, wantCached: true},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			useEngineReadPath(t)
			path := fmt.Sprintf("/test/pump-chunk-%d.mkv", i)
			h := engineHandle(path, c.size)
			rd := &fakeReader{readFn: func(_ int, p []byte, off int64) (int, error) {
				fillPattern(p, off)
				return c.readN, c.readErr
			}}
			if c.cached {
				raCache.Put(path, c.offset, c.offset+chunk-1, wantPattern(int(chunk), c.offset))
			}

			stop, next := h.nativePumpChunk(rd, c.offset, chunk, c.playerOff)
			if stop != c.wantStop || next != c.wantNext {
				t.Errorf("got (stop=%v, next=%d), want (%v, %d)", stop, next, c.wantStop, c.wantNext)
			}
			if n := len(rd.readCalls()); n != c.wantReads {
				t.Errorf("reads = %d, want %d", n, c.wantReads)
			}
			if c.wantCached && !raCache.Covered(path, c.offset, c.offset+int64(c.readN)-1) {
				t.Error("the bytes read must be cached")
			}
		})
	}
}

func TestNativePumpChunkWritesTheHeadWarmup(t *testing.T) {
	useEngineReadPath(t)
	hash := useDiskWarmup(t)
	const path = "/test/pump-chunk-ssd.mkv"
	h := engineHandle(path, 100*mb)
	h.hash, h.usesWarmup = hash, true
	h.state.Store(stateStreaming) // the warmup-zone skip is for fresh handles only
	rd := &fakeReader{}

	if stop, next := h.nativePumpChunk(rd, 0, 4*mb, 0); stop || next != 4*mb {
		t.Fatalf("got (%v, %d)", stop, next)
	}
	waitUntil(t, "the head file", func() bool { return warmup.DiskWarmup.GetAvailableRange(hash, 3) >= 4*mb })

	// A second pass finds the chunk cached and still mirrors it to the head file.
	if stop, next := h.nativePumpChunk(rd, 0, 4*mb, 0); stop || next != 4*mb || len(rd.readCalls()) != 1 {
		t.Errorf("the cached chunk must be skipped without a read (reads=%d)", len(rd.readCalls()))
	}

	// A fresh handle in WARMUP (own cache key, same file) skips a chunk the SSD covers.
	fresh := engineHandle("/test/pump-chunk-ssd-fresh.mkv", 100*mb)
	fresh.hash, fresh.usesWarmup = hash, true
	if stop, next := fresh.nativePumpChunk(rd, 0, 4*mb, 0); stop || next != 4*mb || len(rd.readCalls()) != 1 {
		t.Errorf("got (%v, %d)", stop, next)
	}
}

func TestStartNativePumpRegistersTheReaderAndRunsToEOF(t *testing.T) {
	fe := useEngineReadPath(t)
	const path = "/test/pump-start.mkv"
	h, _ := pumpHandle(path, 10*mb)
	gate := make(chan struct{})
	fe.newRead = func() *fakeReader {
		return &fakeReader{readFn: func(_ int, p []byte, off int64) (int, error) {
			<-gate
			fillPattern(p, off)
			return len(p), nil
		}}
	}
	t.Cleanup(func() { app.activePumps.Delete(path) })

	h.startNativePump(testHash, 3)
	if !h.hasSlot.Load() || len(masterDataSemaphore) != 1 {
		t.Fatal("the pump must hold a master slot")
	}
	v, ok := app.activePumps.Load(path)
	if !ok {
		t.Fatal("the pump must be registered before it starts reading")
	}
	ps := v.(*NativePumpState)
	if fe.readerCount() != 1 || ps.reader != fe.readers[0] || h.nativeReader != fe.readers[0] || h.pumpState != ps {
		t.Error("the handle and the shared state must hold the reader the engine made")
	}
	if atomic.LoadInt32(&ps.refCount) != 1 || !h.isPrimaryHandle.Load() {
		t.Error("the creator holds one reference and is primary")
	}
	close(gate)
	waitUntil(t, "the pump to finish", func() bool { return !h.hasSlot.Load() })
	if got := len(fe.readers[0].readCalls()); got != 3 {
		t.Errorf("pump reads = %d, want 3 chunks", got)
	}
	if !cachedAcross(path, 0, 10*mb-1) || fe.readers[0].closes.Load() != 1 {
		t.Error("the pump must fill the cache and close its reader")
	}
}

func TestStartNativePumpResumesFromTheCachedOffsetAndAnchorsToThePlayer(t *testing.T) {
	cases := []struct {
		name      string
		cached    bool // a stale high-water mark in raCache
		lastOff   int64
		wantFirst int64
	}{
		{"cold start", false, -1, 0},
		{"resume anchor: the player is far ahead of the cache", true, 40 * mb, 40 * mb},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fe := useEngineReadPath(t)
			path := fmt.Sprintf("/test/pump-resume-%d.mkv", i)
			h, _ := pumpHandle(path, 48*mb)
			h.lastOff = c.lastOff
			if c.cached {
				raCache.Put(path, 0, 4*mb-1, wantPattern(int(4*mb), 0))
			}
			t.Cleanup(func() { app.activePumps.Delete(path) })

			h.startNativePump(testHash, 3)
			waitUntil(t, "the pump to finish", func() bool { return !h.hasSlot.Load() && fe.readerCount() == 1 })
			got := fe.readers[0].readCalls()
			if len(got) == 0 || got[0].off != c.wantFirst {
				t.Errorf("reads = %+v, want the first at %d", got, c.wantFirst)
			}
		})
	}
}

func TestStartNativePumpAttachesToARunningPump(t *testing.T) {
	cases := []struct {
		name         string
		refs         int32
		playerOff    int64
		wantPrimary  bool
		wantLastOff  int64
		wantRefsNext int32
	}{
		{"a second handle joins a live pump", 1, 50 * mb, false, -1, 2},
		{"a reconnect after every handle left inherits the player position", 0, 50 * mb, true, 50 * mb, 1},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fe := useEngineReadPath(t)
			path := fmt.Sprintf("/test/pump-attach-%d.mkv", i)
			h, _ := pumpHandle(path, 100*mb)
			h.lastOff = -1
			rd := &fakeReader{}
			ps := &NativePumpState{path: path, refCount: c.refs, reader: rd, cancel: func() {}, playerOff: c.playerOff}
			app.activePumps.Store(path, ps)
			t.Cleanup(func() { app.activePumps.Delete(path) })

			h.startNativePump(testHash, 3)
			if !h.hasSlot.Load() || h.pumpState != ps || h.nativeReader != streamReader(rd) {
				t.Error("the handle must share the running pump")
			}
			if got := atomic.LoadInt32(&ps.refCount); got != c.wantRefsNext {
				t.Errorf("refCount = %d, want %d", got, c.wantRefsNext)
			}
			if h.isPrimaryHandle.Load() != c.wantPrimary || atomic.LoadInt64(&h.lastOff) != c.wantLastOff {
				t.Errorf("primary=%v lastOff=%d, want %v and %d", h.isPrimaryHandle.Load(), h.lastOff, c.wantPrimary, c.wantLastOff)
			}
			if fe.readerCount() != 0 {
				t.Error("attaching must not create a second reader")
			}
		})
	}
}

func TestStartNativePumpDeniesABackgroundScanWhenTheReserveIsTaken(t *testing.T) {
	fe := useEngineReadPath(t)
	h, _ := pumpHandle("/test/pump-denied.mkv", 10*mb)
	for i := 0; i < cap(masterDataSemaphore)-5; i++ { // scanSlotLimit with no healthy playback
		masterDataSemaphore <- struct{}{}
	}

	h.startNativePump(testHash, 3)
	if h.hasSlot.Load() || fe.readerCount() != 0 {
		t.Error("a scan must not take a slot from the playback reserve")
	}
}

func TestStartNativePumpReturnsAtOnceForAHandleThatHoldsASlot(t *testing.T) {
	fe := useEngineReadPath(t)
	h, _ := pumpHandle("/test/pump-has-slot.mkv", 10*mb)
	h.hasSlot.Store(true)

	h.startNativePump(testHash, 3)
	if fe.readerCount() != 0 || len(masterDataSemaphore) != 0 {
		t.Error("a handle with a slot must not start a second pump")
	}
}

func TestReadUpgradesToAPumpOnTheFlyForConfirmedPlayback(t *testing.T) {
	fe := useEngineReadPath(t)
	const path = "/test/pump-upgrade.mkv"
	app.playbackRegistry.Store(path, &PlaybackState{Path: path, IsHealthy: true})
	t.Cleanup(func() { app.playbackRegistry.Delete(path); app.activePumps.Delete(path) })
	h := engineHandle(path, 10*mb)
	h.hasSlot.Store(false)
	h.lastActivityTime = time.Now()
	gate := make(chan struct{})
	fe.newRead = func() *fakeReader {
		return &fakeReader{readFn: func(_ int, p []byte, off int64) (int, error) {
			<-gate
			fillPattern(p, off)
			return len(p), nil
		}}
	}

	got := readBytes(t, h, 256*1024, 1*mb)
	if !bytes.Equal(got, wantPattern(256*1024, 1*mb)) {
		t.Error("wrong bytes")
	}
	v, ok := app.activePumps.Load(path)
	if !ok || fe.readerCount() != 1 || v.(*NativePumpState).reader != streamReader(fe.readers[0]) || !h.hasSlot.Load() {
		t.Fatal("a streaming read during confirmed playback must start a pump with a slot")
	}
	close(gate)
	waitUntil(t, "the pump to finish", func() bool { return !h.hasSlot.Load() })
}

func TestReadAttachesToTheRunningPumpInsteadOfUpgrading(t *testing.T) {
	fe := useEngineReadPath(t)
	const path = "/test/pump-attach-on-read.mkv"
	rd := &fakeReader{}
	ps := &NativePumpState{path: path, refCount: 1, reader: rd, cancel: func() {}}
	app.activePumps.Store(path, ps)
	t.Cleanup(func() { app.activePumps.Delete(path) })
	h := engineHandle(path, 100*mb)
	h.hasSlot.Store(false)
	registerHandle(t, h)

	readBytes(t, h, 256*1024, 50*mb)
	if !h.hasSlot.Load() || h.pumpState != ps || atomic.LoadInt32(&ps.refCount) != 2 {
		t.Error("a read must attach a slotless handle to the running pump")
	}
	if fe.readerCount() != 0 {
		t.Error("no second reader")
	}
}
