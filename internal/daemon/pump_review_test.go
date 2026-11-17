package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// Review fixes for the probe rules: the session-start stamp, older players, pump creators,
// reopen-to-seek, the orphan sweep and the singleflight. Each scenario interleaves the
// player's reads with the other handle's, as a live player's do.

// Analysis opens mid-session; the user pauses and resumes. The resume webhook moves
// ConfirmedAt, not the session start, so the analysis stays a probe: it never takes the
// primary, however it reads, and the resume position stays the player's.
func TestAnAnalysisOpenedMidSessionStaysAProbeThroughPauseAndResume(t *testing.T) {
	sim := newPumpSim(t, "/test/review-resume.mkv", probeSize, probeAt, probeFetch)
	a := sim.open("A")
	sim.settle()
	a.readRange(probeAt, 40*probeStep, probeStep)
	b := sim.open("B")
	next := probeAt + 40*probeStep
	a.read(next, probeStep)
	next += probeStep

	sim.resume() // pause, resume
	streamAlongside(a, b, 700*mb, 24, &next)
	b.read(300*mb, probeStep) // and a seek
	a.read(next, probeStep)
	next += probeStep
	b.readRange(300*mb+probeStep, 8*probeStep, probeStep)
	b.release()

	if got := sim.primaryOwner(); got != "[A]" {
		t.Errorf("primary owner = %s, want [A]: the resume webhook made the analysis a successor", got)
	}
	if got, want := sim.resumeOff(), next-probeStep; got != want {
		t.Errorf("persisted resume position = %d MiB, want A's %d MiB", got/mb, want/mb)
	}
	if got := atomic.LoadInt64(&sim.pumpState().playerOff); got != 0 {
		t.Errorf("shared player position = %d MiB after B's Release, want it untouched", got/mb)
	}
}

// A pauses past the window, an analysis streams and is promoted (nothing watches), A resumes:
// A, the older handle, is not deferred to the later one and retakes the primary and the
// resume position on its normal rules.
func TestAnOlderPlayerRetakesThePrimaryWhenItResumes(t *testing.T) {
	sim := newPumpSim(t, "/test/review-older.mkv", probeSize, probeAt, probeFetch)
	sim.shortWindow(1)
	a := sim.open("A")
	sim.settle()
	a.readRange(probeAt, 40*probeStep, probeStep)
	time.Sleep(1500 * time.Millisecond) // A pauses past the window, for real
	b := sim.open("B")
	streamFar(b, 700*mb, 16)
	if got := sim.primaryOwner(); got != "[B]" {
		t.Fatalf("setup: primary owner = %s, want [B] while A is paused", got)
	}

	next := probeAt + 40*probeStep
	a.readRange(next, 16*probeStep, probeStep) // A resumes, B keeps reading in between
	last := next + 15*probeStep
	b.read(700*mb+16*probeStep, probeStep)
	a.read(last+probeStep, probeStep)
	last += probeStep
	if got := sim.primaryOwner(); got != "[A]" {
		t.Errorf("primary owner = %s, want [A]: the older player was served as a probe", got)
	}
	ps := sim.pumpState()
	a.release()
	if got := atomic.LoadInt64(&ps.playerOff); got != last {
		t.Errorf("player position after A's Release = %d MiB, want A's %d MiB", got/mb, last/mb)
	}
	if got := sim.resumeOff(); got != last {
		t.Errorf("persisted resume position = %d MiB, want A's %d MiB", got/mb, last/mb)
	}
}

// B opens while A, the player, is primary and reading but no pump is registered (it ended at
// EOF): B becomes the pump's creator, but never its primary.
func TestAHandleOpenedWhenNoPumpIsRegisteredIsNotMadePrimaryAsCreator(t *testing.T) {
	sim := newPumpSim(t, "/test/review-creator.mkv", probeSize, probeAt, probeFetch)
	a := sim.open("A")
	sim.settle()
	a.readRange(probeAt, 12*probeStep, probeStep)
	sim.endPump()
	if a.h.hasSlot.Load() {
		t.Fatal("setup: A must have lost its pump")
	}

	b := sim.open("B")
	a.read(probeAt+12*probeStep, probeStep) // the player keeps reading
	if b.h.isPrimaryHandle.Load() {
		t.Error("B became primary as the pump's creator while A is watching")
	}
	if got := sim.primaryOwner(); got != "[A]" {
		t.Errorf("primary owner = %s, want [A]", got)
	}
}

// Reopen-to-seek with the old handle lingering: A stays open but silent; the new handle C
// seeks and reads. C is the player, not a probe: it gets the read-ahead window and the
// primary at once.
func TestAReopenWhileTheOldHandleLingersSilentTakesThePrimary(t *testing.T) {
	const path = "/test/review-reopen.mkv"
	sim := newPumpSim(t, path, probeSize, probeAt, probeFetch)
	a := sim.open("A")
	sim.settle()
	a.readRange(probeAt, 40*probeStep, probeStep)
	c := sim.open("C")

	c.read(0, probeStep) // the header
	c.read(probeStep, probeStep)
	c.read(700*mb, probeStep) // the seek
	if got := sim.primaryOwner(); got != "[C]" {
		t.Errorf("primary owner = %s, want [C] at once", got)
	}
	if !raCache.Covered(path, 700*mb+probeStep, 700*mb+2*mb-1) {
		t.Error("C's read got no read-ahead window: it was served as a probe")
	}
}

// The orphan sweep drops a handle's reference without taking the latch of the kernel's own
// Release. A late read re-attaches; the kernel's Release then drops that reference, once.
func TestSweepThenALateReadThenTheKernelReleaseLeavesNothingHeld(t *testing.T) {
	const path = "/test/review-sweep.mkv"
	sim := newPumpSim(t, path, probeSize, probeAt, probeFetch)
	a := sim.open("A")
	sim.settle()
	a.readRange(probeAt, 8*probeStep, probeStep)
	if got := globalOpenTracker.CountByPath(path); got != 1 {
		t.Fatalf("setup: open count = %d, want 1", got)
	}

	a.h.sweepOrphan()
	sim.endPump()             // the pump exits: the late read finds none
	a.read(800*mb, probeStep) // a miss: it needs the engine, so it needs a pump
	if _, live := app.activePumps.Load(path); !live {
		t.Fatal("setup: the late read must have started a pump")
	}
	ps := sim.pumpState()
	a.release()

	if got := atomic.LoadInt32(&ps.refCount); got != 0 {
		t.Errorf("refCount = %d after the kernel's Release, want 0: the pump stays pinned", got)
	}
	if got := globalOpenTracker.CountByPath(path); got != 0 {
		t.Errorf("open count = %d after the kernel's Release, want 0", got)
	}
}

// A probe does not lead the singleflight for a chunk: its fetch is uncached, so a player miss
// in the same chunk waiting behind it would wait for nothing.
func TestAPlayerMissDoesNotWaitBehindAProbeFetchOfTheSameChunk(t *testing.T) {
	sim := newPumpSim(t, "/test/review-flight.mkv", probeSize, probeAt, probeFetch)
	a := sim.open("A")
	sim.settle()
	a.readRange(probeAt, 40*probeStep, probeStep)
	b := sim.open("B")
	next := probeAt + 40*probeStep
	a.read(next, probeStep)

	gate := make(chan struct{})
	sim.fe.aheadFn = func(_ int, off int64, buf, dest []byte, onFill func(int, bool, error)) (int, error) {
		if off == 600*mb { // the probe's fetch hangs; the player's does not
			<-gate
		}
		return fillAhead(off, buf, dest, onFill)
	}
	sim.fe.blockFn = func(_ int, off int64, p []byte) (int, error) {
		if off >= 600*mb && off < 604*mb { // the player's warm landing must not rescue its wait
			<-gate
		}
		fillPattern(p, off)
		return len(p), nil
	}
	pump := sim.fe.readers[0] // nor may the pump, jumping to the player's seek, fill the chunk
	pump.mu.Lock()
	pump.readFn = func(_ int, p []byte, off int64) (int, error) {
		if off >= 600*mb && off < 604*mb {
			<-gate
		}
		fillPattern(p, off)
		return len(p), nil
	}
	pump.mu.Unlock()
	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		b.read(600*mb, probeStep)
	}()
	waitUntil(t, "the probe's fetch to be in flight", func() bool {
		return len(sim.fe.aheadCalls()) > 0 && sim.fe.aheadCalls()[len(sim.fe.aheadCalls())-1].off == 600*mb
	})

	playerDone := make(chan struct{})
	go func() { // the player seeks into the same chunk
		defer close(playerDone)
		a.h.Read(context.Background(), make([]byte, probeStep), 601*mb)
	}()
	defer func() { close(gate); <-probeDone; <-playerDone }()
	select {
	case <-playerDone:
	case <-time.After(1500 * time.Millisecond):
		t.Error("the player's miss waited behind the probe's fetch")
	}
	select {
	case <-probeDone:
		t.Error("setup: the probe's fetch was not in flight")
	default:
	}
}
