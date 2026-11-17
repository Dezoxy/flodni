package torr

import (
	"sync"
	"testing"
	"time"

	"flodni/internal/gostorm/settings"
)

// engine_cold_boost: a torrent below the cold-start connection count gets more connections
// until data flows or the boost times out, then the configured limit comes back exactly once.

type fakeConns struct {
	mu    sync.Mutex
	calls []int
}

func (f *fakeConns) set(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, n)
}

func (f *fakeConns) got() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.calls...)
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func wantCalls(t *testing.T, f *fakeConns, want ...int) {
	t.Helper()
	if got := f.got(); !equalInts(got, want) {
		t.Fatalf("SetMaxEstablishedConns calls = %v, want %v", got, want)
	}
}

var t0 = time.Unix(1_700_000_000, 0)

func TestColdBoostRaisesWhenBelowTheColdStartCount(t *testing.T) {
	var b coldBoost
	f := &fakeConns{}
	if !b.begin(25, 0, t0, f.set) {
		t.Fatal("begin refused a limit of 25")
	}
	wantCalls(t, f, 35)
}

func TestColdBoostDoesNotRaiseAtOrAboveTheColdStartCount(t *testing.T) {
	for _, limit := range []int{35, 36, 200} {
		var b coldBoost
		f := &fakeConns{}
		if b.begin(limit, 0, t0, f.set) {
			t.Fatalf("limit %d: boost began", limit)
		}
		b.tick(1<<30, t0.Add(time.Hour), limit)
		b.cancel(limit)
		wantCalls(t, f)
	}
}

func TestColdBoostRestoresAfterOneMiBOfUsefulData(t *testing.T) {
	var b coldBoost
	f := &fakeConns{}
	b.begin(25, 5000, t0, f.set)
	b.tick(5000+coldBoostEndBytes-1, t0.Add(time.Second), 25)
	wantCalls(t, f, 35)
	b.tick(5000+coldBoostEndBytes, t0.Add(2*time.Second), 25)
	wantCalls(t, f, 35, 25)
	b.tick(1<<40, t0.Add(time.Hour), 25)
	wantCalls(t, f, 35, 25)
}

func TestColdBoostRestoresAfterThirtySeconds(t *testing.T) {
	var b coldBoost
	f := &fakeConns{}
	b.begin(25, 0, t0, f.set)
	b.tick(10, t0.Add(coldBoostMaxAge-time.Nanosecond), 25)
	wantCalls(t, f, 35)
	b.tick(10, t0.Add(coldBoostMaxAge), 25)
	wantCalls(t, f, 35, 25)
}

func TestColdBoostRestoresTheLimitThatIsConfiguredNow(t *testing.T) {
	var b coldBoost
	f := &fakeConns{}
	b.begin(25, 0, t0, f.set)
	b.tick(coldBoostEndBytes, t0.Add(time.Second), 40)
	wantCalls(t, f, 35, 40)
}

func TestColdBoostRestoresOnceWhenTheTorrentClosesDuringTheBoost(t *testing.T) {
	var b coldBoost
	f := &fakeConns{}
	b.begin(25, 0, t0, f.set)
	b.cancel(25)
	b.cancel(25)
	b.tick(coldBoostEndBytes, t0.Add(time.Hour), 25)
	wantCalls(t, f, 35, 25)
}

func TestColdBoostCloseWithoutABoostChangesNothing(t *testing.T) {
	var b coldBoost
	f := &fakeConns{}
	b.cancel(25)
	b.tick(1<<30, t0, 25)
	wantCalls(t, f)
}

func TestColdBoostRunsOncePerActivation(t *testing.T) {
	var b coldBoost
	f := &fakeConns{}
	b.begin(25, 0, t0, f.set)
	if b.begin(25, 0, t0, f.set) {
		t.Fatal("second begin while active raised again")
	}
	b.tick(coldBoostEndBytes, t0, 25)
	if b.begin(25, 0, t0, f.set) {
		t.Fatal("a finished boost began again on the same wrapper")
	}
	wantCalls(t, f, 35, 25)
	// A re-woken torrent is a new wrapper with a new state.
	var fresh coldBoost
	g := &fakeConns{}
	if !fresh.begin(25, 0, t0, g.set) {
		t.Fatal("a re-woken torrent did not get its boost")
	}
}

func TestColdBoostRestoreRacesCloseExactlyOnce(t *testing.T) {
	for i := 0; i < 200; i++ {
		var b coldBoost
		f := &fakeConns{}
		b.begin(25, 0, t0, f.set)
		var wg sync.WaitGroup
		for j := 0; j < 4; j++ {
			wg.Add(2)
			go func() { defer wg.Done(); b.tick(coldBoostEndBytes, t0, 25) }()
			go func() { defer wg.Done(); b.cancel(25) }()
		}
		wg.Wait()
		wantCalls(t, f, 35, 25)
	}
}

func TestColdBoostFlagOffNeverRaises(t *testing.T) {
	prev := settings.BTsets
	settings.BTsets = &settings.BTSets{ConnectionsLimit: 25}
	defer func() { settings.BTsets = prev }()

	EngineColdBoost = false
	tr := &Torrent{}
	f := &fakeConns{}
	if tr.startColdBoost(0, t0, f.set) {
		t.Fatal("boost began with engine_cold_boost off")
	}
	wantCalls(t, f)

	EngineColdBoost = true
	defer func() { EngineColdBoost = false }()
	if !tr.startColdBoost(0, t0, f.set) {
		t.Fatal("boost did not begin with engine_cold_boost on")
	}
	wantCalls(t, f, 35)
}
