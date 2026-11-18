package daemon

import (
	"sync/atomic"
	"testing"
	"time"
)

// The probe-during-playback scenario (media analysis on a file somebody is watching): session
// A reads sequentially at its play position, 40% into a large file; a second open B reads the
// header, then a region further on, then closes. A's pump target, served offsets, latency and
// resume position must not notice B.
//
// The scenario runs once per probe region; every property is its own subtest.

const (
	probeSize  = 1000 * mb
	probeAt    = 400 * mb // 40%
	probeStep  = 256 * 1024
	probeFetch = 100 * time.Millisecond // engine fetch latency
)

type probeRun struct {
	sim *pumpSim
	a   *simHandle
	b   *simHandle

	aBaselineFetches int // engine fetches A's reads made before B opened
	aLastBeforeB     int64
	pumpReadsBeforeB int
	fetchesBeforeB   int
	interruptsBefore int32

	primaryDuringProbe string
	pumpAfterProbe     []readCall // pump reads since B opened
	aAfterProbe        []simRead  // A's reads since B opened
	demandAfterB       []int64    // engine demand fetches since B opened
	pumpState          *NativePumpState
	playerOffAtRelease int64 // the shared state's position after A's Release
	resumeAtRelease    int64 // PlaybackState.LastSeekOff after A's Release
	aLastOff           int64 // A's last read offset
	playerOffAfterB    int64 // the shared state's position once B is released
	resumeAfterB       int64 // PlaybackState.LastSeekOff once B is released
	aLastBeforeRelease int64 // A's last read offset when B was released
	nextCachedBefore   bool  // A's next chunk was in the read-ahead cache before B opened
	nextCachedAtProbe  bool  // and still was when A read it during B's probe
}

// runProbeScenario plays A, then lets B read the header and probeLen bytes from probeFrom
// (A keeps reading in between), closes B and stops A after one more read.
func runProbeScenario(t *testing.T, path string, probeFrom, probeLen int64) *probeRun {
	t.Helper()
	sim := newPumpSim(t, path, probeSize, probeAt, probeFetch)
	run := &probeRun{sim: sim}

	run.a = sim.open("A")
	sim.settle() // the pump fills its lead past A
	run.a.readRange(probeAt, 40*probeStep, probeStep)
	for _, r := range run.a.served() {
		run.aBaselineFetches += r.fetched
	}
	run.aLastBeforeB = probeAt + 39*probeStep
	run.fetchesBeforeB = len(sim.demandFetches())
	run.interruptsBefore = sim.interrupts()
	sim.settle()
	run.pumpReadsBeforeB = len(sim.pumpReads())
	nextChunk := probeAt + 40*probeStep
	run.nextCachedBefore = raCache.Covered(path, nextChunk, nextChunk+probeStep-1)

	run.b = sim.open("B")
	aNext := probeAt + 40*probeStep
	run.a.read(aNext, probeStep) // the player keeps reading: it reads after B opened
	aNext += probeStep
	run.b.readRange(0, 1*mb, probeStep)
	for i := int64(0); i < 4; i++ {
		run.b.readRange(probeFrom+i*(probeLen/4), probeLen/4, probeStep)
		if i == 0 {
			run.nextCachedAtProbe = raCache.Covered(path, aNext, aNext+probeStep-1)
		}
		run.a.read(aNext, probeStep)
		aNext += probeStep
	}
	sim.settle()
	run.primaryDuringProbe = sim.primaryOwner()
	run.b.release()
	run.aLastBeforeRelease = aNext - probeStep
	run.playerOffAfterB = atomic.LoadInt64(&sim.pumpState().playerOff)
	run.resumeAfterB = sim.resumeOff()
	sim.settle()

	run.a.read(aNext, probeStep) // A's next read once B is gone
	run.aAfterProbe = run.a.served()[40:]
	run.aLastOff = aNext
	run.pumpAfterProbe = sim.pumpReads()[run.pumpReadsBeforeB:]
	run.demandAfterB = sim.demandFetches()[run.fetchesBeforeB:]
	run.pumpState = sim.pumpState()
	run.a.release()
	run.playerOffAtRelease = atomic.LoadInt64(&run.pumpState.playerOff)
	run.resumeAtRelease = sim.resumeOff()
	return run
}

// checkProbeProperties asserts what A must keep.
func checkProbeProperties(t *testing.T, run *probeRun) {
	sim := run.sim
	budget := int64(gc().ReadAheadBudget)
	chunk := int64(4 * mb)

	if run.aBaselineFetches != 0 {
		t.Fatalf("A's baseline reads made %d engine fetches: the scenario is not set up", run.aBaselineFetches)
	}
	if run.aLastBeforeB != probeAt+39*probeStep || sim.primaryOwner() == "" {
		t.Fatal("the scenario is not set up")
	}
	sub := func(name string, f func(t *testing.T)) {
		t.Run(name, f)
	}

	sub("the pump keeps fetching ahead of A and never at B's offsets", func(t *testing.T) {
		lo, hi := probeAt-chunk, run.aLastOff+budget+chunk
		for _, r := range run.pumpAfterProbe {
			if r.off < lo || r.off >= hi {
				t.Errorf("pump read at %d MiB, outside A's window [%d, %d) MiB", r.off/mb, lo/mb, hi/mb)
				return
			}
		}
	})
	sub("the pump reader is not interrupted by B", func(t *testing.T) {
		if got := sim.interrupts() - run.interruptsBefore; got != 0 {
			t.Errorf("the pump reader was interrupted %d times while B probed", got)
		}
	})
	sub("A stays the primary stream", func(t *testing.T) {
		if run.primaryDuringProbe != "[A]" {
			t.Errorf("primary owner during the probe = %s, want [A]", run.primaryDuringProbe)
		}
	})
	sub("A's reads need no engine fetch because of B", func(t *testing.T) {
		// B's own reads fetch the header window and the probed windows; anything else is A's.
		for _, off := range run.demandAfterB {
			if off >= 8*mb && off < probeSize-18*mb && !inRange(off, run.b, 0) {
				t.Errorf("a demand fetch at %d MiB: A's read went to the engine (A's next chunk cached before B: %v, when A read it: %v)", off/mb, run.nextCachedBefore, run.nextCachedAtProbe)
			}
		}
	})
	sub("A's reads cause no engine fetch after B started", func(t *testing.T) {
		for _, r := range run.aAfterProbe {
			if r.fetched != 0 {
				t.Errorf("A's read at %d MiB made %d engine fetches after B started, baseline %d", r.off/mb, r.fetched, run.aBaselineFetches)
			}
		}
	})
	sub("B's reads and Release never persist a resume position", func(t *testing.T) {
		if run.playerOffAfterB != 0 {
			t.Errorf("B's Release moved the shared player position to %d MiB", run.playerOffAfterB/mb)
		}
		if run.resumeAfterB != run.aLastBeforeRelease {
			t.Errorf("the persisted resume position after B = %d MiB, want A's %d MiB", run.resumeAfterB/mb, run.aLastBeforeRelease/mb)
		}
	})
	sub("A's resume position on Release is A's own", func(t *testing.T) {
		if run.playerOffAtRelease != run.aLastOff {
			t.Errorf("the pump's player position after A's Release = %d MiB, want A's %d MiB", run.playerOffAtRelease/mb, run.aLastOff/mb)
		}
		if run.resumeAtRelease != run.aLastOff {
			t.Errorf("the persisted resume position = %d MiB, want A's %d MiB", run.resumeAtRelease/mb, run.aLastOff/mb)
		}
	})
	sub("A's bytes and offsets are served unchanged", func(t *testing.T) {
		// The simulator already fails any wrong byte; this pins the served offsets.
		for i, r := range run.a.served() {
			if want := probeAt + int64(i)*probeStep; r.off != want || r.n != probeStep {
				t.Fatalf("read %d at %d (%d bytes), want %d", i, r.off, r.n, want)
			}
		}
	})
}

// inRange reports whether off lies in a window one of sh's own reads fetched. The probe
// reads are sequential, so the window of a read at o is [o, o+ReadAheadInitial).
func inRange(off int64, sh *simHandle, _ int64) bool {
	for _, r := range sh.served() {
		if off >= r.off && off < r.off+int64(gc().ReadAheadInitial) {
			return true
		}
	}
	return false
}

// B reads the header, then the last 10 MB: the media-analysis pattern.
func TestProbeOfTheTailDuringPlaybackLeavesTheWatchingSessionAlone(t *testing.T) {
	run := runProbeScenario(t, "/test/probe-tail.mkv", probeSize-10*mb, 10*mb)
	checkProbeProperties(t, run)
}

// B reads the header, then 10 MB in the middle of the file, beyond the SSD tail window the
// seek gate recognises.
func TestProbeOfTheMiddleDuringPlaybackLeavesTheWatchingSessionAlone(t *testing.T) {
	run := runProbeScenario(t, "/test/probe-middle.mkv", 700*mb, 10*mb)
	checkProbeProperties(t, run)
}
