package daemon

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"
)

var kaT0 = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

const (
	kaHashC = "cccccccccccccccccccccccccccccccccccccccc"
	kaHashD = "dddddddddddddddddddddddddddddddddddddddd"
)

// keepaliveFixture isolates the registry, the caches, the held set and the counters,
// pins the clock at kaT0 and sets the keep-alive to minutes and max.
func keepaliveFixture(t *testing.T, minutes, max int) *time.Time {
	t.Helper()
	resetKeepaliveState(t)
	resetKeepaliveHold(t)
	now := kaT0
	confirmedHashesNow = func() time.Time { return now }
	prev := keepaliveSettings
	keepaliveSettings = func() (int, int) { return minutes, max }
	t.Cleanup(func() { keepaliveSettings = prev })
	return &now
}

func resetKeepaliveHold(t *testing.T) {
	t.Helper()
	reset := func() {
		keepalive.mu.Lock()
		keepalive.held = nil
		keepalive.evictedSeen = nil
		keepalive.verdicts = nil
		keepalive.gen = 0
		keepalive.mu.Unlock()
		for i := range keepaliveCounters.releases {
			keepaliveCounters.releases[i].Store(0)
		}
		keepaliveCounters.evicted.Store(0)
	}
	reset()
	t.Cleanup(reset)
}

// kaPlay registers a confirmed state whose last read was `ago` before kaT0.
func kaPlay(path, hash string, ago time.Duration) *PlaybackState {
	ps := &PlaybackState{Path: path, Hash: hash, IsHealthy: true,
		ConfirmedAt: kaT0.Add(-ago), LastReadAt: kaT0.Add(-ago)}
	app.playbackRegistry.Store(path, ps)
	return ps
}

func kaReleases() (stop, ceiling int64) {
	return keepaliveCounters.releases[0].Load(), keepaliveCounters.releases[1].Load()
}

// A confirmed torrent with a recent read is held; the gauge counts it.
func TestKeepaliveHoldsConfirmedRecentRead(t *testing.T) {
	keepaliveFixture(t, 15, 4)
	kaPlay("/m/a.mkv", kaHashA, 2*time.Minute)
	if !keepaliveHold(kaHashA) {
		t.Fatal("confirmed torrent with a recent read not held")
	}
	if got := keepaliveHeld(); got != 1 {
		t.Fatalf("held gauge = %d, want 1", got)
	}
}

// Last activity is the later of the last read and the last play/resume webhook: a
// recent webhook with no read holds. Nothing else extends it, so a pause (which leaves
// no mark) cannot.
func TestKeepaliveActivityIsReadOrConfirm(t *testing.T) {
	keepaliveFixture(t, 15, 4)
	ps := kaPlay("/m/a.mkv", kaHashA, 20*time.Minute)
	if keepaliveHold(kaHashA) {
		t.Fatal("held 20 minutes after the last activity with a 15 minute ceiling")
	}
	ps.mu.Lock()
	ps.ConfirmedAt = kaT0.Add(-time.Minute) // a resume webhook; no reads
	ps.mu.Unlock()
	invalidateConfirmed()
	if !keepaliveHold(kaHashA) {
		t.Fatal("a recent play/resume webhook did not hold")
	}
}

// Past the ceiling the hold ends and is counted once as a ceiling release.
func TestKeepaliveCeilingReleases(t *testing.T) {
	now := keepaliveFixture(t, 15, 4)
	kaPlay("/m/a.mkv", kaHashA, 10*time.Minute)
	if !keepaliveHold(kaHashA) {
		t.Fatal("not held at 10 minutes")
	}
	*now = kaT0.Add(6 * time.Minute) // 16 minutes idle
	if keepaliveHold(kaHashA) {
		t.Fatal("still held past the ceiling")
	}
	if keepaliveHold(kaHashA) { // asked again
		t.Fatal("held again")
	}
	if stop, ceiling := kaReleases(); stop != 0 || ceiling != 1 {
		t.Fatalf("releases stop=%d ceiling=%d, want 0 and 1", stop, ceiling)
	}
	if keepaliveHeld() != 0 {
		t.Fatal("gauge not back to 0")
	}
}

// A stop webhook releases at once, inside the one-second cache window, counted as a
// stop release.
func TestKeepaliveStopReleasesAtOnce(t *testing.T) {
	keepaliveFixture(t, 15, 4)
	ps := kaPlay("/m/a.mkv", kaHashA, time.Minute)
	if !keepaliveHold(kaHashA) {
		t.Fatal("not held")
	}
	ps.mu.Lock()
	ps.IsStopped = true
	ps.mu.Unlock()
	ps.SetHealthy(false)
	invalidateConfirmed() // what the webhook handler does after SetHealthy
	if keepaliveHold(kaHashA) {
		t.Fatal("held after stop")
	}
	if stop, ceiling := kaReleases(); stop != 1 || ceiling != 0 {
		t.Fatalf("releases stop=%d ceiling=%d, want 1 and 0", stop, ceiling)
	}
}

// Without invalidation the stop shows within a second, never later.
func TestKeepaliveStopVisibleAfterTheCacheWindow(t *testing.T) {
	now := keepaliveFixture(t, 15, 4)
	ps := kaPlay("/m/a.mkv", kaHashA, time.Minute)
	keepaliveHold(kaHashA)
	ps.mu.Lock()
	ps.IsHealthy, ps.IsStopped = false, true
	ps.mu.Unlock()
	*now = kaT0.Add(1100 * time.Millisecond)
	if keepaliveHold(kaHashA) {
		t.Fatal("held after stop and a second")
	}
}

// An unconfirmed torrent is never held, however fresh its reads, and costs no counter.
func TestKeepaliveIgnoresUnconfirmed(t *testing.T) {
	keepaliveFixture(t, 15, 4)
	ps := kaPlay("/m/a.mkv", kaHashA, time.Second)
	ps.IsHealthy = false
	if keepaliveHold(kaHashA) || keepaliveHold("") || keepaliveHold(kaHashD) {
		t.Fatal("held an unconfirmed or unknown torrent")
	}
	if stop, ceiling := kaReleases(); stop+ceiling != 0 || keepaliveCounters.evicted.Load() != 0 || keepaliveHeld() != 0 {
		t.Fatal("an unconfirmed torrent moved a keep-alive counter")
	}
}

// Two episodes of one pack: one stopped, the other still playing keeps the torrent; the
// stopped one's reads do not count.
func TestKeepaliveSiblingEpisodes(t *testing.T) {
	keepaliveFixture(t, 15, 4)
	ep1 := kaPlay("/m/e1.mkv", kaHashA, time.Minute)
	kaPlay("/m/e2.mkv", kaHashA, 20*time.Minute) // stale: past the ceiling alone
	if !keepaliveHold(kaHashA) {
		t.Fatal("pack with a live episode not held")
	}
	ep1.mu.Lock()
	ep1.IsHealthy, ep1.IsStopped = false, true
	ep1.mu.Unlock() // its reads are still recent, but it is stopped
	invalidateConfirmed()
	if keepaliveHold(kaHashA) {
		t.Fatal("held on the strength of a stopped episode's reads")
	}
}

// The cap holds the most recently active torrents; the rest lose and are counted once
// per episode as evicted, not as a release.
func TestKeepaliveCapRanksByRecency(t *testing.T) {
	keepaliveFixture(t, 15, 2)
	kaPlay("/m/a.mkv", kaHashA, 5*time.Minute)
	kaPlay("/m/b.mkv", kaHashB, time.Minute)
	kaPlay("/m/c.mkv", kaHashC, 3*time.Minute)

	if !keepaliveHold(kaHashB) || !keepaliveHold(kaHashC) {
		t.Fatal("the two most recently active were not held")
	}
	if keepaliveHold(kaHashA) || keepaliveHold(kaHashA) {
		t.Fatal("the least recently active was held over the cap")
	}
	if got := keepaliveCounters.evicted.Load(); got != 1 {
		t.Fatalf("evicted = %d, want 1 (once per episode)", got)
	}
	if stop, ceiling := kaReleases(); stop+ceiling != 0 {
		t.Fatal("a cap loss was counted as a release")
	}
	if keepaliveHeld() != 2 {
		t.Fatalf("held gauge = %d, want 2", keepaliveHeld())
	}
}

// A held torrent pushed out by a more recent one is counted as evicted, and leaves the
// gauge.
func TestKeepaliveCapEvictsAHeldTorrent(t *testing.T) {
	now := keepaliveFixture(t, 15, 1)
	kaPlay("/m/a.mkv", kaHashA, 2*time.Minute)
	if !keepaliveHold(kaHashA) {
		t.Fatal("not held")
	}
	*now = kaT0.Add(2 * time.Second)
	kaPlay("/m/b.mkv", kaHashB, 0)
	ps, _ := app.playbackRegistry.Load("/m/b.mkv")
	ps.(*PlaybackState).LastReadAt = kaT0.Add(time.Second)
	if keepaliveHold(kaHashA) {
		t.Fatal("older torrent kept over the cap")
	}
	if !keepaliveHold(kaHashB) {
		t.Fatal("newer torrent not held")
	}
	if got := keepaliveCounters.evicted.Load(); got != 1 {
		t.Fatalf("evicted = %d, want 1", got)
	}
	if stop, ceiling := kaReleases(); stop+ceiling != 0 {
		t.Fatal("an eviction was counted as a release")
	}
	if keepaliveHeld() != 1 {
		t.Fatalf("held gauge = %d, want 1", keepaliveHeld())
	}
}

// Minutes 0 is the one off switch: nothing is held, nothing is counted.
func TestKeepaliveOffWithZeroMinutes(t *testing.T) {
	keepaliveFixture(t, 0, 4)
	kaPlay("/m/a.mkv", kaHashA, time.Second)
	if keepaliveHold(kaHashA) {
		t.Fatal("held with the keep-alive off")
	}
	if stop, ceiling := kaReleases(); stop+ceiling != 0 || keepaliveCounters.evicted.Load() != 0 || keepaliveHeld() != 0 {
		t.Fatal("the off switch moved a counter")
	}
}

// Switching the keep-alive off live releases the held set without counting releases.
func TestKeepaliveSwitchedOffDropsHeld(t *testing.T) {
	keepaliveFixture(t, 15, 4)
	kaPlay("/m/a.mkv", kaHashA, time.Second)
	keepaliveHold(kaHashA)
	keepaliveSettings = func() (int, int) { return 0, 4 }
	if keepaliveHold(kaHashA) || keepaliveHeld() != 0 {
		t.Fatal("still held after the off switch")
	}
}

// A torrent no longer asked about (closed by something else) leaves the gauge.
func TestKeepaliveHeldGaugeForgetsTheUnasked(t *testing.T) {
	now := keepaliveFixture(t, 15, 4)
	kaPlay("/m/a.mkv", kaHashA, time.Second)
	keepaliveHold(kaHashA)
	*now = kaT0.Add(10 * time.Second)
	if got := keepaliveHeld(); got != 0 {
		t.Fatalf("gauge = %d for a torrent not asked about for 10 s", got)
	}
}

// A stop gives the torrent a 30 s grace in which the engine does not ask: the gauge drops
// at once, scrapes in the grace do not lose the held entry, and the release is counted
// when the engine asks again.
func TestKeepaliveStopReleaseSurvivesTheGrace(t *testing.T) {
	now := keepaliveFixture(t, 15, 4)
	ps := kaPlay("/m/a.mkv", kaHashA, time.Minute)
	if !keepaliveHold(kaHashA) {
		t.Fatal("not held")
	}
	ps.mu.Lock()
	ps.IsHealthy, ps.IsStopped = false, true
	ps.mu.Unlock()
	invalidateConfirmed()
	if got := keepaliveHeld(); got != 0 {
		t.Fatalf("gauge = %d right after the stop, want 0", got)
	}
	for _, d := range []time.Duration{10, 20} { // scrapes during the grace
		*now = kaT0.Add(d * time.Second)
		keepaliveHeld()
	}
	*now = kaT0.Add(31 * time.Second)
	if keepaliveHold(kaHashA) {
		t.Fatal("held after the grace")
	}
	if stop, ceiling := kaReleases(); stop != 1 || ceiling != 0 {
		t.Fatalf("releases stop=%d ceiling=%d, want 1 and 0", stop, ceiling)
	}
}

// The predicate runs under muTorrent on the engine's ticker, so it must not reach the
// engine: neither file may import the engine packages, and it works with no engine up.
func TestKeepalivePredicateIsEngineFree(t *testing.T) {
	for _, f := range []string{"keepalive.go", "confirmed_counters.go"} {
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			if strings.Contains(imp.Path.Value, "gostorm") {
				t.Errorf("%s imports %s: the hold predicate must not call into the engine", f, imp.Path.Value)
			}
		}
	}
	keepaliveFixture(t, 15, 4)
	kaPlay("/m/a.mkv", kaHashA, time.Second)
	if !keepaliveHold(kaHashA) { // web.BTS is nil in this test binary
		t.Fatal("not held")
	}
}

func TestKeepaliveStartupField(t *testing.T) {
	if got := keepaliveStartupField(0, 4, true); got != "keep-alive: off" {
		t.Errorf("off: %q", got)
	}
	got := keepaliveStartupField(15, 4, false)
	for _, w := range []string{"keep-alive: on", "15 min", "max 4 torrents", "inactive until a play webhook matches"} {
		if !strings.Contains(got, w) {
			t.Errorf("%q lacks %q", got, w)
		}
	}
	if got := keepaliveStartupField(15, 4, true); strings.Contains(got, "inactive") {
		t.Errorf("a matched webhook still reads inactive: %q", got)
	}
}
