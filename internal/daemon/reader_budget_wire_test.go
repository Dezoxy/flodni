package daemon

import (
	"bytes"
	"context"
	"sync"
	"syscall"
	"testing"
	"time"

	"flodni/internal/gostorm/native"
	"flodni/internal/warmup"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// These tests drive the refusal through the read path (readInner, Read) and the pump
// with the fake engine. refuseEnv arms the gate; the session sits at /m/x.mkv, 1 GiB, so
// offset 500 MiB is past both windows and aligned to the 4 MiB test chunks.

const (
	wireOff    = 500 * mb
	wireMargin = 10 * mb
)

// wireEnv is an engine read path with an armed gate and a session already past its
// margin. The torrent counts as closed, so an unguarded miss would wake it.
func wireEnv(t *testing.T) (*fakeEngine, *TTFFSession, *MkvHandle) {
	t.Helper()
	fe := useEngineReadPath(t)
	s, _ := refuseEnv(t, wireMargin)
	s.unconfirmedSwarm.Store(wireMargin)
	prevLive := torrentLive
	torrentLive = func(string) bool { return false }
	t.Cleanup(func() { torrentLive = prevLive })
	h := engineHandle(s.path, s.size)
	h.magnet, h.wake = "magnet:?xt=urn:btih:"+testHash, fe.Wake
	return fe, s, h
}

func engineQuiet(t *testing.T, fe *fakeEngine) {
	t.Helper()
	if n := len(fe.aheadCalls()) + len(fe.blockCalls()); n != 0 {
		t.Errorf("engine fetches = %d, want none", n)
	}
	fe.mu.Lock()
	defer fe.mu.Unlock()
	if len(fe.woke) != 0 || len(fe.readers) != 0 {
		t.Errorf("wakes = %d, readers = %d, want none", len(fe.woke), len(fe.readers))
	}
}

func TestRefusedMissStartsNothing(t *testing.T) {
	fe, s, h := wireEnv(t)
	h.hasSlot.Store(false) // an unguarded miss would attach a pump or wait for a slot

	res, errno := h.readInner(context.Background(), make([]byte, 256*1024), wireOff)
	if errno != errnoBudgetRefused || res != nil {
		t.Fatalf("got (res nil = %v, %v), want (nil, errnoBudgetRefused)", res == nil, errno)
	}
	engineQuiet(t, fe)
	if h.hasSlot.Load() || len(masterDataSemaphore) != 0 {
		t.Error("a refused miss must take no slot")
	}
	if s.refusedReads.Load() != 1 {
		t.Errorf("refusedReads = %d, want 1", s.refusedReads.Load())
	}
}

func TestCacheHitPastTheMarginIsServedWithoutPrefetch(t *testing.T) {
	fe, _, h := wireEnv(t)
	h.hasSlot.Store(false) // no pump: the hit would prefetch the next chunk
	raCache.Put(h.path, wireOff, wireOff+4*mb-1, wantPattern(int(4*mb), wireOff))

	got := readBytes(t, h, 256*1024, wireOff+3*mb+mb/2) // near the chunk end
	if !bytes.Equal(got, wantPattern(256*1024, wireOff+3*mb+mb/2)) {
		t.Error("the cached bytes were not served")
	}
	time.Sleep(100 * time.Millisecond) // a prefetch goroutine would have run by now
	engineQuiet(t, fe)
}

func TestWindowReadsFetchAsBefore(t *testing.T) {
	fe, _, h := wireEnv(t)
	got := readBytes(t, h, 256*1024, 20*mb) // inside the head window
	if !bytes.Equal(got, wantPattern(256*1024, 20*mb)) {
		t.Error("wrong bytes")
	}
	if n := len(fe.aheadCalls()); n != 1 {
		t.Errorf("FetchAhead calls = %d, want 1", n)
	}
	tail := h.size - warmup.TailWarmupSize + mb
	readBytes(t, h, 256*1024, tail)
	if n := len(fe.aheadCalls()); n != 2 {
		t.Errorf("FetchAhead calls = %d after a tail-window read, want 2", n)
	}
}

func TestSwitchOffFetchesAsBefore(t *testing.T) {
	fe, _, h := wireEnv(t)
	refuseUnconfirmedReads = func() bool { return false }

	got := readBytes(t, h, 256*1024, wireOff)
	if !bytes.Equal(got, wantPattern(256*1024, wireOff)) {
		t.Error("wrong bytes")
	}
	if n := len(fe.aheadCalls()); n != 1 {
		t.Errorf("FetchAhead calls = %d, want 1", n)
	}
	fe.mu.Lock()
	woke := len(fe.woke)
	fe.mu.Unlock()
	if woke != 1 {
		t.Errorf("wakes = %d, want 1 (the torrent counts as closed)", woke)
	}
}

func TestPrefetchSitesAskThePredicate(t *testing.T) {
	// The WarmLanding site (a new handle's first read past the head) is the only
	// prefetch that runs before the cache lookup; with the switch off it fetches.
	cases := []struct {
		name       string
		on         bool
		wantBlocks int
	}{{"refusal on", true, 0}, {"switch off", false, 1}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fe, _, h := wireEnv(t)
			refuseUnconfirmedReads = func() bool { return c.on }
			raCache.Put(h.path, wireOff, wireOff+4*mb-1, wantPattern(int(4*mb), wireOff))
			h.hasSlot.Store(false)

			readBytes(t, h, 256*1024, wireOff)
			waitUntil(t, "prefetches to end", flightFree(h.path, wireOff))
			time.Sleep(100 * time.Millisecond)
			if n := len(fe.blockCalls()); (n > 0) != (c.wantBlocks > 0) {
				t.Errorf("FetchBlock calls = %d, want %d fetches", n, c.wantBlocks)
			}
		})
	}
}

// --- Read: a refusal is not a failed read ---

type outcomeLog struct {
	mu sync.Mutex
	o  []native.Outcome
}

func (l *outcomeLog) record(o native.Outcome) {
	l.mu.Lock()
	l.o = append(l.o, o)
	l.mu.Unlock()
}

func (l *outcomeLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.o)
}

func readerDeadSwarmCheck(t *testing.T, s *TTFFSession, wantFailed int64) {
	t.Helper()
	if got := s.failedReads.Load(); got != wantFailed {
		t.Errorf("failedReads = %d, want %d", got, wantFailed)
	}
	if s.readFailed.Load() {
		t.Error("a refusal must not mark the session as having had a read give up")
	}
	log := &outcomeLog{}
	prev := native.ReachabilityOutcome
	native.ReachabilityOutcome = log.record
	t.Cleanup(func() { native.ReachabilityOutcome = prev })
	s.openedAt.Store(time.Now().Add(-time.Hour).UnixNano())
	s.closeSession()
	if log.count() != 0 {
		t.Errorf("close produced %d reachability outcomes, want none (no dead-swarm verdict)", log.count())
	}
}

func TestReadMapsARefusalToEIOWithoutCountingAFailure(t *testing.T) {
	_, s, h := wireEnv(t)
	_, _, f0 := shortReadCounters()

	res, errno := h.Read(context.Background(), make([]byte, 256*1024), wireOff)
	if errno != syscall.EIO || res != nil {
		t.Fatalf("got (res nil = %v, %v), want (nil, EIO)", res == nil, errno)
	}
	if _, _, f1 := shortReadCounters(); f1 != f0 {
		t.Errorf("fuseShortReadFailed moved by %d, want 0", f1-f0)
	}
	readerDeadSwarmCheck(t, s, 0)
}

// The edge of a window: the SSD serves the first 100 KiB of the read, the refill
// sub-read lies past the window and is refused. raCache never returns a partial hit, so
// the first pass is scripted; the refusal is the real gate.
func TestReadMapsAPartialHitThenARefusedSubReadToEIO(t *testing.T) {
	fe, s, h := wireEnv(t)
	h.hasSlot.Store(false)
	prev := readInnerCall
	calls := 0
	readInnerCall = func(h *MkvHandle, ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
		calls++
		if calls == 1 {
			n := copy(dest[:100*1024], wantPattern(100*1024, off))
			return fuse.ReadResultData(dest[:n]), 0
		}
		return prev(h, ctx, dest, off)
	}
	t.Cleanup(func() { readInnerCall = prev })
	_, _, f0 := shortReadCounters()

	res, errno := h.Read(context.Background(), make([]byte, 256*1024), wireOff)
	if errno != syscall.EIO || res != nil {
		t.Fatalf("got (res nil = %v, %v), want (nil, EIO)", res == nil, errno)
	}
	if calls != 2 {
		t.Fatalf("readInner calls = %d, want the short pass and one refused sub-read", calls)
	}
	if _, _, f1 := shortReadCounters(); f1 != f0 {
		t.Errorf("fuseShortReadFailed moved by %d, want 0", f1-f0)
	}
	engineQuiet(t, fe)
	readerDeadSwarmCheck(t, s, 0)
}

// The two prefetch sites the table above cannot reach: the seek WarmLanding (a read far
// from the last one, with a pump reader attached) and the prefetch after a fetch, which
// is the one that stops on the read that itself crosses the margin.
func TestSeekWarmLandingAsksThePredicate(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "refusal on", false: "switch off"}[on], func(t *testing.T) {
			fe, _, h := wireEnv(t)
			refuseUnconfirmedReads = func() bool { return on }
			h.nativeReader = &fakeReader{}
			h.lastOff = 100 * mb // a seek of more than the 64 MiB budget

			_, _ = h.readInner(context.Background(), make([]byte, 256*1024), wireOff)
			waitUntil(t, "prefetches to end", flightFree(h.path, wireOff))
			time.Sleep(100 * time.Millisecond)
			if blocks := len(fe.blockCalls()); on && blocks != 0 || !on && blocks == 0 {
				t.Errorf("FetchBlock calls = %d with refusal on = %v", blocks, on)
			}
		})
	}
}

func TestPrefetchAfterTheCrossingFetchAsksThePredicate(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "refusal on", false: "switch off"}[on], func(t *testing.T) {
			fe, s, h := wireEnv(t)
			refuseUnconfirmedReads = func() bool { return on }
			s.unconfirmedSwarm.Store(wireMargin - 1) // this read's own bytes cross the margin
			// A fill that stops at the end of the read leaves the cache lagging behind the
			// next chunk, which is what arms the prefetch.
			fe.aheadFn = func(_ int, off int64, buf, dest []byte, onFill func(int, bool, error)) (int, error) {
				fillPattern(buf[:len(dest)], off)
				copy(dest, buf)
				onFill(len(dest), true, nil)
				return len(dest), nil
			}
			off := int64(wireOff + 4*mb - 256*1024)
			h.state.Store(stateStreaming) // no WarmLanding before the fetch: only the site under test
			h.lastOff = off - 256*1024

			res, errno := h.readInner(context.Background(), make([]byte, 256*1024), off)
			if errno != 0 || res == nil {
				t.Fatalf("the crossing read must be served: (%v, %v)", res == nil, errno)
			}
			waitUntil(t, "prefetches to end", flightFree(h.path, wireOff+4*mb))
			time.Sleep(100 * time.Millisecond)
			if blocks := len(fe.blockCalls()); on && blocks != 0 || !on && blocks == 0 {
				t.Errorf("FetchBlock calls = %d with refusal on = %v", blocks, on)
			}
		})
	}
}

// The caller leaves while a refill sub-read is held: EINTR, and nothing counts as a
// failed read.
func TestReadReturnsEINTRWhenTheCallerLeavesMidHold(t *testing.T) {
	_, s, h := wireEnv(t)
	unconfirmedReadHold = 5 * time.Second
	prev := readInnerCall
	calls := 0
	readInnerCall = func(h *MkvHandle, ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
		calls++
		if calls == 1 {
			n := copy(dest[:100*1024], wantPattern(100*1024, off))
			return fuse.ReadResultData(dest[:n]), 0
		}
		return prev(h, ctx, dest, off)
	}
	t.Cleanup(func() { readInnerCall = prev })
	_, _, f0 := shortReadCounters()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(40*time.Millisecond, cancel)

	start := time.Now()
	res, errno := h.Read(ctx, make([]byte, 256*1024), wireOff)
	if errno != syscall.EINTR || res != nil {
		t.Fatalf("got (res nil = %v, %v), want (nil, EINTR)", res == nil, errno)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("the read stayed held after the caller left")
	}
	if _, _, f1 := shortReadCounters(); f1 != f0 {
		t.Errorf("fuseShortReadFailed moved by %d, want 0", f1-f0)
	}
	readerDeadSwarmCheck(t, s, 0)
}

// --- pump ---

func pumpBudgetEnv(t *testing.T) (*MkvHandle, *NativePumpState, *TTFFSession) {
	t.Helper()
	_, s, _ := wireEnv(t)
	h, ps := pumpHandle(s.path, s.size)
	// The player sits at the pump's offset, so the pump does not re-anchor to 0.
	player := engineHandle(s.path, s.size)
	player.isPrimaryHandle.Store(true)
	player.lastOff = wireOff
	registerHandle(t, player)
	takeSlot(h)
	return h, ps, s
}

func runPump(h *MkvHandle, ctx context.Context, rd *fakeReader, ps *NativePumpState) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.nativePump(ctx, rd, wireOff, ps)
	}()
	return done
}

func TestPumpParksPastTheMarginAndResumesOnConfirmation(t *testing.T) {
	h, ps, s := pumpBudgetEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rd := &fakeReader{readFn: func(_ int, p []byte, off int64) (int, error) {
		cancel() // one chunk is enough to see it resume
		fillPattern(p, off)
		return len(p), nil
	}}
	done := runPump(h, ctx, rd, ps)

	time.Sleep(400 * time.Millisecond)
	if n := len(rd.readCalls()); n != 0 {
		t.Fatalf("reads while parked = %d, want 0", n)
	}
	select {
	case <-done:
		t.Fatal("the pump exited; it must stay parked, not cancel")
	default:
	}
	confirmPlayback(s.path)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the pump did not resume after the confirmation")
	}
	if got := rd.readCalls(); len(got) != 1 || got[0].off != wireOff {
		t.Errorf("reads = %+v, want one chunk at %d", got, wireOff)
	}
}

func TestParkedPumpStillExitsOnCancelAndOnIdle(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		h, ps, _ := pumpBudgetEnv(t)
		ctx, cancel := context.WithCancel(context.Background())
		rd := &fakeReader{}
		done := runPump(h, ctx, rd, ps)
		time.Sleep(250 * time.Millisecond)
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("a parked pump ignored the cancel")
		}
		if len(rd.readCalls()) != 0 || rd.closes.Load() != 1 {
			t.Errorf("reads = %d, closes = %d", len(rd.readCalls()), rd.closes.Load())
		}
	})
	t.Run("idle", func(t *testing.T) {
		h, ps, _ := pumpBudgetEnv(t)
		rd := &fakeReader{}
		done := runPump(h, context.Background(), rd, ps)
		time.Sleep(250 * time.Millisecond)
		h.mu.Lock()
		h.lastActivityTime = time.Now().Add(-time.Hour)
		h.mu.Unlock()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("a parked pump ignored the idle timeout")
		}
		if len(rd.readCalls()) != 0 || rd.closes.Load() != 1 {
			t.Errorf("reads = %d, closes = %d", len(rd.readCalls()), rd.closes.Load())
		}
	})
}
