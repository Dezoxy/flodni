package daemon

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Review fixes for the pump-ahead-of-the-player work: the end-of-file boundary, the open
// count, who may own a pump, and the watchdog's concurrency.

// playTo reads sequentially from 'from' to 'to' in 1 MiB steps.
func playTo(h *simHandle, from, to int64) {
	for off := from; off < to; off += mb {
		h.read(off, int(mb))
	}
}

// A playback that reached the last lead has its pump end for good: the watchdog neither
// restarts it nor counts the session as one without a pump.
func TestTheWatchdogLeavesAPumpThatEndedWithThePlayerAtTheEnd(t *testing.T) {
	const path = "/test/wd-genuine-end.mkv"
	sim := newPumpSim(t, path, eofSize, eofSize-100*mb, 0)
	a := sim.open("A")
	sim.settle()
	playTo(a, eofSize-100*mb, eofSize-40*mb)
	waitUntil(t, "the pump to end at the end of the file", func() bool { return !pumpRegistered(path) })

	if body := gaugeBody(t); !strings.Contains(body, "flodni_playing_sessions_without_pump 0\n") {
		t.Errorf("gauge with the player in the last lead:\n%s", body)
	}
	restarts := pumpWatchdogRestarts.Load()
	w := newPumpWatchdog()
	t0 := time.Now()
	w.tick(t0)
	w.tick(t0.Add(pumpWatchdogGrace))
	w.tick(t0.Add(2 * pumpWatchdogGrace))
	if pumpRegistered(path) || pumpWatchdogRestarts.Load() != restarts {
		t.Error("the watchdog restarted a pump that ended at the end of the file")
	}
}

// A player crossing size - lead slowly: the pump reaches the last chunk while the player is
// still short of the genuine end. It never loops through re-anchors there.
func TestASlowPlayerCrossingTheLastLeadDoesNotLoopTheEOFReanchor(t *testing.T) {
	const path = "/test/eof-boundary.mkv"
	sim := newPumpSim(t, path, eofSize, eofSize-120*mb, 0)
	buf := captureLog()
	a := sim.open("A")
	sim.settle()
	playTo(a, eofSize-120*mb, eofSize-54*mb) // 946 MiB: inside the last chunk's reach, short of the end
	time.Sleep(1500 * time.Millisecond)
	if n := strings.Count(buf.String(), "re-anchoring"); n != 0 {
		t.Errorf("%d re-anchors while the player sat at 946 MiB of 1000, want none", n)
	}
}

// A creator whose pump ended and which starts a pump again (a read, then the watchdog twice)
// still owes one open count, and gives it back once on Release.
func TestACreatorThatRestartsItsPumpBalancesTheOpenCount(t *testing.T) {
	const path = "/test/creator-recreate.mkv"
	sim := newPumpSim(t, path, eofSize, eofAt, probeFetch)
	a := sim.open("A")
	sim.settle()
	a.readRange(eofAt, 8*probeStep, probeStep)
	sim.endPump()
	a.read(300*mb, probeStep) // a seek: the read misses the cache and starts a pump again
	if !pumpRegistered(path) {
		t.Fatal("the read did not start a pump: the scenario is not set up")
	}
	for lap := 0; lap < 2; lap++ { // and the watchdog, twice
		sim.endPump()
		w := newPumpWatchdog()
		t0 := time.Now()
		w.tick(t0)
		w.tick(t0.Add(pumpWatchdogGrace))
		if !pumpRegistered(path) {
			t.Fatalf("lap %d: the watchdog did not restart the pump", lap)
		}
	}
	if got := globalOpenTracker.CountByPath(path); got != 1 {
		t.Errorf("open count with the creator open = %d, want 1", got)
	}
	a.release()
	if got := globalOpenTracker.CountByPath(path); got != 0 {
		t.Errorf("open count after Release = %d, want 0", got)
	}
}

// An older handle that was the pump's creator but never played (a scan) and stays open
// does not take the primary from the player in a seek burst.
func TestAnOlderCreatorThatStaysOpenDoesNotTakeThePrimaryInASeekBurst(t *testing.T) {
	const path = "/test/burst-creator.mkv"
	sim := newPumpSim(t, path, eofSize, eofAt, probeFetch)
	c := sim.open("C") // the creator: primary at first, reads the header only
	c.read(0, probeStep)
	sim.settle()
	a := sim.open("A")
	a.readRange(eofAt, 40*probeStep, probeStep)
	sim.settle()
	if got := sim.primaryOwner(); got != "[A]" {
		t.Fatalf("primary owner = %s, want [A]: the scenario is not set up", got)
	}
	next := eofAt + 40*probeStep
	for i := int64(0); i < 24; i++ {
		c.read(548*mb+i*probeStep, probeStep)
		a.read(next, probeStep)
		next += probeStep
		if got := sim.primaryOwner(); got != "[A]" {
			t.Fatalf("after %d rounds the primary owner = %s, want [A]", i+1, got)
		}
	}
	if got, want := sim.resumeOff(), next-probeStep; got != want {
		t.Errorf("resume position = %d MiB, want the player's %d MiB", got/mb, want/mb)
	}
}

// The pump vanishes; a probe reads first and does not start one; the player's read does, on
// its own handle, and its seeks interrupt that pump's reader.
func TestAProbeDoesNotStartThePumpTheNextPlayerReadDoes(t *testing.T) {
	const path = "/test/probe-no-pump.mkv"
	sim := newPumpSim(t, path, eofSize, eofAt, probeFetch)
	a := sim.open("A")
	b := sim.open("B")
	p := sim.open("P")
	sim.settle()
	a.release()
	b.readRange(eofAt, 40*probeStep, probeStep)
	if got := sim.primaryOwner(); got != "[B]" {
		t.Fatalf("primary owner = %s, want [B]: the scenario is not set up", got)
	}
	sim.endPump()

	p.read(600*mb, probeStep) // the probe reads first
	if pumpRegistered(path) {
		t.Fatal("a probe started the pump")
	}
	b.read(300*mb, probeStep) // the player seeks: a cache miss
	if !pumpRegistered(path) {
		t.Fatal("no pump after the player's read")
	}
	if b.h.pumpState != sim.pumpState() {
		t.Error("the player's handle is not on the live pump")
	}
	before := sim.pumpState().reader.(*fakeReader).interrupts.Load()
	b.read(700*mb, probeStep) // and seeks again
	if sim.pumpState().reader.(*fakeReader).interrupts.Load() == before {
		t.Error("the player's seek did not interrupt the live pump's reader")
	}
}

// The watchdog starts a pump while the handle's own read does: one reference, one count.
func TestAWatchdogRestartRacingTheHandlesOwnReadMakesOneReference(t *testing.T) {
	const path = "/test/wd-race.mkv"
	sim := newPumpSim(t, path, eofSize, eofAt, 0)
	a := sim.open("A")
	sim.settle()
	a.readRange(eofAt, 8*probeStep, probeStep)
	for round := 0; round < 12; round++ {
		sim.endPump()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			w := newPumpWatchdog()
			t0 := time.Now()
			w.tick(t0)
			w.tick(t0.Add(pumpWatchdogGrace))
		}()
		go func() {
			defer wg.Done()
			a.read(300*mb+int64(round)*8*mb, probeStep) // a seek: a cache miss
		}()
		wg.Wait()
		if !pumpRegistered(path) {
			t.Fatalf("round %d: no pump", round)
		}
		if got := atomic.LoadInt32(&sim.pumpState().refCount); got != 1 {
			t.Fatalf("round %d: pump references = %d, want 1", round, got)
		}
		if got := globalOpenTracker.CountByPath(path); got != 1 {
			t.Fatalf("round %d: open count = %d, want 1", round, got)
		}
	}
}

// A scan's read of the index at the tail still sends its pump to the end of the file, where
// it ends: the hold is for a playback, not for a scan.
func TestAScansIndexReadStillEndsItsPump(t *testing.T) {
	const path = "/test/scan-tail.mkv"
	sim := newPumpSim(t, path, eofSize, 0, 0)
	ps := sim.playback()
	ps.mu.Lock()
	ps.IsHealthy, ps.ConfirmedAt, ps.LastSeekOff = false, time.Time{}, 0
	ps.mu.Unlock()
	s := sim.open("S")
	s.readRange(0, 4*probeStep, probeStep)
	s.read(eofSize-10*mb, probeStep)
	s.read(eofSize-10*mb+probeStep, probeStep)
	waitUntil(t, "the scan's pump to end at the end of the file", func() bool { return !pumpRegistered(path) })
}
