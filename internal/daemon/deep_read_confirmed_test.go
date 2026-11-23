package daemon

import (
	"strings"
	"testing"
	"time"
)

func deepEnv(t *testing.T, confirmed bool) (*TTFFSession, *time.Time) {
	t.Helper()
	s, clock := stallEnv(t)
	if confirmed {
		app.playbackRegistry.Store("/m/s.mkv", &PlaybackState{Path: "/m/s.mkv", Hash: kaHashA, IsHealthy: true})
		invalidateConfirmed()
	}
	return s, clock
}

// The blocked time is the sum of the session's read durations from open up to and including
// the first deep read; reads after it add nothing.
func TestFirstDeepReadBlockedSum(t *testing.T) {
	s, clock := deepEnv(t, false)
	*clock = clock.Add(time.Second)
	ttffRead("/m/s.mkv", srcWarmupHead, 40*time.Millisecond, 4096, 0, stallRoleSecond) // the header
	*clock = clock.Add(time.Second)
	ttffRead("/m/s.mkv", srcFetchBlock, 3*time.Second, 4096, 1<<20, stallRoleSecond) // a slow shallow read
	if s.firstDeepReadAt.Load() != 0 {
		t.Fatal("a shallow read stamped the deep read")
	}
	*clock = clock.Add(time.Second)
	ttffRead("/m/s.mkv", srcFetchBlock, 20*time.Second, 4096, ttffDeepReadOff, stallRoleSecond) // the first deep read
	*clock = clock.Add(time.Second)
	ttffRead("/m/s.mkv", srcFetchBlock, 9*time.Second, 4096, ttffDeepReadOff+4096, stallRoleSecond) // after it
	want := 40*time.Millisecond + 3*time.Second + 20*time.Second
	if got := time.Duration(s.blockedNS.Load()); got != want {
		t.Fatalf("blocked = %v, want %v", got, want)
	}
}

// The confirmed label is read when the deep read happens, by the same rule as the stalls.
func TestFirstDeepReadConfirmedLabel(t *testing.T) {
	for _, conf := range []bool{false, true} {
		s, _ := deepEnv(t, conf)
		ttffRead("/m/s.mkv", srcFetchBlock, 2*time.Second, int(stallMiB), ttffDeepReadOff, stallRoleSecond)
		if got := s.deepConfirmed.Load(); got != conf {
			t.Errorf("confirmed=%v: session labelled %v", conf, got)
		}
	}
}

// A closed real playback lands in the histogram of its label, in both the open-to-deep-read
// and the blocked family; the unlabelled aggregate keeps counting every session.
func TestFirstDeepReadClosesIntoLabelledHistograms(t *testing.T) {
	for _, conf := range []bool{true, false} {
		i := confirmedIdx(conf)
		before := [2]int64{ttffStats.openToDeepReadBy[i].Count(), ttffStats.deepReadBlocked[i].Count()}
		agg := ttffStats.openToDeepRead.Count()
		s, clock := deepEnv(t, conf)
		*clock = clock.Add(time.Second)
		ttffRead("/m/s.mkv", srcFetchBlock, 50*time.Second, int(2*stallMiB), ttffDeepReadOff, stallRoleSecond)
		s.closeSession()
		if got := ttffStats.openToDeepReadBy[i].Count() - before[0]; got != 1 {
			t.Errorf("confirmed=%v: open-to-deep-read +%d, want 1", conf, got)
		}
		if got := ttffStats.deepReadBlocked[i].Count() - before[1]; got != 1 {
			t.Errorf("confirmed=%v: blocked +%d, want 1", conf, got)
		}
		if got := ttffStats.openToDeepRead.Count() - agg; got != 1 {
			t.Errorf("confirmed=%v: aggregate +%d, want 1", conf, got)
		}
	}
}

// 30, 60 and 120 s are bounds now, and every histogram sees them.
func TestTTFFBucketsReachTwoMinutes(t *testing.T) {
	var h TTFFHistogram
	h.Add(45 * time.Second)
	h.Add(100 * time.Second)
	h.Add(3 * time.Minute)
	counts, _ := h.snapshot()
	bounds := map[int64]int{}
	for i, b := range ttffBucketsMS {
		bounds[b] = i
	}
	for _, b := range []int64{30000, 60000, 120000} {
		if _, ok := bounds[b]; !ok {
			t.Fatalf("bound %d ms missing", b)
		}
	}
	if counts[bounds[60000]] != 1 || counts[bounds[120000]] != 1 || counts[len(ttffBucketsMS)] != 1 {
		t.Errorf("45 s, 100 s and 3 min landed in %v", counts)
	}
}

// Both families are exposed with the label from the start, with the new bounds.
func TestFirstDeepReadFamiliesExposed(t *testing.T) {
	body := scrape(t).Body.String()
	for _, want := range []string{
		"# TYPE flodni_session_first_deep_read_seconds histogram\n",
		"# TYPE flodni_session_first_deep_read_blocked_seconds histogram\n",
		`flodni_session_first_deep_read_seconds_bucket{confirmed="true",le="120"}`,
		`flodni_session_first_deep_read_seconds_bucket{confirmed="false",le="30"}`,
		`flodni_session_first_deep_read_seconds_count{confirmed="false"}`,
		`flodni_session_first_deep_read_blocked_seconds_bucket{confirmed="true",le="60"}`,
		`flodni_session_first_deep_read_blocked_seconds_sum{confirmed="false"}`,
		`flodni_session_first_deep_read_blocked_seconds_count{confirmed="true"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
}
