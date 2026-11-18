package daemon

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// pumpSim drives several handles on one file through the real Read, Release and pump code
// with the fake engine (the engineBridge and streamReader seam in engine_bridge.go), no
// torrent and no FUSE mount. Engine fetches take fetchLatency, so a read that reaches the
// engine is told apart from one served from the read-ahead cache. It records, per handle,
// every served offset and its latency, and reports the pump's target (where its reader
// last read), the primary-stream owner and the interrupts the pump reader received.
//
// Like the other read-path tests it swaps package globals, so none using it runs in parallel.
type pumpSim struct {
	t            *testing.T
	fe           *fakeEngine
	path         string
	size         int64
	fetchLatency time.Duration
	handles      []*simHandle
}

type simRead struct {
	off  int64
	n    int
	took time.Duration
}

type simHandle struct {
	name  string
	h     *MkvHandle
	sim   *pumpSim
	mu    sync.Mutex
	reads []simRead
}

// newPumpSim arms a confirmed playback of path (size bytes) whose persisted resume position
// is resumeAt, with 4 MiB chunks and a 64 MiB read-ahead budget (useEngineReadPath).
func newPumpSim(t *testing.T, path string, size, resumeAt int64, fetchLatency time.Duration) *pumpSim {
	t.Helper()
	fe := useEngineReadPath(t)
	s := &pumpSim{t: t, fe: fe, path: path, size: size, fetchLatency: fetchLatency}
	fe.aheadFn = func(_ int, off int64, buf, dest []byte, onFill func(int, bool, error)) (int, error) {
		time.Sleep(fetchLatency)
		return fillAhead(off, buf, dest, onFill)
	}
	fe.blockFn = func(_ int, off int64, p []byte) (int, error) {
		time.Sleep(fetchLatency)
		fillPattern(p, off)
		return len(p), nil
	}
	app.playbackRegistry.Store(path, &PlaybackState{
		Path: path, Hash: testHash, IsHealthy: true, ConfirmedAt: time.Now(), SessionStart: time.Now(), OpenedAt: time.Now(), LastSeekOff: resumeAt,
	})
	t.Cleanup(func() {
		if v, ok := app.activePumps.Load(path); ok {
			v.(*NativePumpState).cancel()
		}
		// The pump returns its slot (hasSlot false) from its last defer, after its final
		// log line: waiting for the registry entry alone leaves it reading restored globals.
		waitUntil(t, "the pump to end", s.pumpGone)
		for _, sh := range s.handles {
			app.activeHandles.Delete(sh.h)
		}
		app.playbackRegistry.Delete(path)
		stopTimer(&app.pumpTimers, path)
		stopTimer(&app.priorityTimers, path)
	})
	return s
}

// play is the media server's play webhook arriving now: it starts a session, so every handle
// opened so far predates it and every later one follows it.
func (s *pumpSim) play() {
	ps := s.playback()
	ps.mu.Lock()
	ps.ConfirmedAt, ps.SessionStart = time.Now(), time.Now()
	ps.mu.Unlock()
}

// resume is a resume webhook after a pause: it confirms the playback again but starts no session.
func (s *pumpSim) resume() {
	ps := s.playback()
	ps.mu.Lock()
	ps.ConfirmedAt = time.Now()
	ps.mu.Unlock()
}

func (s *pumpSim) playback() *PlaybackState {
	v, ok := app.playbackRegistry.Load(s.path)
	if !ok {
		s.t.Fatal("no playback registered")
	}
	return v.(*PlaybackState)
}

// shortWindow sets warm_start_idle_seconds, which is how long a primary may be quiet and
// still count as watching, so a test can let one go quiet for real in about a second.
func (s *pumpSim) shortWindow(seconds int) {
	cfg := *gc()
	cfg.WarmStartIdleSeconds = seconds
	globalConfig.Store(&cfg)
}

// endPump cancels the shared pump and waits until it has gone, as a pump that reached EOF has.
func (s *pumpSim) endPump() {
	s.t.Helper()
	s.pumpState().cancel()
	waitUntil(s.t, "the pump to end", s.pumpGone)
}

func (s *pumpSim) pumpGone() bool {
	if _, live := app.activePumps.Load(s.path); live {
		return false
	}
	for _, sh := range s.handles {
		// A creator clears hasSlot from the pump's last defer; an attached handle never does.
		sh.h.mu.Lock()
		watching := sh.h.isWatching
		sh.h.mu.Unlock()
		if !watching && sh.h.hasSlot.Load() {
			return false
		}
	}
	return true
}

// open mirrors VirtualMkvNode.Open for a native file: a fresh handle, the pump started or
// attached at open, the handle registered afterwards.
func (s *pumpSim) open(name string) *simHandle {
	h := engineHandle(s.path, s.size)
	h.hasSlot.Store(false)
	h.lastOff = -1
	h.lastActivityTime = time.Now()
	h.usesWarmup = true
	h.openedAt = time.Now()
	h.state.Store(stateWarmup)
	h.pumpOnce.Do(func() { h.startNativePump(testHash, 3) })
	storeOpenHandle(h)
	sh := &simHandle{name: name, h: h, sim: s}
	s.handles = append(s.handles, sh)
	return sh
}

// read serves one FUSE read through MkvHandle.Read, checks the bytes and records the call.
func (sh *simHandle) read(off int64, n int) simRead {
	sh.sim.t.Helper()
	start := time.Now()
	res, errno := sh.h.Read(context.Background(), make([]byte, n), off)
	took := time.Since(start)
	if errno != 0 || res == nil {
		sh.sim.t.Fatalf("%s: Read(%d, %d) = (%v, %v)", sh.name, n, off, res, errno)
	}
	got, _ := res.Bytes(nil)
	if !bytes.Equal(got, wantPattern(n, off)) {
		sh.sim.t.Fatalf("%s: Read(%d, %d) returned the wrong bytes", sh.name, n, off)
	}
	r := simRead{off: off, n: n, took: took}
	sh.mu.Lock()
	sh.reads = append(sh.reads, r)
	sh.mu.Unlock()
	return r
}

// readRange reads [from, from+length) in step-sized sequential reads.
func (sh *simHandle) readRange(from, length, step int64) {
	sh.sim.t.Helper()
	for off := from; off < from+length; off += step {
		sh.read(off, int(step))
	}
}

func (sh *simHandle) release() {
	sh.sim.t.Helper()
	if errno := sh.h.Release(context.Background()); errno != 0 {
		sh.sim.t.Fatalf("%s: Release = %v", sh.name, errno)
	}
}

func (sh *simHandle) served() []simRead {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return append([]simRead(nil), sh.reads...)
}

// pumpReads is every read any pump reader made, in order per reader.
func (s *pumpSim) pumpReads() []readCall {
	s.fe.mu.Lock()
	readers := append([]*fakeReader(nil), s.fe.readers...)
	s.fe.mu.Unlock()
	var all []readCall
	for _, r := range readers {
		all = append(all, r.readCalls()...)
	}
	return all
}

// pumpTarget is the offset the pump's reader last read: where the pump is fetching.
func (s *pumpSim) pumpTarget() int64 {
	reads := s.pumpReads()
	if len(reads) == 0 {
		return -1
	}
	return reads[len(reads)-1].off
}

func (s *pumpSim) interrupts() int32 {
	s.fe.mu.Lock()
	readers := append([]*fakeReader(nil), s.fe.readers...)
	s.fe.mu.Unlock()
	var n int32
	for _, r := range readers {
		n += r.interrupts.Load()
	}
	return n
}

// primaryOwner names the handles the pump follows. At most one is expected.
func (s *pumpSim) primaryOwner() string {
	var names []string
	for _, sh := range s.handles {
		if sh.h.isPrimaryHandle.Load() {
			if _, live := app.activeHandles.Load(sh.h); live {
				names = append(names, sh.name)
			}
		}
	}
	return fmt.Sprint(names)
}

// demandFetches lists the engine fetches reads made (not the pump): FetchAhead and FetchBlock.
func (s *pumpSim) demandFetches() []int64 {
	var offs []int64
	for _, c := range s.fe.aheadCalls() {
		offs = append(offs, c.off)
	}
	for _, c := range s.fe.blockCalls() {
		offs = append(offs, c.off)
	}
	return offs
}

// settle waits until the pump has made no read for a while: parked at its lead limit, or
// following a new target and done with it. The pump loop sleeps 100-200 ms between checks.
func (s *pumpSim) settle() {
	s.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	last, since := len(s.pumpReads()), time.Now()
	for time.Since(since) < 500*time.Millisecond {
		if time.Now().After(deadline) {
			s.t.Fatalf("the pump did not settle (reads = %d)", last)
		}
		time.Sleep(20 * time.Millisecond)
		if n := len(s.pumpReads()); n != last {
			last, since = n, time.Now()
		}
	}
}

// pumpState is the shared pump registered for the path.
func (s *pumpSim) pumpState() *NativePumpState {
	v, ok := app.activePumps.Load(s.path)
	if !ok {
		s.t.Fatal("no pump registered")
	}
	return v.(*NativePumpState)
}

func (s *pumpSim) resumeOff() int64 {
	v, ok := app.playbackRegistry.Load(s.path)
	if !ok {
		s.t.Fatal("no playback registered")
	}
	ps := v.(*PlaybackState)
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.LastSeekOff
}
