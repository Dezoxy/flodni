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
	sim.confirm()

	streamFar(b, 700*mb, 16)
	sim.settle()
	if got := sim.primaryOwner(); got != "[B]" {
		t.Errorf("primary owner = %s, want [B]: the new player must take over", got)
	}
	if target := sim.pumpTarget(); target < 700*mb {
		t.Errorf("pump target = %d MiB, want it at B's 700 MiB", target/mb)
	}
}

// The same viewer reopening, or a second one, once the primary went quiet: nothing watches
// through it any more, so the next handle that streams takes the pump whenever it opened.
func TestAPlayerWhosePrimaryWentQuietTakesThePump(t *testing.T) {
	sim := newPumpSim(t, "/test/takeover-quiet.mkv", probeSize, probeAt, probeFetch)
	a := sim.open("A")
	sim.settle()
	a.readRange(probeAt, 40*probeStep, probeStep)
	b := sim.open("B") // after the confirmation: not the player of that session

	streamFar(b, 700*mb, 16)
	if got := sim.primaryOwner(); got != "[A]" {
		t.Fatalf("primary owner = %s with A still reading, want [A]", got)
	}

	a.h.lastRead.Store(time.Now().Add(-2 * primaryActiveWindow).UnixNano())
	b.readRange(700*mb+16*probeStep, 16*probeStep, probeStep)
	if got := sim.primaryOwner(); got != "[B]" {
		t.Errorf("primary owner = %s, want [B] once A went quiet", got)
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
