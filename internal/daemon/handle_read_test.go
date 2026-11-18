package daemon

import (
	"bytes"
	"context"
	"flodni/internal/gostorm/native"
	"flodni/internal/readahead"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// These tests drive MkvHandle.Read, readInner and Release through the real package
// globals. They swap raCache, nativeBridge, masterDataSemaphore and readInnerCall and
// restore them on cleanup, so none of them runs in parallel.

// useFreshReadPath gives the test its own read-ahead cache and no native bridge: a branch
// that wrongly reaches the engine panics on the nil client instead of passing silently.
func useFreshReadPath(t *testing.T) {
	t.Helper()
	useTestGlobals(t)
	prevCache, prevBridge, prevSem := raCache, nativeBridge, masterDataSemaphore
	raCache = readahead.New(readahead.Config{
		Base:   func() int64 { return gc().ReadAheadBase },
		Budget: func() int64 { return gc().ReadAheadBudget },
		Logf:   func(string, ...any) {},
		Go:     func(f func()) { go f() },

		PlayerOff: raPlayerOff,
	})
	nativeBridge = nil
	t.Cleanup(func() {
		raCache, nativeBridge, masterDataSemaphore = prevCache, prevBridge, prevSem
	})
}

func readHandle(path string, size int64) *MkvHandle {
	h := &MkvHandle{path: path, size: size, lastOff: -1}
	h.lastGlobalUpdate = time.Now() // skips globalCleanupManager, which is nil in tests
	h.hasSlot.Store(true)           // skips the on-demand prefetch and the semaphore
	return h
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func TestReadInnerServesFromRACacheWithoutTouchingTheEngine(t *testing.T) {
	useFreshReadPath(t)
	const path = "/test/ra-hit.mkv"
	h := readHandle(path, 10*gb)
	chunk := pattern(int(16 * mb))
	raCache.Put(path, 0, int64(len(chunk))-1, chunk)

	dest := make([]byte, 4096)
	res, errno := h.Read(context.Background(), dest, 100)
	if errno != 0 {
		t.Fatalf("errno = %v", errno)
	}
	got, _ := res.Bytes(nil)
	if !bytes.Equal(got, chunk[100:100+4096]) {
		t.Error("Read returned bytes that differ from the cached chunk")
	}
	if atomic.LoadInt64(&h.lastOff) != 100 || h.lastLen != 4096 {
		t.Errorf("handle position = (%d, %d), want (100, 4096)", h.lastOff, h.lastLen)
	}
	if h.state.Load() != stateWarmup {
		t.Error("a read inside the warmup zone must keep the handle in WARMUP")
	}
}

func TestReadInnerReadAtOrPastEOFIsEmpty(t *testing.T) {
	useFreshReadPath(t)
	h := readHandle("/test/eof.mkv", 1000)
	for _, off := range []int64{1000, 5000} {
		res, errno := h.Read(context.Background(), make([]byte, 100), off)
		if errno != 0 || res == nil || res.Size() != 0 {
			t.Errorf("off %d: want an empty successful read, got size=%v errno=%v", off, res, errno)
		}
	}
}

func TestReadInnerLeavesWarmupForStreamingOnASeekPastTheWarmupZone(t *testing.T) {
	useFreshReadPath(t)
	const path = "/test/to-streaming.mkv"
	h := readHandle(path, 10*gb)
	start := int64(96) * mb // chunk-aligned, past the 64MB warmup zone
	raCache.Put(path, start, start+16*mb-1, pattern(int(16*mb)))

	if _, errno := h.Read(context.Background(), make([]byte, 4096), start+mb); errno != 0 {
		t.Fatalf("errno = %v", errno)
	}
	if h.state.Load() != stateStreaming {
		t.Errorf("state = %s, want STREAMING after a first read past the warmup zone", stateName(h.state.Load()))
	}
}

func TestReadInnerRecordsPlaybackProgressOnlyForAHealthyPrimaryHandle(t *testing.T) {
	newTestApp(t)
	useFreshReadPath(t)
	const path = "/test/progress.mkv"
	off := int64(96)*mb + mb
	raCache.Put(path, 96*mb, 96*mb+16*mb-1, pattern(int(16*mb)))

	cases := []struct {
		name               string
		healthy, isPrimary bool
		wantSeek           int64
	}{
		{"healthy primary records the position", true, true, off},
		{"healthy secondary does not", true, false, 0},
		{"unconfirmed primary does not", false, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ps := &PlaybackState{Path: path, IsHealthy: c.healthy}
			app.playbackRegistry.Store(path, ps)
			t.Cleanup(func() { app.playbackRegistry.Delete(path) })
			h := readHandle(path, 10*gb)
			h.isPrimaryHandle.Store(c.isPrimary)

			if _, errno := h.Read(context.Background(), make([]byte, 4096), off); errno != 0 {
				t.Fatalf("errno = %v", errno)
			}
			ps.mu.RLock()
			defer ps.mu.RUnlock()
			if ps.ReadCount != 1 || ps.LastSeekOff != c.wantSeek {
				t.Errorf("ReadCount=%d LastSeekOff=%d, want 1 and %d", ps.ReadCount, ps.LastSeekOff, c.wantSeek)
			}
		})
	}
}

func TestReadInnerCacheMissWithoutAHashFailsWithEIO(t *testing.T) {
	useFreshReadPath(t)
	h := readHandle("/test/miss.mkv", 10*gb)
	// A streaming-sized read skips the rate limiter; no hash means nothing can fetch it.
	res, errno := h.Read(context.Background(), make([]byte, 256*1024), 500*mb)
	if errno != syscall.EIO || res != nil {
		t.Errorf("got (%v, %v), want (nil, EIO)", res, errno)
	}
}

func TestReadInnerGivesUpWithEINTRWhenTheContextEndsWhileWaitingForASlot(t *testing.T) {
	useFreshReadPath(t)
	masterDataSemaphore = make(chan struct{}, 1)
	masterDataSemaphore <- struct{}{} // saturated
	h := readHandle("/test/no-slot.mkv", 10*gb)
	h.hasSlot.Store(false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, errno := h.Read(ctx, make([]byte, 256*1024), 500*mb)
	if errno != syscall.EINTR || res != nil {
		t.Errorf("got (%v, %v), want (nil, EINTR)", res, errno)
	}
}

// fakeReadInner replaces readInnerCall with a script of read sizes: call i copies
// sizes[i] bytes of data (at the requested offset) into dest, or the last size once the
// script runs out.
func fakeReadInner(t *testing.T, data []byte, sizes ...int) *int {
	t.Helper()
	prev := readInnerCall
	calls := 0
	readInnerCall = func(_ *MkvHandle, _ context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
		i := calls
		calls++
		if i >= len(sizes) {
			i = len(sizes) - 1
		}
		n := min(sizes[i], len(dest))
		n = copy(dest[:n], data[off:])
		return fuse.ReadResultData(dest[:n]), 0
	}
	t.Cleanup(func() { readInnerCall = prev })
	return &calls
}

func shortReadCounters() (count, repaired, failed int64) {
	return fuseShortReadCount.Load(), fuseShortReadRepaired.Load(), fuseShortReadFailed.Load()
}

func TestReadRefetchesTheMissingTailOfAShortRead(t *testing.T) {
	useTestGlobals(t)
	data := pattern(1000)
	calls := fakeReadInner(t, data, 60, 40)
	h := readHandle("/test/repair.mkv", 1000)
	c0, r0, f0 := shortReadCounters()

	dest := make([]byte, 100)
	res, errno := h.Read(context.Background(), dest, 0)
	if errno != 0 {
		t.Fatalf("errno = %v", errno)
	}
	got, _ := res.Bytes(nil)
	if !bytes.Equal(got, data[:100]) {
		t.Error("the repaired read must carry the first and the refetched bytes in order")
	}
	if *calls != 2 {
		t.Errorf("readInner calls = %d, want 2", *calls)
	}
	c1, r1, f1 := shortReadCounters()
	if c1-c0 != 1 || r1-r0 != 1 || f1-f0 != 0 {
		t.Errorf("counters delta = (%d, %d, %d), want (1, 1, 0)", c1-c0, r1-r0, f1-f0)
	}
}

func TestReadFailsWithEIOWhenTheGapStaysAfterTheRetryBound(t *testing.T) {
	useTestGlobals(t)
	calls := fakeReadInner(t, pattern(1000), 10) // every call returns 10 bytes
	h := readHandle("/test/unfillable.mkv", 1000)
	c0, r0, f0 := shortReadCounters()

	res, errno := h.Read(context.Background(), make([]byte, 100), 0)
	if errno != syscall.EIO || res != nil {
		t.Fatalf("got (%v, %v), want (nil, EIO): a partial read would cache zeros", res, errno)
	}
	if want := 1 + shortReadRefillAttempts; *calls != want {
		t.Errorf("readInner calls = %d, want the first read plus %d refill attempts", *calls, shortReadRefillAttempts)
	}
	c1, r1, f1 := shortReadCounters()
	if c1-c0 != 1 || r1-r0 != 0 || f1-f0 != 1 {
		t.Errorf("counters delta = (%d, %d, %d), want (1, 0, 1)", c1-c0, r1-r0, f1-f0)
	}
}

func TestReadFailsWithEIOWhenTheRefillReturnsNothing(t *testing.T) {
	useTestGlobals(t)
	calls := fakeReadInner(t, pattern(1000), 60, 0)
	h := readHandle("/test/empty-refill.mkv", 1000)

	if res, errno := h.Read(context.Background(), make([]byte, 100), 0); errno != syscall.EIO || res != nil {
		t.Errorf("got (%v, %v), want (nil, EIO)", res, errno)
	}
	if *calls != 2 {
		t.Errorf("readInner calls = %d, want to stop at the first empty refill (2)", *calls)
	}
}

func TestReadReturnsAShortReadAtEOFAsIs(t *testing.T) {
	useTestGlobals(t)
	data := pattern(50)
	calls := fakeReadInner(t, data, 50)
	h := readHandle("/test/at-eof.mkv", 50)
	c0, _, _ := shortReadCounters()

	res, errno := h.Read(context.Background(), make([]byte, 100), 0)
	if errno != 0 || res.Size() != 50 {
		t.Fatalf("got size=%v errno=%v, want 50 bytes", res, errno)
	}
	if *calls != 1 {
		t.Errorf("readInner calls = %d, want 1: a short read at EOF is not retried", *calls)
	}
	if c1, _, _ := shortReadCounters(); c1 != c0 {
		t.Error("a short read at EOF must not count as a short read")
	}
}

func TestReadPassesTheInnerErrnoThrough(t *testing.T) {
	useTestGlobals(t)
	prev := readInnerCall
	readInnerCall = func(*MkvHandle, context.Context, []byte, int64) (fuse.ReadResult, syscall.Errno) {
		return nil, syscall.ETIMEDOUT
	}
	t.Cleanup(func() { readInnerCall = prev })
	h := readHandle("/test/errno.mkv", 1000)

	if res, errno := h.Read(context.Background(), make([]byte, 100), 0); errno != syscall.ETIMEDOUT || res != nil {
		t.Errorf("got (%v, %v), want (nil, ETIMEDOUT)", res, errno)
	}
}

func TestReadStopsRefillingWhenTheContextIsCancelled(t *testing.T) {
	useTestGlobals(t)
	ctx, cancel := context.WithCancel(context.Background())
	prev := readInnerCall
	calls := 0
	readInnerCall = func(_ *MkvHandle, _ context.Context, dest []byte, _ int64) (fuse.ReadResult, syscall.Errno) {
		calls++
		cancel() // the kernel gave up on the request mid-read
		return fuse.ReadResultData(dest[:10]), 0
	}
	t.Cleanup(func() { readInnerCall = prev })
	h := readHandle("/test/cancel.mkv", 1000)

	if res, errno := h.Read(ctx, make([]byte, 100), 0); errno != syscall.EINTR || res != nil {
		t.Errorf("got (%v, %v), want (nil, EINTR)", res, errno)
	}
	if calls != 1 {
		t.Errorf("readInner calls = %d, want 1: no refill after cancellation", calls)
	}
}

func TestReleaseRemovesTheHandleAndSurvivesNoPump(t *testing.T) {
	newTestApp(t)
	useTestGlobals(t)
	const path = "/test/release-no-pump.mkv"
	t.Cleanup(func() { stopTimer(&app.priorityTimers, path) })
	h := readHandle(path, 10*gb)
	h.hasSlot.Store(false)
	h.nativeReader = &native.NativeReader{}
	registerHandle(t, h)

	if errno := h.Release(context.Background()); errno != 0 {
		t.Fatalf("errno = %v", errno)
	}
	if _, ok := app.activeHandles.Load(h); ok {
		t.Error("Release must remove the handle from activeHandles")
	}
	if h.nativeReader != nil {
		t.Error("Release must drop the handle's reader reference")
	}
	if _, ok := app.priorityTimers.Load(path); !ok {
		t.Error("Release must schedule the priority-retention timer")
	}
}

func TestReleaseOfASlotlessProbeLeavesTheSharedPumpAlone(t *testing.T) {
	newTestApp(t)
	useTestGlobals(t)
	const path = "/test/release-probe.mkv"
	ps := &NativePumpState{refCount: 2}
	app.activePumps.Store(path, ps)
	t.Cleanup(func() { app.activePumps.Delete(path); stopTimer(&app.priorityTimers, path) })
	h := readHandle(path, 10*gb)
	h.hasSlot.Store(false)
	h.isPrimaryHandle.Store(true)
	h.lastOff = 3 * gb

	h.Release(context.Background())
	if got := atomic.LoadInt32(&ps.refCount); got != 2 {
		t.Errorf("refCount = %d, want 2: a probe without a slot must not decrement", got)
	}
	if got := atomic.LoadInt64(&ps.playerOff); got != 3*gb {
		t.Errorf("playerOff = %d, want the primary handle's last offset", got)
	}
}

func TestReleaseOfAStaleHandleLeavesTheReplacementPumpAlone(t *testing.T) {
	newTestApp(t)
	useTestGlobals(t)
	const path = "/test/release-stale.mkv"
	current := &NativePumpState{refCount: 1}
	app.activePumps.Store(path, current)
	t.Cleanup(func() { app.activePumps.Delete(path); stopTimer(&app.priorityTimers, path) })
	h := readHandle(path, 10*gb)
	h.pumpState = &NativePumpState{refCount: 1} // an older, replaced pump

	h.Release(context.Background())
	if got := atomic.LoadInt32(&current.refCount); got != 1 {
		t.Errorf("refCount = %d, want 1: a handle must not decrement a pump it never counted", got)
	}
}

func TestReleaseOfTheLastHandleStartsOrSkipsTheGracePeriod(t *testing.T) {
	newTestApp(t)
	useTestGlobals(t)
	cases := []struct {
		name      string
		state     *PlaybackState
		wantTimer bool
	}{
		{"unconfirmed probe: grace timer runs", &PlaybackState{}, true},
		{"no playback registered: grace timer runs", nil, true},
		{"healthy playback keeps the pump with no timer", &PlaybackState{IsHealthy: true}, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := "/test/release-last-" + string(rune('a'+i)) + ".mkv"
			ps := &NativePumpState{refCount: 1}
			app.activePumps.Store(path, ps)
			if c.state != nil {
				c.state.Path = path
				app.playbackRegistry.Store(path, c.state)
			}
			t.Cleanup(func() {
				app.activePumps.Delete(path)
				app.playbackRegistry.Delete(path)
				stopTimer(&app.pumpTimers, path)
				stopTimer(&app.priorityTimers, path)
			})
			h := readHandle(path, 10*gb)
			h.pumpState = ps

			h.Release(context.Background())
			if got := atomic.LoadInt32(&ps.refCount); got != 0 {
				t.Errorf("refCount = %d, want 0", got)
			}
			if _, has := app.pumpTimers.Load(path); has != c.wantTimer {
				t.Errorf("grace timer present = %v, want %v", has, c.wantTimer)
			}
			if _, ok := app.activePumps.Load(path); !ok {
				t.Error("the pump must survive until the grace timer fires")
			}
		})
	}
}

// stopTimer stops and forgets the *time.Timer stored under key, so a test never leaves a
// 30s retention or grace timer behind.
func stopTimer(m interface {
	LoadAndDelete(any) (any, bool)
}, key string) {
	if v, ok := m.LoadAndDelete(key); ok {
		v.(*time.Timer).Stop()
	}
}
