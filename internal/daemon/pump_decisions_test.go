package daemon

import (
	"flodni/internal/config"
	"flodni/internal/flog"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

const (
	mb = int64(1 << 20)
	gb = int64(1 << 30)
)

// useTestGlobals installs a config and a silent logger for tests that reach the package
// globals, and restores both on cleanup. Tests that call it must not run in parallel.
func useTestGlobals(t *testing.T) {
	t.Helper()
	prevCfg, prevLog := globalConfig.Load(), logger
	globalConfig.Store(&config.Config{
		ReadAheadBudget:      256 * mb,
		ReadAheadBase:        16 * mb,
		StreamingThreshold:   128 * 1024,
		SequentialTolerance:  512 * 1024,
		WarmStartIdleSeconds: 6,
		PumpOnOpen:           true,
		// Fill pacing as a default config has it, so the pump suites run with it on.
		FillPaceEnabled:  true,
		FillPaceMbit:     500,
		FillPaceMarginMB: 128,
	})
	logger = flog.NewText(io.Discard)
	t.Cleanup(func() {
		globalConfig.Store(prevCfg)
		logger = prevLog
	})
}

func TestShouldInterruptForSeek(t *testing.T) {
	t.Parallel()
	const budget = 256 * mb
	cases := []struct {
		name         string
		prevOff, off int64
		want         bool
	}{
		{"new handle (prevOff 0) never seeks", 0, 5 * gb, false},
		{"unset prevOff (-1) never seeks", -1, 5 * gb, false},
		{"read at offset 0 (header probe) never seeks", 5 * gb, 0, false},
		{"sequential read", 100 * mb, 101 * mb, false},
		{"forward jump of exactly budget is not a seek", 100 * mb, 100*mb + budget, false},
		{"forward jump of budget+1 is a seek", 100 * mb, 100*mb + budget + 1, true},
		{"backward jump of exactly budget is not a seek", 1000 * mb, 1000*mb - budget, false},
		{"backward jump of budget+1 is a seek", 1000 * mb, 1000*mb - budget - 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldInterruptForSeek(c.prevOff, c.off, budget); got != c.want {
				t.Errorf("shouldInterruptForSeek(%d, %d, %d) = %v, want %v", c.prevOff, c.off, budget, got, c.want)
			}
		})
	}
}

func TestShouldInterruptPumpSkipsReadsServedFromTheSSDTail(t *testing.T) {
	t.Parallel()
	const budget = 256 * mb
	cases := []struct {
		name       string
		tailServed bool
		want       bool
	}{
		{"genuine seek not served from the tail interrupts", false, true},
		{"genuine seek served from the SSD tail leaves the pump alone", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldInterruptPump(10*mb, 10*mb+budget+1, budget, c.tailServed); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
	if shouldInterruptPump(10*mb, 11*mb, budget, false) {
		t.Error("a sequential read must not interrupt the pump")
	}
}

func TestAdvanceProvesStreaming(t *testing.T) {
	t.Parallel()
	const (
		size      = 10 * gb
		tol       = 512 * 1024
		threshold = 128 * 1024
		readLen   = 128 * 1024
		prev      = 1 * gb
	)
	zone := tailProbeZoneSize(size) // 512MB
	cases := []struct {
		name               string
		prevOff, off, rlen int64
		want               bool
	}{
		{"sequential streaming-sized read", prev, prev + readLen, readLen, true},
		{"unset previous offset", -1, prev, readLen, false},
		{"same offset is not an advance", prev, prev, readLen, false},
		{"backward read is not an advance", prev, prev - 1, readLen, false},
		{"empty read", prev, prev + 1, 0, false},
		{"gap of exactly readLen+tolerance still counts", prev, prev + readLen + tol, readLen, true},
		{"gap of readLen+tolerance+1 is a skip", prev, prev + readLen + tol + 1, readLen, false},
		{"read of exactly a quarter of the threshold counts", prev, prev + threshold/4, threshold / 4, true},
		{"read below a quarter of the threshold does not", prev, prev + threshold/4, threshold/4 - 1, false},
		{"last offset before the tail zone counts", size - zone - 2, size - zone - 1, readLen, true},
		{"first offset in the tail zone does not", size - zone - 1, size - zone, readLen, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := advanceProvesStreaming(c.prevOff, c.off, c.rlen, size, tol, threshold); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestTailProbeZoneSizeIsFivePercentClamped(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		size int64
		want int64
	}{
		{"tiny file gets the 64MB floor", 10 * mb, 64 * mb},
		{"zero size gets the floor", 0, 64 * mb},
		{"5% still below the floor", 1280*mb - 20, 64 * mb},
		{"5% exactly at the floor", 1280 * mb, 64 * mb},
		{"5% just above the floor", 1280*mb + 20, 64*mb + 1},
		{"10GB remux is 5%", 10 * gb, 10 * gb / 20},
		{"5% exactly at the ceiling", 40 * gb, 2 * gb},
		{"5% just above the ceiling is clamped", 40*gb + 20, 2 * gb},
		{"huge file gets the 2GB ceiling", 500 * gb, 2 * gb},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := tailProbeZoneSize(c.size); got != c.want {
				t.Errorf("tailProbeZoneSize(%d) = %d, want %d", c.size, got, c.want)
			}
		})
	}
}

func TestShouldServeTailFromSSD(t *testing.T) {
	t.Parallel()
	const size, tail = 1000 * mb, 16 * mb
	cases := []struct {
		name              string
		off, size, window int64
		want              bool
	}{
		{"last byte before the window", size - tail - 1, size, tail, false},
		{"first byte of the window", size - tail, size, tail, true},
		{"last byte of the file", size - 1, size, tail, true},
		{"unknown size is never served", 0, 0, tail, false},
		{"negative size is never served", 0, -1, tail, false},
		{"no tail warmup is never served", size - 1, size, 0, false},
		{"window larger than the file covers it all", 0, 10 * mb, tail, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldServeTailFromSSD(c.off, c.size, c.window); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestShouldRecordResumeOff(t *testing.T) {
	t.Parallel()
	const size = 10 * gb
	zone := tailProbeZoneSize(size)
	cases := []struct {
		name               string
		healthy, isPrimary bool
		off, size          int64
		want               bool
	}{
		{"healthy primary mid-file", true, true, 1 * gb, size, true},
		{"unconfirmed playback records nothing", false, true, 1 * gb, size, false},
		{"a secondary handle records nothing", true, false, 1 * gb, size, false},
		{"unknown size records nothing", true, true, 1 * gb, 0, false},
		{"2MB exactly is still the header", true, true, 2 * mb, size, false},
		{"2MB+1 is past the header", true, true, 2*mb + 1, size, true},
		{"last byte before the tail zone", true, true, size - zone - 1, size, true},
		{"first byte of the tail zone is a Cues probe", true, true, size - zone, size, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldRecordResumeOff(c.healthy, c.isPrimary, c.off, c.size); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestResumeAnchorFromPersisted(t *testing.T) {
	t.Parallel()
	const size = 10 * gb
	zone := tailProbeZoneSize(size)
	cases := []struct {
		name       string
		persisted  int64
		wantAnchor int64
		wantZeroed bool
	}{
		{"nothing persisted", 0, 0, false},
		{"2MB exactly is unusable but not poisoned", 2 * mb, 0, false},
		{"2MB+1 is a real position", 2*mb + 1, 2*mb + 1, false},
		{"mid-file position", 3 * gb, 3 * gb, false},
		{"last byte before the tail zone", size - zone - 1, size - zone - 1, false},
		{"first byte of the tail zone is poisoned", size - zone, 0, true},
		{"beyond EOF is poisoned", size + 1, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			anchor, zeroed := resumeAnchorFromPersisted(c.persisted, size)
			if anchor != c.wantAnchor || zeroed != c.wantZeroed {
				t.Errorf("got (%d, %v), want (%d, %v)", anchor, zeroed, c.wantAnchor, c.wantZeroed)
			}
		})
	}
}

func TestPrimaryProofForDemandsMoreFromAFarChallenger(t *testing.T) {
	t.Parallel()
	const budget = 256 * mb
	far := primaryProofReads * 4
	cases := []struct {
		name                  string
		challenger, incumbent int64
		budget                int64
		want                  int32
	}{
		{"no incumbent", 5 * gb, 0, budget, primaryProofReads},
		{"no budget", 5 * gb, 1 * gb, 0, primaryProofReads},
		{"challenger at the incumbent", 1 * gb, 1 * gb, budget, primaryProofReads},
		{"ahead by exactly budget", 1*gb + budget, 1 * gb, budget, primaryProofReads},
		{"ahead by budget+1", 1*gb + budget + 1, 1 * gb, budget, int32(far)},
		{"behind by exactly budget", 1*gb - budget, 1 * gb, budget, primaryProofReads},
		{"behind by budget+1", 1*gb - budget - 1, 1 * gb, budget, int32(far)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := primaryProofFor(c.challenger, c.incumbent, c.budget); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}

func TestPumpStrandedAhead(t *testing.T) {
	t.Parallel()
	const budget = 256 * mb
	cases := []struct {
		name              string
		offset, playerOff int64
		budget            int64
		want              bool
	}{
		{"pump at the budget lead is healthy throttling", 1*gb + budget, 1 * gb, budget, false},
		{"pump exactly 2x budget ahead", 1*gb + 2*budget, 1 * gb, budget, false},
		{"pump 2x budget + 1 ahead is stranded", 1*gb + 2*budget + 1, 1 * gb, budget, true},
		{"no primary handle must never drag the pump back", 9 * gb, 0, budget, false},
		{"no budget", 9 * gb, 1 * gb, 0, false},
		{"pump behind the player", 1 * gb, 5 * gb, budget, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := pumpStrandedAhead(c.offset, c.playerOff, c.budget); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestShouldFireStarvationRevival(t *testing.T) {
	t.Parallel()
	const lead16 = 16 * mb
	cases := []struct {
		name               string
		confirmed          bool
		lead               int64
		pumpRate, playRate float64
		cooldown           time.Duration
		want               bool
	}{
		{"starved confirmed pump past cooldown", true, 1 * mb, 50, 100, 61 * time.Second, true},
		{"unconfirmed playback never revives", false, 1 * mb, 50, 100, 61 * time.Second, false},
		{"cooldown 1ns short", true, 1 * mb, 50, 100, 60*time.Second - 1, false},
		{"cooldown exactly elapsed", true, 1 * mb, 50, 100, 60 * time.Second, true},
		{"lead just under 16MB is thin", true, lead16 - 1, 50, 100, 61 * time.Second, true},
		{"lead of exactly 16MB is thick enough", true, lead16, 50, 100, 61 * time.Second, false},
		{"player not consuming", true, 1 * mb, 0, 0, 61 * time.Second, false},
		{"pump at 74% of the player rate", true, 1 * mb, 74, 100, 61 * time.Second, true},
		{"pump at exactly 75% of the player rate", true, 1 * mb, 75, 100, 61 * time.Second, false},
		{"pump keeping up", true, 1 * mb, 120, 100, 61 * time.Second, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldFireStarvationRevival(c.confirmed, c.lead, c.pumpRate, c.playRate, c.cooldown); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestPumpLeadLimitReservesThreeChunksOfMargin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		budget, chunk int64
		want          int64
	}{
		{"no budget keeps the (non-positive) budget", 0, 16 * mb, 0},
		{"negative budget kept as is", -5, 16 * mb, -5},
		{"no chunk size keeps the budget", 256 * mb, 0, 256 * mb},
		{"default 256MB budget, 16MB chunk: three chunks reserved", 256 * mb, 16 * mb, 208 * mb},
		{"three chunks exactly half the budget", 96 * mb, 16 * mb, 48 * mb},
		{"slack capped at half the budget", 32 * mb, 16 * mb, 16 * mb},
		{"lead floored at one chunk", 16 * mb, 16 * mb, 16 * mb},
		{"budget below one chunk keeps the budget", 10 * mb, 16 * mb, 10 * mb},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := pumpLeadLimit(c.budget, c.chunk); got != c.want {
				t.Errorf("pumpLeadLimit(%d, %d) = %d, want %d", c.budget, c.chunk, got, c.want)
			}
		})
	}
}

func TestScanSlotLimit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		capacity int
		healthy  bool
		want     int
	}{
		{"healthy playback caps scans at 5 regardless of capacity", 25, true, 5},
		{"healthy playback with tiny capacity", 2, true, 5},
		{"idle: capacity minus a 5 slot reserve", 25, false, 20},
		{"capacity 7 leaves 2", 7, false, 2},
		{"capacity 6 leaves the floor of 1", 6, false, 1},
		{"capacity 5 leaves the floor of 1", 5, false, 1},
		{"capacity 0 leaves the floor of 1", 0, false, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := scanSlotLimit(c.capacity, c.healthy); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}

func TestShortAwayFromEOF(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		n, want   int
		off, size int64
		short     bool
	}{
		{"full read", 100, 100, 0, 1000, false},
		{"short with file left is a gap", 60, 100, 0, 1000, true},
		{"short ending exactly at EOF is normal", 60, 100, 940, 1000, false},
		{"short ending one byte before EOF is a gap", 59, 100, 940, 1000, true},
		{"short past EOF is normal", 10, 100, 995, 1000, false},
		{"zero bytes away from EOF is a gap", 0, 100, 0, 1000, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := shortAwayFromEOF(c.n, c.want, c.off, c.size); got != c.short {
				t.Errorf("got %v, want %v", got, c.short)
			}
		})
	}
}

func TestStateName(t *testing.T) {
	t.Parallel()
	cases := map[uint32]string{stateWarmup: "WARMUP", stateStreaming: "STREAMING", 7: "UNKNOWN"}
	for s, want := range cases {
		if got := stateName(s); got != want {
			t.Errorf("stateName(%d) = %q, want %q", s, got, want)
		}
	}
}

func TestOwnsPumpStateOnlyForTheSamePointer(t *testing.T) {
	t.Parallel()
	a, b := &NativePumpState{}, &NativePumpState{}
	if !ownsPumpState(a, a) {
		t.Error("a handle must own the pump it counted")
	}
	if ownsPumpState(a, b) {
		t.Error("a handle must not own a replacement pump")
	}
	if ownsPumpState(nil, nil) || ownsPumpState(nil, a) {
		t.Error("a handle without a pump state owns nothing")
	}
}

func TestAbs(t *testing.T) {
	t.Parallel()
	for in, want := range map[int64]int64{-5: 5, 0: 0, 7: 7} {
		if got := abs(in); got != want {
			t.Errorf("abs(%d) = %d, want %d", in, got, want)
		}
	}
}

// The tests below touch package globals (activeHandles, playbackRegistry, activePumps,
// config) and therefore run serially, restoring each global they change.

func newTestHandle(path string) *MkvHandle {
	h := &MkvHandle{path: path, hash: "abcdef0123456789", size: 10 * gb}
	h.lastOff = -1
	return h
}

func registerHandle(t *testing.T, hs ...*MkvHandle) {
	t.Helper()
	for _, h := range hs {
		app.activeHandles.Store(h, true)
	}
	t.Cleanup(func() {
		for _, h := range hs {
			app.activeHandles.Delete(h)
		}
	})
}

func TestBecomePrimaryDemotesTheOldPrimaryAndResetsItsStreak(t *testing.T) {
	newTestApp(t)
	const path = "/test/become-primary.mkv"
	old, next, other := newTestHandle(path), newTestHandle(path), newTestHandle("/test/other.mkv")
	old.isPrimaryHandle.Store(true)
	old.seqAdvances.Store(2)
	other.isPrimaryHandle.Store(true)
	registerHandle(t, old, next, other)

	if !becomePrimary(next, "test", 0, 0) {
		t.Fatal("promotion of a non-primary handle must report true")
	}
	if !next.isPrimaryHandle.Load() || old.isPrimaryHandle.Load() {
		t.Error("the promoted handle must be the only primary for its path")
	}
	if old.seqAdvances.Load() != 0 {
		t.Error("a demoted handle loses its sequential streak")
	}
	if !other.isPrimaryHandle.Load() {
		t.Error("a primary on another path must be left alone")
	}
	if becomePrimary(next, "test", 0, 0) {
		t.Error("promoting an already primary handle must report false")
	}
}

func TestPrimaryOffsetForTakesTheHighestOtherPrimaryOnThePath(t *testing.T) {
	newTestApp(t)
	const path = "/test/primary-offset.mkv"
	self, p1, p2, secondary, other := newTestHandle(path), newTestHandle(path), newTestHandle(path), newTestHandle(path), newTestHandle("/test/else.mkv")
	for h, off := range map[*MkvHandle]int64{self: 9 * gb, p1: 1 * gb, p2: 3 * gb, secondary: 8 * gb, other: 7 * gb} {
		h.lastOff = off
	}
	self.isPrimaryHandle.Store(true)
	p1.isPrimaryHandle.Store(true)
	p2.isPrimaryHandle.Store(true)
	other.isPrimaryHandle.Store(true)
	registerHandle(t, self, p1, p2, secondary, other)

	if got := primaryOffsetFor(path, self); got != 3*gb {
		t.Errorf("got %d, want the highest other primary on the path (%d)", got, 3*gb)
	}
	if got := primaryOffsetFor("/test/no-handles.mkv", nil); got != 0 {
		t.Errorf("a path without handles must report 0, got %d", got)
	}
}

func TestPromoteIfStreaming(t *testing.T) {
	newTestApp(t)
	useTestGlobals(t)
	const (
		path    = "/test/promote.mkv"
		readLen = 256 * 1024
	)
	healthy := func(t *testing.T) {
		t.Helper()
		app.playbackRegistry.Store(path, &PlaybackState{Path: path, IsHealthy: true})
		t.Cleanup(func() { app.playbackRegistry.Delete(path) })
	}
	// reads feeds n consecutive sequential reads starting at start.
	reads := func(h *MkvHandle, start int64, n int) {
		off := start
		for i := 0; i < n; i++ {
			h.promoteIfStreaming(off-readLen, off, readLen)
			off += readLen
		}
	}

	t.Run("promotes after primaryProofReads sequential reads in confirmed playback", func(t *testing.T) {
		healthy(t)
		h := newTestHandle(path)
		registerHandle(t, h)
		reads(h, 1*gb, primaryProofReads-1)
		if h.isPrimaryHandle.Load() {
			t.Fatal("promoted one read early")
		}
		reads(h, 1*gb+int64(primaryProofReads-1)*readLen, 1)
		if !h.isPrimaryHandle.Load() {
			t.Fatal("not promoted after the proof reads")
		}
	})
	t.Run("a non-sequential read resets the streak", func(t *testing.T) {
		healthy(t)
		h := newTestHandle(path)
		registerHandle(t, h)
		reads(h, 1*gb, primaryProofReads-1)
		h.promoteIfStreaming(1*gb, 3*gb, readLen) // jump
		if h.seqAdvances.Load() != 0 {
			t.Errorf("streak = %d after a jump, want 0", h.seqAdvances.Load())
		}
	})
	t.Run("without confirmed or inferred playback nothing is promoted", func(t *testing.T) {
		app.playbackRegistry.Store(path, &PlaybackState{Path: path})
		t.Cleanup(func() { app.playbackRegistry.Delete(path) })
		h := newTestHandle(path)
		registerHandle(t, h)
		reads(h, 1*gb, primaryProofReads+2)
		if h.isPrimaryHandle.Load() {
			t.Error("a background scan was promoted to primary")
		}
	})
	t.Run("without a registry entry nothing is promoted", func(t *testing.T) {
		h := newTestHandle(path)
		registerHandle(t, h)
		reads(h, 1*gb, primaryProofReads+2)
		if h.isPrimaryHandle.Load() {
			t.Error("promoted without any playback registered")
		}
	})
	t.Run("a handle without a hash is never promoted", func(t *testing.T) {
		healthy(t)
		h := newTestHandle(path)
		h.hash = ""
		registerHandle(t, h)
		reads(h, 1*gb, primaryProofReads+2)
		if h.isPrimaryHandle.Load() {
			t.Error("promoted without a hash")
		}
	})
	t.Run("tail-zone reads never count", func(t *testing.T) {
		healthy(t)
		h := newTestHandle(path)
		registerHandle(t, h)
		reads(h, h.size-tailProbeZoneSize(h.size)+mb, primaryProofReads+2)
		if h.isPrimaryHandle.Load() {
			t.Error("an EOF probe was promoted")
		}
	})
	t.Run("a challenger far from the incumbent needs four times the proof", func(t *testing.T) {
		healthy(t)
		incumbent, h := newTestHandle(path), newTestHandle(path)
		incumbent.isPrimaryHandle.Store(true)
		incumbent.lastOff = 1 * gb
		registerHandle(t, incumbent, h)
		reads(h, 5*gb, primaryProofReads*4-1)
		if h.isPrimaryHandle.Load() {
			t.Fatal("far challenger promoted early")
		}
		reads(h, 5*gb+int64(primaryProofReads*4-1)*readLen, 1)
		if !h.isPrimaryHandle.Load() || incumbent.isPrimaryHandle.Load() {
			t.Error("far challenger must take primary after the extended proof and demote the incumbent")
		}
	})
	t.Run("an already primary handle is left alone", func(t *testing.T) {
		h := newTestHandle(path)
		h.isPrimaryHandle.Store(true)
		h.promoteIfStreaming(1*gb, 1*gb+readLen, readLen)
		if h.seqAdvances.Load() != 0 {
			t.Error("a primary handle must not count streaks")
		}
	})
}

func TestAttachToExistingPumpAttachesExactlyOnce(t *testing.T) {
	const path = "/test/attach.mkv"
	ps := &NativePumpState{path: path, cancel: func() {}}
	h := newTestHandle(path)
	t.Cleanup(func() { globalOpenTracker.Dec(h.hash, h.path) })

	if !h.attachToExistingPump(ps) {
		t.Fatal("first attach must succeed")
	}
	if h.attachToExistingPump(ps) {
		t.Fatal("second attach on the same handle must be refused")
	}
	if got := atomic.LoadInt32(&ps.refCount); got != 1 {
		t.Errorf("refCount = %d, want 1 (a double attach would pin the pump)", got)
	}
	if !h.hasSlot.Load() || h.pumpState != ps || !h.isWatching || h.pumpCancel == nil {
		t.Error("attach must record the slot, the state and the cancel func on the handle")
	}
	if !globalOpenTracker.IsPathOpen(path) {
		t.Error("attach must be visible to the open tracker")
	}
}

func TestTerminateOrphanPump(t *testing.T) {
	newTestApp(t)
	const path = "/test/orphan.mkv"
	t.Cleanup(func() { app.activePumps.Delete(path) })

	if terminateOrphanPump(path) {
		t.Error("no pump registered: must report false")
	}
	var cancelled atomic.Bool
	app.activePumps.Store(path, &NativePumpState{cancel: func() { cancelled.Store(true) }})
	if !terminateOrphanPump(path) {
		t.Fatal("registered pump: must report true")
	}
	if !cancelled.Load() {
		t.Error("the pump must be cancelled")
	}
	if _, ok := app.activePumps.Load(path); ok {
		t.Error("the pump must be forgotten")
	}
	app.activePumps.Store(path, &NativePumpState{}) // nil cancel must not panic
	if !terminateOrphanPump(path) {
		t.Error("a pump without a cancel func is still removed")
	}
}

func TestSeedResumeAnchor(t *testing.T) {
	useTestGlobals(t)
	const size = 10 * gb
	zone := tailProbeZoneSize(size)
	newH := func() *MkvHandle {
		h := newTestHandle("/test/seed.mkv")
		h.size = size
		return h
	}

	t.Run("a real position anchors the pump start", func(t *testing.T) {
		h := newH()
		if !h.seedResumeAnchor(nil, 3*gb) {
			t.Fatal("want anchored")
		}
		if h.lastOff != 3*gb {
			t.Errorf("lastOff = %d, want %d", h.lastOff, 3*gb)
		}
	})
	t.Run("a header-sized position is ignored", func(t *testing.T) {
		h := newH()
		if h.seedResumeAnchor(nil, 2*mb) || h.lastOff != -1 {
			t.Error("a position at or below 2MB must not anchor")
		}
	})
	t.Run("a tail-zone position is not used and is cleared in memory", func(t *testing.T) {
		h := newH()
		ps := &PlaybackState{LastSeekOff: size - zone}
		if h.seedResumeAnchor(ps, size-zone) {
			t.Error("a poisoned tail probe must not anchor")
		}
		if h.lastOff != -1 {
			t.Error("lastOff must be untouched")
		}
		if ps.LastSeekOff != 0 {
			t.Errorf("LastSeekOff = %d, want the poisoned value zeroed", ps.LastSeekOff)
		}
	})
	t.Run("a tail-zone position without state or database does not panic", func(t *testing.T) {
		h := newH()
		if h.seedResumeAnchor(nil, size+1) {
			t.Error("beyond EOF must not anchor")
		}
	})
}

func TestForceTorrentWarmupActiveIsANoOpWithoutDiskWarmup(t *testing.T) {
	forceTorrentWarmupActive("abcdef0123456789", 0) // warmup.DiskWarmup is nil in tests
}
