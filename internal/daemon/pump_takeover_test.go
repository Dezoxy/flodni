package daemon

import (
	"sync/atomic"
	"testing"
	"time"
)

// Who may take the pump from a player that is watching: a probe never does, a genuine new
// player does. See mayTakeOverStreamingPrimary and activeStreamingPrimary (pump.go).

// streamFar reads n sequential probeStep reads from off: enough to earn primary by the
// sequential proof, the far one (primaryProofFor).
func streamFar(h *simHandle, off int64, n int) {
	h.readRange(off, int64(n)*probeStep, probeStep)
}

// A second viewer opens while A plays and before the play webhook confirms the session: it is
// the player of that session, so once it has streamed it takes the pump from A.
func TestASecondViewerOpenedBeforeTheConfirmationTakesThePump(t *testing.T) {
	sim := newPumpSim(t, "/test/takeover-viewer.mkv", probeSize, probeAt, probeFetch)
	a := sim.open("A")
	sim.settle()
	a.readRange(probeAt, 40*probeStep, probeStep)
	b := sim.open("B")
	a.read(probeAt+40*probeStep, probeStep) // A keeps reading after B opened
	sim.play()

	streamFar(b, 700*mb, 16)
	sim.settle()
	if got := sim.primaryOwner(); got != "[B]" {
		t.Errorf("primary owner = %s, want [B]: the new player must take over", got)
	}
	if target := sim.pumpTarget(); target < 700*mb {
		t.Errorf("pump target = %d MiB, want it at B's 700 MiB", target/mb)
	}
}

// streamAlongside has b read n sequential steps from off while the player a keeps reading:
// one read of a after every four of b's. next is a's next offset and advances.
func streamAlongside(a, b *simHandle, off int64, n int, next *int64) {
	for i := 0; i < n; i++ {
		b.read(off+int64(i)*probeStep, probeStep)
		if i%4 == 3 {
			a.read(*next, probeStep)
			*next += probeStep
		}
	}
}

// A second handle takes the pump once the primary has really gone quiet, however it opened:
// nothing watches through the primary any more, so a reopen or a second player may have it.
func TestAPlayerWhosePrimaryWentQuietTakesThePump(t *testing.T) {
	sim := newPumpSim(t, "/test/takeover-quiet.mkv", probeSize, probeAt, probeFetch)
	sim.shortWindow(2)
	a := sim.open("A")
	sim.settle()
	a.readRange(probeAt, 40*probeStep, probeStep)
	b := sim.open("B") // after the session started: not that session's player
	next := probeAt + 40*probeStep

	streamAlongside(a, b, 700*mb, 16, &next)
	if got := sim.primaryOwner(); got != "[A]" {
		t.Fatalf("primary owner = %s with A still reading, want [A]", got)
	}

	time.Sleep(2500 * time.Millisecond) // A goes quiet past the window, for real
	b.readRange(700*mb+16*probeStep, 16*probeStep, probeStep)
	if got := sim.primaryOwner(); got != "[B]" {
		t.Errorf("primary owner = %s, want [B] once A went quiet", got)
	}
}

// A buffered player goes quiet between bursts. Three seconds is inside the window: a probe
// that opens in the lull still defers once the player reads again, and the resume position
// stays the player's.
func TestAProbeArrivingWhileThePrimaryIsBrieflyQuietLeavesItAlone(t *testing.T) {
	sim := newPumpSim(t, "/test/takeover-brief-quiet.mkv", probeSize, probeAt, probeFetch)
	a := sim.open("A")
	sim.settle()
	a.readRange(probeAt, 40*probeStep, probeStep)
	if 3*time.Second >= primaryActiveWindow() {
		t.Fatalf("setup: 3 s is not inside the window (%v)", primaryActiveWindow())
	}
	time.Sleep(3 * time.Second)
	b := sim.open("B")
	next := probeAt + 40*probeStep
	a.read(next, probeStep) // the player's next burst
	next += probeStep

	streamAlongside(a, b, 700*mb, 16, &next)
	b.release()
	if got := sim.primaryOwner(); got != "[A]" {
		t.Errorf("primary owner = %s, want [A]: a probe took the primary from a briefly quiet player", got)
	}
	if got, want := sim.resumeOff(), next-probeStep; got != want {
		t.Errorf("persisted resume position = %d MiB, want A's %d MiB", got/mb, want/mb)
	}
	if got := atomic.LoadInt64(&sim.pumpState().playerOff); got != 0 {
		t.Errorf("shared player position = %d MiB, want it untouched", got/mb)
	}
	if sim.pumpTarget() >= 600*mb {
		t.Errorf("pump target = %d MiB: the pump followed the probe", sim.pumpTarget()/mb)
	}
}

// A probe's read is served and nothing else: the shared read-ahead cache keeps what the
// player's pump put there.
func TestAProbeReadDoesNotFillTheSharedCache(t *testing.T) {
	const path = "/test/probe-cache.mkv"
	sim := newPumpSim(t, path, probeSize, probeAt, probeFetch)
	a := sim.open("A")
	sim.settle()
	a.readRange(probeAt, 40*probeStep, probeStep)
	b := sim.open("B")
	a.read(probeAt+40*probeStep, probeStep) // the player keeps reading after B opened

	b.read(600*mb, probeStep)
	b.read(600*mb+probeStep, probeStep)
	if raCache.Covered(path, 600*mb, 600*mb+probeStep-1) || raCache.Exists(path, 600*mb) {
		t.Error("a probe's read filled the shared read-ahead cache")
	}
	if got := sim.demandFetches(); len(got) < 2 {
		t.Errorf("demand fetches = %v, want one per probe read", got)
	}
}

// A single viewer's seek costs what it cost before the probe rules: they never touch a
// handle that is the primary or has no watching primary to defer to.
func TestSingleViewerSeekFetchesAndInterruptsAsBefore(t *testing.T) {
	sim := newPumpSim(t, "/test/single-seek.mkv", probeSize, probeAt, probeFetch)
	a := sim.open("A")
	sim.settle()
	a.readRange(probeAt, 8*probeStep, probeStep)
	sim.settle()
	ahead0, block0, int0 := len(sim.fe.aheadCalls()), len(sim.fe.blockCalls()), sim.interrupts()

	a.read(700*mb, probeStep) // a seek far beyond the read-ahead budget
	a.readRange(700*mb+probeStep, 7*probeStep, probeStep)
	sim.settle()

	got := [3]int{len(sim.fe.aheadCalls()) - ahead0, len(sim.fe.blockCalls()) - block0, int(sim.interrupts() - int0)}
	if want := [3]int{singleSeekAhead, singleSeekBlock, singleSeekInterrupts}; got != want {
		t.Errorf("FetchAhead, FetchBlock and Interrupt calls for a single viewer's seek = %v, want %v (unchanged from before the probe rules)", got, want)
	}
	if atomic.LoadInt64(&a.h.lastOff) != 700*mb+7*probeStep || !a.h.isPrimaryHandle.Load() {
		t.Error("the seeking viewer must stay the primary at its new position")
	}
	if sim.pumpTarget() < 700*mb {
		t.Errorf("pump target = %d MiB, want it past the seek", sim.pumpTarget()/mb)
	}
}

const (
	singleSeekAhead      = 1
	singleSeekBlock      = 1
	singleSeekInterrupts = 1
)
