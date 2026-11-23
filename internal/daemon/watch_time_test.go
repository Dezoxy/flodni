package daemon

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// watchEnv is stallEnv with the torrent of the session confirmed or not, and the
// counter in nanoseconds relative to its value at the start.
func watchEnv(t *testing.T, confirmed bool) (*time.Time, func() time.Duration) {
	t.Helper()
	_, clock := stallEnv(t)
	if confirmed {
		app.playbackRegistry.Store("/m/s.mkv", &PlaybackState{Path: "/m/s.mkv", Hash: kaHashA, IsHealthy: true})
		invalidateConfirmed()
		if !hashConfirmed(kaHashA) {
			t.Fatal("torrent not confirmed")
		}
	}
	base := confirmedPlaybackNS.Load()
	return clock, func() time.Duration { return time.Duration(confirmedPlaybackNS.Load() - base) }
}

func watchRead(off int64) {
	ttffRead("/m/s.mkv", srcFetchBlock, time.Millisecond, 4096, off, stallRoleSecond)
}

func TestWatchTimeFirstReadCreditsNothing(t *testing.T) {
	_, credited := watchEnv(t, true)
	watchRead(0)
	if got := credited(); got != 0 {
		t.Fatalf("first read credited %v, want 0", got)
	}
}

func TestWatchTimeGapUnderCapIsCredited(t *testing.T) {
	clock, credited := watchEnv(t, true)
	watchRead(0)
	*clock = clock.Add(7*time.Second + 250*time.Millisecond)
	watchRead(4096)
	if got, want := credited(), 7*time.Second+250*time.Millisecond; got != want {
		t.Fatalf("credited %v, want %v", got, want)
	}
}

func TestWatchTimeGapOverCapCreditsExactlyTheCap(t *testing.T) {
	for _, gap := range []time.Duration{watchCreditCap, watchCreditCap + time.Nanosecond, 10 * time.Minute} {
		clock, credited := watchEnv(t, true)
		watchRead(0)
		*clock = clock.Add(gap)
		watchRead(4096)
		if got := credited(); got != watchCreditCap {
			t.Fatalf("gap %v credited %v, want the cap %v", gap, got, watchCreditCap)
		}
	}
	if watchCreditCap != 30*time.Second {
		t.Fatalf("cap is %v, want 30s", watchCreditCap)
	}
}

// An unconfirmed read credits nothing but moves the clock, so a confirmation that
// arrives later does not credit the unconfirmed time before it.
// Reads less than a step after the last move credit nothing; their time stays in the
// gap the next read at least a step later credits whole.
func TestWatchTimeReadsInsideAStepAccumulate(t *testing.T) {
	clock, credited := watchEnv(t, true)
	t0 := *clock
	watchRead(0)
	for _, d := range []time.Duration{300 * time.Millisecond, 600 * time.Millisecond} {
		*clock = t0.Add(d)
		watchRead(4096)
		if got := credited(); got != 0 {
			t.Fatalf("read at +%v credited %v, want 0", d, got)
		}
	}
	*clock = t0.Add(1200 * time.Millisecond)
	watchRead(8192)
	if got, want := credited(), 1200*time.Millisecond; got != want {
		t.Fatalf("credited %v, want %v", got, want)
	}
}

// Many reads inside one step reach the confirmed lookup (one confirmedHashesNow call
// each) not at all; the first read at least a step later reaches it once.
func TestWatchTimeConfirmedLookupOncePerStep(t *testing.T) {
	clock, _ := watchEnv(t, true)
	var lookups atomic.Int64
	prev := confirmedHashesNow
	confirmedHashesNow = func() time.Time { lookups.Add(1); return time.Now() }
	t.Cleanup(func() { confirmedHashesNow = prev })
	t0 := *clock
	watchRead(0)
	for i := 1; i <= 500; i++ {
		*clock = t0.Add(time.Duration(i) * time.Millisecond)
		watchRead(int64(i) * 4096)
	}
	if got := lookups.Load(); got != 0 {
		t.Fatalf("%d lookups inside one step, want 0", got)
	}
	*clock = t0.Add(watchCreditStep)
	watchRead(0)
	if got := lookups.Load(); got != 1 {
		t.Fatalf("%d lookups at the step, want 1", got)
	}
}

func TestWatchTimeUnconfirmedReadAdvancesClockWithoutCredit(t *testing.T) {
	clock, credited := watchEnv(t, false)
	watchRead(0)
	*clock = clock.Add(10 * time.Second)
	watchRead(4096)
	if got := credited(); got != 0 {
		t.Fatalf("unconfirmed credited %v, want 0", got)
	}
	app.playbackRegistry.Store("/m/s.mkv", &PlaybackState{Path: "/m/s.mkv", Hash: kaHashA, IsHealthy: true})
	invalidateConfirmed()
	*clock = clock.Add(3 * time.Second)
	watchRead(8192)
	if got, want := credited(), 3*time.Second; got != want {
		t.Fatalf("credited %v after confirmation, want only the last gap %v", got, want)
	}
}

// A stopped playback is not confirmed: the predicate is the one of the stall counter.
func TestWatchTimeUsesTheStallCounterPredicate(t *testing.T) {
	clock, credited := watchEnv(t, true)
	watchRead(0)
	app.playbackRegistry.Store("/m/s.mkv", &PlaybackState{Path: "/m/s.mkv", Hash: kaHashA, IsHealthy: true, IsStopped: true})
	invalidateConfirmed()
	*clock = clock.Add(5 * time.Second)
	watchRead(4096)
	if got := credited(); got != 0 || hashConfirmed(kaHashA) {
		t.Fatalf("stopped playback credited %v", got)
	}
}

// Concurrent reads of one session credit each stretch of time once: with a clock that
// moves on every call, the total never exceeds the time from the first read to the
// latest read time any goroutine saw, and falls short of it by under a step, however the
// reads interleave.
func TestWatchTimeConcurrentReadsCreditElapsedTimeOnce(t *testing.T) {
	_, credited := watchEnv(t, true)
	start := time.Unix(1_700_000_000, 0)
	const tick = 100 * time.Millisecond
	var ticks atomic.Int64
	ttffNow = func() time.Time { return start.Add(time.Duration(ticks.Add(1)) * tick) }
	watchRead(0)
	first := ticks.Load()
	const workers, per = 8, 200
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				watchRead(int64(i) * 4096)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Duration(ticks.Load()-first) * tick
	// Never more than the elapsed time; less only by the step the last read may not
	// have moved the clock past, and the few ticks goroutines can be out of order by.
	if got := credited(); got > elapsed || got < elapsed-watchCreditStep-time.Duration(workers)*tick {
		t.Fatalf("credited %v over %v elapsed", got, elapsed)
	}
}

func TestWatchTimeMetricExposedFromZero(t *testing.T) {
	resetKeepaliveState(t)
	body := scrape(t).Body.String()
	if !strings.Contains(body, "# TYPE flodni_confirmed_playback_seconds_total counter") {
		t.Error("TYPE line missing")
	}
	base := confirmedPlaybackNS.Load()
	if base == 0 && !strings.Contains(body, "\nflodni_confirmed_playback_seconds_total 0\n") {
		t.Errorf("series missing or not 0:\n%s", grepLines(body, "flodni_confirmed_playback"))
	}
}

// Whole seconds would lose the sub-second part of every gap; the exposed value is the
// running total in seconds.
func TestWatchTimeMetricKeepsSubSecondRemainder(t *testing.T) {
	resetKeepaliveState(t)
	prev := confirmedPlaybackNS.Load()
	confirmedPlaybackNS.Store(1_500_000_000)
	t.Cleanup(func() { confirmedPlaybackNS.Store(prev) })
	body := scrape(t).Body.String()
	if !strings.Contains(body, "\nflodni_confirmed_playback_seconds_total 1.5\n") {
		t.Errorf("want 1.5:\n%s", grepLines(body, "flodni_confirmed_playback"))
	}
}
