package daemon

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// Release runs once per handle; the kernel and the orphan sweep may both ask. A second call
// must not decrement the shared pump's reference again (it would release a sibling's), move
// the resume position, or free the open-tracker count twice.
func TestReleaseTwiceFreesTheReferenceOnce(t *testing.T) {
	const path = "/test/release-twice.mkv"
	sim := newPumpSim(t, path, 1000*mb, 100*mb, 5*time.Millisecond)
	a := sim.open("A")
	sim.settle()
	c := sim.open("C") // a sibling holding the second reference
	a.readRange(100*mb, 4*probeStep, probeStep)
	ps := sim.pumpState()
	if refs := atomic.LoadInt32(&ps.refCount); refs != 2 || !a.h.isPrimaryHandle.Load() {
		t.Fatalf("setup: refCount = %d, A primary = %v, want 2 and true", refs, a.h.isPrimaryHandle.Load())
	}

	a.release()
	pos := atomic.LoadInt64(&ps.playerOff)
	open := globalOpenTracker.CountByPath(path)
	if pos != 100*mb+3*probeStep || atomic.LoadInt32(&ps.refCount) != 1 {
		t.Fatalf("first Release: playerOff = %d, refCount = %d", pos, atomic.LoadInt32(&ps.refCount))
	}

	atomic.StoreInt64(&a.h.lastOff, 700*mb) // a late read on the released handle moved it
	a.release()
	if got := atomic.LoadInt32(&ps.refCount); got != 1 {
		t.Errorf("refCount = %d after a second Release, want 1: C's reference was freed", got)
	}
	if got := atomic.LoadInt64(&ps.playerOff); got != pos {
		t.Errorf("playerOff = %d MiB after a second Release, want %d MiB: a released handle moved the resume position", got/mb, pos/mb)
	}
	if got := globalOpenTracker.CountByPath(path); got != open {
		t.Errorf("open count = %d after a second Release, want %d", got, open)
	}
	if _, live := app.activePumps.Load(path); !live {
		t.Error("the pump C still reads from was torn down")
	}
	_ = c
}

// A cancelled pump must not keep downloading: its reader is blocked in ReadAt, which only
// Interrupt unblocks, and the pump checks its context only between chunks.
func TestCancelledPumpInterruptsItsBlockedReader(t *testing.T) {
	const path = "/test/pump-cancel.mkv"
	sim := newPumpSim(t, path, 100*mb, 0, time.Millisecond)
	blocked := make(chan struct{}, 1)
	sim.fe.newRead = func() *fakeReader {
		r := &fakeReader{}
		r.readFn = func(_ int, _ []byte, _ int64) (int, error) {
			select {
			case blocked <- struct{}{}:
			default:
			}
			// Like the engine reader: blocked until Interrupt, with a bound so a missing
			// interrupt fails the test instead of hanging it.
			for deadline := time.Now().Add(1500 * time.Millisecond); r.interrupts.Load() == 0; {
				if time.Now().After(deadline) {
					return 0, errors.New("never interrupted")
				}
				time.Sleep(time.Millisecond)
			}
			return 0, errors.New("interrupted")
		}
		return r
	}
	a := sim.open("A")
	select {
	case <-blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("the pump never started reading")
	}

	sim.pumpState().cancel()
	waitUntil(t, "the cancelled pump's reader to be interrupted", func() bool { return sim.interrupts() > 0 })
	waitUntil(t, "the cancelled pump to end", func() bool { return !a.h.hasSlot.Load() })
	if got := len(sim.pumpReads()); got != 1 {
		t.Errorf("pump reads = %d, want the one that was in flight", got)
	}
}

// Resume-on-stop as it behaves today for a plain session: the position persisted at Release
// is where the player last read, after sequential reads and a seek, and the next open of the
// file inherits it.
func TestResumeOnStopPersistsTheLastPlayPosition(t *testing.T) {
	const path = "/test/resume-on-stop.mkv"
	sim := newPumpSim(t, path, 1000*mb, 100*mb, 5*time.Millisecond)
	a := sim.open("A")
	sim.settle()
	a.readRange(100*mb, 8*probeStep, probeStep)
	a.read(600*mb, probeStep) // a seek far beyond the read-ahead budget
	a.readRange(600*mb+probeStep, 7*probeStep, probeStep)
	last := 600*mb + 7*probeStep
	sim.settle()

	ps := sim.pumpState()
	if !a.h.isPrimaryHandle.Load() {
		t.Fatal("the only handle must stay primary through a seek")
	}
	a.release()
	if got := atomic.LoadInt64(&ps.playerOff); got != last {
		t.Errorf("playerOff after Release = %d, want the last read offset %d", got, last)
	}
	if got := sim.resumeOff(); got != last {
		t.Errorf("persisted resume position = %d, want %d", got, last)
	}
	if got := atomic.LoadInt32(&ps.refCount); got != 0 {
		t.Errorf("refCount after the only handle left = %d, want 0", got)
	}
	if _, live := app.activePumps.Load(path); !live {
		t.Error("healthy playback keeps its pump after the last handle leaves")
	}

	again := sim.open("A again")
	if !again.h.isPrimaryHandle.Load() || atomic.LoadInt64(&again.h.lastOff) != last {
		t.Errorf("the reopened handle: primary = %v, lastOff = %d, want primary at %d", again.h.isPrimaryHandle.Load(), again.h.lastOff, last)
	}
}
