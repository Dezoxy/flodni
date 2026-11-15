package daemon

import (
	"bytes"
	"testing"

	"flodni/internal/warmup"
)

// Reader budget follow-ups: the read-ahead overshoot (counted always, clamped on
// opt-in) and the attribution of pump and prefetch fills.

func TestReadaheadOvershootArithmetic(t *testing.T) {
	const size = 10 * gb
	head := warmup.FileSize
	tests := []struct {
		name                string
		off, target, fetchN int64
		want                int64
	}{
		{"read-ahead stays inside the head", 0, 256 << 10, 8 * mb, 0},
		{"read-ahead crosses the head edge", head - mb, 256 << 10, 8 * mb, 7 * mb},
		{"the read itself straddles the edge", head - 128<<10, 256 << 10, 4 * mb, 4*mb - 128<<10 - 128<<10},
		{"a miss in the middle is not an overshoot", 500 * mb, 256 << 10, 8 * mb, 0},
		{"a miss in the tail window reads to the end", size - mb, 256 << 10, mb, 0},
		{"no read-ahead", head - mb, 256 << 10, 256 << 10, 0},
	}
	for _, tt := range tests {
		if got := readaheadOvershoot(tt.off, tt.target, tt.fetchN, size); got != tt.want {
			t.Errorf("%s: overshoot = %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestClampReadahead(t *testing.T) {
	newTestApp(t)
	const size = 10 * gb
	head := warmup.FileSize
	on := func(v bool) {
		prev := unconfirmedReadaheadClamp
		unconfirmedReadaheadClamp = func() bool { return v }
		t.Cleanup(func() { unconfirmedReadaheadClamp = prev })
	}
	const path = "/m/clamp.mkv"
	app.playbackRegistry.Delete(path)

	off, end := head-mb, head-mb+256<<10-1
	on(false)
	if got := clampReadahead(path, off, end, off+8*mb-1, size); got != off+8*mb-1 {
		t.Errorf("switch off: fetchEnd = %d, moved", got)
	}
	on(true)
	if got := clampReadahead(path, off, end, off+8*mb-1, size); got != head-1 {
		t.Errorf("clamp on: fetchEnd = %d, want the window edge %d", got, head-1)
	}
	// Never short of what the read asked for.
	off, end = head-128<<10, head+128<<10-1
	if got := clampReadahead(path, off, end, off+4*mb-1, size); got != end {
		t.Errorf("straddling read: fetchEnd = %d, want the end of the read %d", got, end)
	}
	// A read that starts in the middle is not touched.
	off, end = 500*mb, 500*mb+256<<10-1
	if got := clampReadahead(path, off, end, off+8*mb-1, size); got != off+8*mb-1 {
		t.Errorf("middle read: fetchEnd = %d, moved", got)
	}
	// A confirmed file is not touched.
	confirmPlayback(path)
	t.Cleanup(func() { app.playbackRegistry.Delete(path) })
	off, end = head-mb, head-mb+256<<10-1
	if got := clampReadahead(path, off, end, off+8*mb-1, size); got != off+8*mb-1 {
		t.Errorf("confirmed file: fetchEnd = %d, moved", got)
	}
}

// overshootRead reads 256 KiB one MiB before the head window edge of the budget session's
// file and returns the length of the engine fetch and the overshoot counted.
func overshootRead(t *testing.T, clamp bool, confirmed bool) (fetchLen int64, counted int64) {
	t.Helper()
	fe := useEngineReadPath(t)
	s := budgetEnv(t, 1<<40)
	prev := unconfirmedReadaheadClamp
	unconfirmedReadaheadClamp = func() bool { return clamp }
	t.Cleanup(func() { unconfirmedReadaheadClamp = prev })
	if confirmed {
		confirmPlayback(s.path)
	}
	h := engineHandle(s.path, s.size)
	off := warmup.FileSize - mb
	o0 := app.readaheadOvershootBytes.Load()
	got := readBytes(t, h, 256*1024, off)
	if !bytes.Equal(got, wantPattern(256*1024, off)) {
		t.Fatal("wrong bytes")
	}
	calls := fe.aheadCalls()
	if len(calls) != 1 {
		t.Fatalf("FetchAhead calls = %d, want 1", len(calls))
	}
	waitUntil(t, "the fill to land", func() bool { return raCache.Covered(s.path, off, off+256*1024-1) })
	return int64(calls[0].bufLen), app.readaheadOvershootBytes.Load() - o0
}

func TestReadaheadOvershootIsCountedWithoutTheClamp(t *testing.T) {
	// The first read of a handle gets ReadAheadInitial (8 MiB in the test path): 1 MiB
	// inside the head window, the rest past its edge.
	n, over := overshootRead(t, false, false)
	if n != 8*mb || over != 7*mb {
		t.Fatalf("fetch %d bytes, overshoot %d, want %d and %d", n, over, 8*mb, 7*mb)
	}
}

func TestReadaheadClampStopsAtTheEdgeAndCountsNothing(t *testing.T) {
	n, over := overshootRead(t, true, false)
	if n != mb || over != 0 {
		t.Fatalf("fetch %d bytes, overshoot %d, want %d and 0", n, over, mb)
	}
}

func TestReadaheadOvershootAndClampSkipAConfirmedFile(t *testing.T) {
	n, over := overshootRead(t, true, true)
	if n != 8*mb || over != 0 {
		t.Fatalf("confirmed: fetch %d bytes, overshoot %d, want %d and 0 (no clamp, no count)", n, over, 8*mb)
	}
}

// --- attribution of fills ---

func TestNoteFillAttributedCountsOnlyWhatTheBudgetSees(t *testing.T) {
	s := budgetEnv(t, 1<<40)
	a0 := app.attributedFillBytes.Load()
	head := warmup.FileSize

	noteFillAttributed(s.path, 0, 4*mb, s.size) // inside the head window
	noteFillAttributed(s.path, s.size-mb, mb, s.size)
	noteFillAttributed("/m/no-session.mkv", 500*mb, 4*mb, s.size)
	if d := app.attributedFillBytes.Load() - a0; d != 0 {
		t.Fatalf("window fills or a file without a session counted +%d", d)
	}
	noteFillAttributed(s.path, head-mb, 4*mb, s.size) // straddles the edge: 3 MiB outside
	noteFillAttributed(s.path, 500*mb, 4*mb, s.size)
	if d := app.attributedFillBytes.Load() - a0; d != 7*mb {
		t.Fatalf("counted +%d, want %d", d, 7*mb)
	}
	if s.unconfirmedFill.Load() != 7*mb {
		t.Fatalf("session fill tally = %d", s.unconfirmedFill.Load())
	}
	confirmPlayback(s.path)
	noteFillAttributed(s.path, 500*mb, 4*mb, s.size)
	if d := app.attributedFillBytes.Load() - a0; d != 7*mb {
		t.Fatalf("a confirmed file counted +%d more", d-7*mb)
	}
}

func TestAttributionMovesTheMarginOnlyWhenSwitchedOn(t *testing.T) {
	s := budgetEnv(t, 8*mb)
	prev := attributeFillsToReader
	t.Cleanup(func() { attributeFillsToReader = prev })

	attributeFillsToReader = func() bool { return false }
	noteFillAttributed(s.path, 500*mb, 4*mb, s.size)
	noteFillAttributed(s.path, 504*mb, 4*mb, s.size)
	if s.pastMargin() || s.overMarginAt.Load() != 0 {
		t.Fatal("with the switch off, fills moved the margin")
	}
	if s.unconfirmedFill.Load() != 8*mb {
		t.Fatalf("fills not counted with the switch off: %d", s.unconfirmedFill.Load())
	}

	attributeFillsToReader = func() bool { return true }
	if !s.pastMargin() {
		t.Fatal("with the switch on, 8 MiB of fills did not reach an 8 MiB margin")
	}
	// The served tally and the fills are two views of the same bytes: the larger counts.
	s.unconfirmedCache.Store(3 * mb)
	if got := s.budgetTally(); got != 8*mb {
		t.Fatalf("tally = %d, want max(served, fills) = %d", got, 8*mb)
	}
	s.unconfirmedCache.Store(9 * mb)
	if got := s.budgetTally(); got != 9*mb {
		t.Fatalf("tally = %d, want %d", got, 9*mb)
	}
}

func TestAttributedFillCrossesTheMarginOnce(t *testing.T) {
	s := budgetEnv(t, 8*mb)
	prev := attributeFillsToReader
	attributeFillsToReader = func() bool { return true }
	t.Cleanup(func() { attributeFillsToReader = prev })
	captureSessionLog(t)

	noteFillAttributed(s.path, 500*mb, 4*mb, s.size)
	if s.overMarginAt.Load() != 0 {
		t.Fatal("crossed early")
	}
	noteFillAttributed(s.path, 504*mb, 4*mb, s.size)
	if s.overMarginAt.Load() == 0 {
		t.Fatal("did not cross at 8 MiB of attributed fills")
	}
}

func TestPumpFillIsAttributedToTheSession(t *testing.T) {
	useEngineReadPath(t)
	s := budgetEnv(t, 1<<40)
	h := engineHandle(s.path, s.size)
	a0 := app.attributedFillBytes.Load()
	rd := &fakeReader{}
	if stop, next := h.nativePumpChunk(rd, 500*mb, 4*mb, 500*mb); stop || next != 504*mb {
		t.Fatalf("got (%v, %d)", stop, next)
	}
	if d := app.attributedFillBytes.Load() - a0; d != 4*mb {
		t.Fatalf("pump fill attributed +%d, want %d", d, 4*mb)
	}
}

func TestWarmLandingPrefetchIsAttributedToTheSession(t *testing.T) {
	fe := useEngineReadPath(t)
	s := budgetEnv(t, 1<<40)
	h := engineHandle(s.path, s.size)
	h.hasSlot.Store(false)
	a0 := app.attributedFillBytes.Load()
	readBytes(t, h, 256*1024, 500*mb) // a new handle's first read past the head
	waitUntil(t, "the prefetch to be attributed", func() bool { return app.attributedFillBytes.Load()-a0 >= 4*mb })
	waitUntil(t, "prefetches to end", flightFree(h.path, 500*mb))
	if len(fe.blockCalls()) == 0 {
		t.Fatal("no prefetch ran")
	}
}

// With the switch off, fills are only counted: at a margin of 0 a pump fill past the window
// must not log over_margin or make the session count as unconfirmed.
func TestFillsDoNotCrossTheMarginWithTheSwitchOff(t *testing.T) {
	s := budgetEnv(t, 0)
	prev := attributeFillsToReader
	attributeFillsToReader = func() bool { return false }
	t.Cleanup(func() { attributeFillsToReader = prev })
	buf := captureSessionLog(t)
	_, u0, _ := rbCounts()

	noteFillAttributed(s.path, 500*mb, 4*mb, s.size)
	if s.overMarginAt.Load() != 0 || len(linesWith(buf, rbPrefix)) != 0 {
		t.Fatalf("a fill logged over_margin with the switch off:\n%s", buf)
	}
	s.closeSession()
	if _, u, _ := rbCounts(); u != u0 {
		t.Fatalf("a fill-only session counted as unconfirmed (+%d)", u-u0)
	}
}

// The served and fill tallies each count from their own floor, so flipping the switch while
// a session is open does not measure one from the other's floor.
func TestFloorsAreKeptApartAcrossALiveToggle(t *testing.T) {
	s := budgetEnv(t, 8*mb)
	prev := attributeFillsToReader
	t.Cleanup(func() { attributeFillsToReader = prev })
	attributeFillsToReader = func() bool { return false }

	s.unconfirmedCache.Store(20 * mb)
	noteFillAttributed(s.path, 500*mb, 4*mb, s.size) // 4 MiB of fills
	confirmPlayback(s.path)
	noteConfirmedLate(s.path) // floors: served 20 MiB, fill 4 MiB
	app.playbackRegistry.Delete(s.path)

	attributeFillsToReader = func() bool { return true }
	if s.pastMargin() {
		t.Fatal("pre-spent right after a confirmation")
	}
	noteFillAttributed(s.path, 504*mb, 4*mb, s.size)
	if s.pastMargin() { // 4 MiB of fills since the floor, below 8
		t.Fatalf("since = %d, want 4 MiB", s.budgetSince())
	}
	noteFillAttributed(s.path, 508*mb, 4*mb, s.size)
	if !s.pastMargin() || s.budgetSince() != 8*mb {
		t.Fatalf("since = %d, want 8 MiB", s.budgetSince())
	}
}
