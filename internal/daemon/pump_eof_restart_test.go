package daemon

import (
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A player reads the file's index at the tail (the Cues of an MKV) and goes back to where it
// was playing. The pump must stay with the player through that, and a pump that did end must
// come back with the player's next read. Seek bursts of probes must not drag it away either.
// Every scenario interleaves several handles through the real Read, Release and pump (pumpSim).

const (
	eofSize = probeSize // 1000 MiB: the tail zone is its last 64 MiB
	eofAt   = 108 * mb  // where the player plays
)

func pumpRegistered(path string) bool {
	_, ok := app.activePumps.Load(path)
	return ok
}

// pumpReadBeyond is the first pump read at or past limit, or -1.
func pumpReadBeyond(sim *pumpSim, limit int64) int64 {
	for _, r := range sim.pumpReads() {
		if r.off >= limit {
			return r.off
		}
	}
	return -1
}

// The primary reads the index at the tail in the middle of playing, with a probe reading the
// header between its reads, and returns to its position: the pump never leaves the player.
func TestThePumpStaysWithThePlayerWhileItReadsTheIndexAtTheTail(t *testing.T) {
	const path = "/test/eof-index.mkv"
	sim := newPumpSim(t, path, eofSize, eofAt, probeFetch)
	a := sim.open("A")
	sim.settle()
	a.readRange(eofAt, 40*probeStep, probeStep)
	next := eofAt + 40*probeStep
	sim.settle()

	p := sim.open("P") // a probe opens during playback and reads the header
	p.readRange(0, 1*mb, probeStep)
	a.read(eofSize-10*mb, probeStep) // the player reads its index
	p.read(2*mb, probeStep)
	a.read(eofSize-10*mb+probeStep, probeStep)
	sim.settle()
	a.read(next, probeStep) // and is back at its position
	next += probeStep
	a.readRange(next, 8*probeStep, probeStep)
	next += 8 * probeStep
	sim.settle()

	if off := pumpReadBeyond(sim, eofSize-tailProbeZoneSize(eofSize)); off >= 0 {
		t.Errorf("the pump read at %d MiB, in the tail zone: it followed the index read", off/mb)
	}
	if !pumpRegistered(path) {
		t.Error("the pump ended while the player was far from the end of the file")
	}
	if got := sim.pumpTarget(); got < eofAt || got > next+int64(gc().ReadAheadBudget)+8*mb {
		t.Errorf("pump target = %d MiB, want it ahead of the player at %d MiB", got/mb, next/mb)
	}
	if got := sim.resumeOff(); got != next-probeStep {
		t.Errorf("resume position = %d MiB, want the player's last read %d MiB", got/mb, (next-probeStep)/mb)
	}
	a.release()
	if got := atomic.LoadInt64(&sim.pumpState().playerOff); got != next-probeStep {
		t.Errorf("player position kept for the next handle = %d MiB, want %d MiB", got/mb, (next-probeStep)/mb)
	}
}

// The player reads the tail like a player (sequentially, long enough to be taken for one) and
// the pump, which followed it, reaches the end and ends. The player then returns to its
// position on a handle that attached to that pump: its next read starts a pump again.
func TestAPumpThatEndedAtTheEndOfTheFileIsStartedAgainByTheNextRead(t *testing.T) {
	const path = "/test/eof-restart.mkv"
	sim := newPumpSim(t, path, eofSize, eofAt, probeFetch)
	buf := captureLog()
	a := sim.open("A")
	b := sim.open("B") // attaches to A's pump
	sim.settle()
	a.release()
	b.readRange(eofAt, 40*probeStep, probeStep)
	sim.settle()
	if got := sim.primaryOwner(); got != "[B]" {
		t.Fatalf("primary owner = %s, want [B]: the scenario is not set up", got)
	}

	p := sim.open("P")
	p.readRange(0, 1*mb, probeStep)
	b.readRange(eofSize-30*mb, 8*probeStep, probeStep) // the tail, read like a player
	p.read(2*mb, probeStep)
	waitUntil(t, "the pump to end at the end of the file", func() bool {
		return !pumpRegistered(path)
	})
	if !strings.Contains(buf.String(), v306EOF) {
		t.Fatalf("the pump ended without the end-of-file line:\n%s", buf.String())
	}

	next := eofAt + 40*probeStep
	b.read(next, probeStep) // the player is back
	if !pumpRegistered(path) {
		t.Fatal("no pump after the player's read: it ran on demand fetches until another handle opened")
	}
	b.readRange(next+probeStep, 8*probeStep, probeStep)
	sim.settle()
	if got := sim.pumpTarget(); got < next || got > next+int64(gc().ReadAheadBudget)+8*mb {
		t.Errorf("pump target = %d MiB, want it ahead of the player at %d MiB", got/mb, next/mb)
	}
	if got, want := sim.resumeOff(), next+8*probeStep; got != want {
		t.Errorf("resume position = %d MiB, want %d MiB", got/mb, want/mb)
	}
	b.release()
	p.release()
	if got := globalOpenTracker.CountByPath(path); got != 0 {
		t.Errorf("open count after every handle was released = %d, want 0", got)
	}
}

// A pump that ends at the end of the file while the player is at the end too is a genuine end.
func TestAPumpEndingWithThePlayerAtTheEndIsAGenuineEnd(t *testing.T) {
	const path = "/test/eof-genuine.mkv"
	sim := newPumpSim(t, path, eofSize, eofSize-80*mb, 0)
	buf := captureLog()
	a := sim.open("A")
	sim.settle()
	a.readRange(eofSize-80*mb, 80*mb, 1*mb) // the last 80 MiB, sequentially
	waitUntil(t, "the pump's last log line", func() bool { return strings.Contains(buf.String(), pumpEnded) })
	out := buf.String()
	if !strings.Contains(out, v306EOF) || strings.Contains(out, "re-anchoring") {
		t.Fatalf("a pump ending with the player at the end must log the end and not re-anchor:\n%s", out)
	}
}

// The same burst, opened after the player's handle: those handles defer to it (the rule from
// the probe work, pinned here for a burst).
func TestASeekBurstOfLaterProbesLeavesThePrimaryAndThePumpWithThePlayer(t *testing.T) {
	const path = "/test/burst-later.mkv"
	sim := newPumpSim(t, path, eofSize, eofAt, probeFetch)
	a := sim.open("A")
	sim.settle()
	a.readRange(eofAt, 40*probeStep, probeStep)
	x1, x2, x3 := sim.open("X1"), sim.open("X2"), sim.open("X3")

	next := eofAt + 40*probeStep
	burst := []*simHandle{x1, x2, x3}
	offs := []int64{548 * mb, 700 * mb, 300 * mb}
	for i := int64(0); i < 24; i++ {
		for j, x := range burst {
			x.read(offs[j]+i*probeStep, probeStep)
		}
		a.read(next, probeStep)
		next += probeStep
	}
	sim.settle()
	if got := sim.primaryOwner(); got != "[A]" {
		t.Errorf("primary owner = %s, want [A]", got)
	}
	if got := sim.pumpTarget(); got > next+int64(gc().ReadAheadBudget)+8*mb {
		t.Errorf("pump target = %d MiB: it followed the burst, the player is at %d MiB", got/mb, next/mb)
	}
}

// setReaderFailure makes every pump reader created so far fail its reads (nil restores them):
// the engine side of a torrent that was dropped and added again under a live pump.
func setReaderFailure(sim *pumpSim, fail bool) {
	sim.fe.mu.Lock()
	readers := append([]*fakeReader(nil), sim.fe.readers...)
	sim.fe.mu.Unlock()
	for _, r := range readers {
		r.mu.Lock()
		if fail {
			r.readFn = func(int, []byte, int64) (int, error) { return 0, io.ErrClosedPipe }
		} else {
			r.readFn = nil
		}
		r.mu.Unlock()
	}
}

// pumpFollows waits until the pump has read within the read-ahead window past the player.
// The pump loop sleeps 100-200 ms between checks and a failed read retries after 200 ms, so
// a pump that is following shows it inside 3 s (waitUntil's limit).
func pumpFollows(t *testing.T, sim *pumpSim, player int64) {
	t.Helper()
	waitUntil(t, "the pump to read ahead of the player", func() bool {
		reads := sim.pumpReads()
		for i := len(reads) - 1; i >= 0 && i >= len(reads)-4; i-- {
			if r := reads[i]; r.off >= player-4*mb && r.off <= player+int64(gc().ReadAheadBudget)+8*mb {
				return true
			}
		}
		return false
	})
}

// The field sequence. Playback runs on A; the settings rebuild drops the engine torrent and
// adds it again (the pump's reads fail meanwhile and A is closed); the resumed session opens
// B, which attaches to the surviving pump; the player seeks and its probes open handles that
// stream far away; then the player reads its index at the tail and comes back.
func TestRebuildThenResumeThenSeekBurstThenTailReadKeepsThePumpOnThePlayer(t *testing.T) {
	const path = "/test/field-regression.mkv"
	sim := newPumpSim(t, path, eofSize, eofAt, probeFetch)
	a := sim.open("A")
	sim.settle()
	a.readRange(eofAt, 40*probeStep, probeStep)
	sim.settle()
	pump := sim.pumpState()

	setReaderFailure(sim, true) // dropAllTorrent
	a.release()                 // the rebuild closes the handles
	time.Sleep(500 * time.Millisecond)
	setReaderFailure(sim, false) // the torrent is added again
	if sim.pumpState() != pump {
		t.Fatal("the pump did not survive the rebuild: the scenario is not set up")
	}

	sim.resume() // the session resumes
	b := sim.open("B")
	next := eofAt + 40*probeStep
	b.readRange(next, 8*probeStep, probeStep)
	next += 8 * probeStep
	if got := sim.primaryOwner(); got != "[B]" {
		t.Fatalf("primary owner after the resume = %s, want [B]", got)
	}
	pumpFollows(t, sim, next)

	// The seek burst: the player jumps and its probes (opened after B) stream far away.
	x1, x2, x3 := sim.open("X1"), sim.open("X2"), sim.open("X3")
	burst := []*simHandle{x1, x2, x3}
	offs := []int64{2 * mb, 548 * mb, 700 * mb}
	for i := int64(0); i < 24; i++ {
		for j, x := range burst {
			x.read(offs[j]+i*probeStep, probeStep)
		}
		b.read(next, probeStep)
		next += probeStep
	}
	if got := sim.primaryOwner(); got != "[B]" {
		t.Fatalf("primary owner after the burst = %s, want [B]", got)
	}
	pumpFollows(t, sim, next)

	// The player reads its index at the tail and comes back.
	b.read(eofSize-10*mb, probeStep)
	b.read(eofSize-10*mb+probeStep, probeStep)
	sim.settle()
	b.read(next, probeStep)
	next += probeStep
	b.readRange(next, 8*probeStep, probeStep)
	next += 8 * probeStep
	sim.settle()

	if sim.pumpState() != pump {
		t.Error("the pump ended or was replaced while the playback continued")
	}
	if got := sim.primaryOwner(); got != "[B]" {
		t.Errorf("primary owner = %s, want [B]", got)
	}
	if off := pumpReadBeyond(sim, eofSize-tailProbeZoneSize(eofSize)); off >= 0 {
		t.Errorf("the pump read at %d MiB, in the tail zone", off/mb)
	}
	pumpFollows(t, sim, next)
	if got, want := sim.resumeOff(), next-probeStep; got != want {
		t.Errorf("resume position = %d MiB, want the player's %d MiB", got/mb, want/mb)
	}
	b.release()
	if got := atomic.LoadInt64(&pump.playerOff); got != next-probeStep {
		t.Errorf("player position kept = %d MiB, want %d MiB", got/mb, (next-probeStep)/mb)
	}
}

// A seek burst of older probes: handles opened before the player, never primary, stream far
// away one after the other while the player (a later handle) reads. An older handle may
// retake the primary only if it was the primary before (a paused player returning).
func TestASeekBurstOfOlderNeverPrimaryProbesLeavesThePrimaryAndThePumpWithThePlayer(t *testing.T) {
	const path = "/test/burst-older.mkv"
	sim := newPumpSim(t, path, eofSize, eofAt, probeFetch)
	c := sim.open("C") // the pump's creator, the only handle that was ever primary before the player
	x1, x2, x3 := sim.open("X1"), sim.open("X2"), sim.open("X3")
	x1.read(0, probeStep)
	sim.settle()
	c.release()
	a := sim.open("A")
	a.readRange(eofAt, 40*probeStep, probeStep)
	sim.settle()
	if got := sim.primaryOwner(); got != "[A]" {
		t.Fatalf("primary owner = %s, want [A]: the scenario is not set up", got)
	}

	next := eofAt + 40*probeStep
	burst := []*simHandle{x1, x2, x3}
	offs := []int64{548 * mb, 700 * mb, 300 * mb}
	for i := int64(0); i < 24; i++ {
		for j, x := range burst {
			x.read(offs[j]+i*probeStep, probeStep)
		}
		a.read(next, probeStep)
		next += probeStep
		if got := sim.primaryOwner(); got != "[A]" {
			t.Fatalf("after %d burst rounds the primary owner = %s, want [A]", i+1, got)
		}
	}
	sim.settle()
	if got := sim.pumpTarget(); got > next+int64(gc().ReadAheadBudget)+8*mb {
		t.Errorf("pump target = %d MiB: it followed the burst, the player is at %d MiB", got/mb, next/mb)
	}
	if got, want := sim.resumeOff(), next-probeStep; got != want {
		t.Errorf("resume position = %d MiB, want the player's %d MiB", got/mb, want/mb)
	}
}

// A pump creator whose pump ended gives its open count back on Release, once.
func TestAPumpCreatorWhosePumpEndedBalancesTheOpenCountOnRelease(t *testing.T) {
	const path = "/test/creator-tracker.mkv"
	sim := newPumpSim(t, path, eofSize, eofAt, 0)
	a := sim.open("A")
	sim.settle()
	if got := globalOpenTracker.CountByPath(path); got != 1 {
		t.Fatalf("open count with A open = %d, want 1: the scenario is not set up", got)
	}
	sim.endPump()
	a.release()
	if got := globalOpenTracker.CountByPath(path); got != 0 {
		t.Errorf("open count after the creator's pump ended and it was released = %d, want 0", got)
	}
	a.release() // idempotent
	if got := globalOpenTracker.CountByPath(path); got != 0 {
		t.Errorf("open count after a second Release = %d, want 0", got)
	}
}
