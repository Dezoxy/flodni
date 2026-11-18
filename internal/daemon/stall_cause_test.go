package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"flodni/internal/config"
)

const stallMiB = int64(1 << 20)

// stallEnv registers a session for one path and gives the test a clock it moves by
// hand. ttffNow is the read's end time; a read that took d began at ttffNow()-d.
func stallEnv(t *testing.T) (*TTFFSession, *time.Time) {
	t.Helper()
	resetKeepaliveState(t)
	clock := time.Unix(1_700_000_000, 0)
	prev := ttffNow
	ttffNow = func() time.Time { return clock }
	t.Cleanup(func() { ttffNow = prev })
	prevCfg := globalConfig.Load()
	globalConfig.Store(&config.Config{ReadAheadBudget: 64 << 20}) // a jump is more than this
	t.Cleanup(func() { globalConfig.Store(prevCfg) })
	ttffRegister("/m/s.mkv", 8<<30, kaHashA, 0, false, false)
	v, ok := sessions.Load("/m/s.mkv")
	if !ok {
		t.Fatal("no session")
	}
	return v.(*TTFFSession), &clock
}

// stallCounts is the unconfirmed row of the counter: playback, then seek.
func stallCounts() (playback, seek int64) {
	return keepaliveCounters.stalls[0][stallCausePlayback].Load(), keepaliveCounters.stalls[0][stallCauseSeek].Load()
}

func stallRead(src ttffSource, d time.Duration, off int64) {
	ttffRead("/m/s.mkv", src, d, int(stallMiB), off)
}

const slow = 2 * time.Second // over ttffStallDuration

// Sequential reading that stalls is playback: a slow swarm, not a seek.
func TestStallCauseSequentialIsPlayback(t *testing.T) {
	s, clock := stallEnv(t)
	stallRead(srcFetchBlock, time.Millisecond, 500*stallMiB)
	*clock = clock.Add(time.Second)
	stallRead(srcFetchBlock, time.Millisecond, 501*stallMiB)
	*clock = clock.Add(time.Second)
	stallRead(srcFetchBlock, slow, 502*stallMiB)
	if p, k := stallCounts(); p != 1 || k != 0 {
		t.Fatalf("playback=%d seek=%d, want 1 and 0", p, k)
	}
	if s.stallCount.Load() != 1 || s.seekStalls.Load() != 0 {
		t.Fatalf("session stalls=%d seek_stalls=%d, want 1 and 0", s.stallCount.Load(), s.seekStalls.Load())
	}
}

// A stall on the read that jumps, or on the reads after it, is a seek stall, for a
// jump forward and for one backward.
func TestStallCauseAfterJumpIsSeek(t *testing.T) {
	for _, tc := range []struct {
		name string
		to   int64
	}{{"forward", 4000 * stallMiB}, {"backward", 50 * stallMiB}} {
		t.Run(tc.name, func(t *testing.T) {
			s, clock := stallEnv(t)
			stallRead(srcFetchBlock, time.Millisecond, 500*stallMiB)
			*clock = clock.Add(time.Second)
			stallRead(srcFetchBlock, slow, tc.to) // the jumping read itself stalls
			*clock = clock.Add(time.Second)
			stallRead(srcFetchBlock, slow, tc.to+stallMiB) // still cold, still a seek
			if p, k := stallCounts(); p != 0 || k != 2 {
				t.Fatalf("playback=%d seek=%d, want 0 and 2", p, k)
			}
			if s.stallCount.Load() != 2 || s.seekStalls.Load() != 2 {
				t.Fatalf("session stalls=%d seek_stalls=%d, want 2 and 2", s.stallCount.Load(), s.seekStalls.Load())
			}
		})
	}
}

// The first read the cache serves after the jump ends the seek: a stall after it is
// playback.
func TestStallCauseAfterCacheServedReadIsPlayback(t *testing.T) {
	s, clock := stallEnv(t)
	stallRead(srcFetchBlock, time.Millisecond, 500*stallMiB)
	*clock = clock.Add(time.Second)
	stallRead(srcFetchBlock, time.Millisecond, 4000*stallMiB) // jump, armed
	*clock = clock.Add(time.Second)
	stallRead(srcRACacheHit, time.Millisecond, 4001*stallMiB) // cache serves: disarmed
	*clock = clock.Add(time.Second)
	stallRead(srcFetchBlock, slow, 4002*stallMiB)
	if p, k := stallCounts(); p != 1 || k != 0 {
		t.Fatalf("playback=%d seek=%d, want 1 and 0", p, k)
	}
	if s.seekStalls.Load() != 0 {
		t.Fatalf("seek_stalls = %d, want 0", s.seekStalls.Load())
	}
	// A second jump arms it again.
	*clock = clock.Add(time.Second)
	stallRead(srcFetchBlock, slow, 100*stallMiB)
	if p, k := stallCounts(); p != 1 || k != 1 {
		t.Fatalf("after a second jump playback=%d seek=%d, want 1 and 1", p, k)
	}
}

// Past 15 s a stall is playback even with no cache-served read in between. The window
// runs from the start of the jumping read to the start of the stalled read.
func TestStallCauseSeekWindowIsFifteenSeconds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		gap      time.Duration // from the jumping read's end to the stalled read's end
		wantSeek bool
	}{
		{"inside", 10 * time.Second, true},
		{"at the cap", 15*time.Second + slow - time.Millisecond, true},
		{"just past", 15*time.Second + slow, false},
		{"long past", time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, clock := stallEnv(t)
			stallRead(srcFetchBlock, time.Millisecond, 500*stallMiB)
			*clock = clock.Add(time.Second)
			stallRead(srcFetchBlock, time.Millisecond, 4000*stallMiB) // the jump, quick
			*clock = clock.Add(tc.gap)
			stallRead(srcFetchBlock, slow, 4001*stallMiB)
			p, k := stallCounts()
			if tc.wantSeek && (p != 0 || k != 1) || !tc.wantSeek && (p != 1 || k != 0) {
				t.Fatalf("playback=%d seek=%d, wantSeek=%v", p, k, tc.wantSeek)
			}
		})
	}
}

// All four series exist from the start, with the cause text in the HELP.
func TestStallCauseMetricHasFourSeries(t *testing.T) {
	resetKeepaliveState(t)
	body := scrape(t).Body.String()
	for _, want := range []string{
		`flodni_read_stalls_total{confirmed="true",cause="seek"} 0`,
		`flodni_read_stalls_total{confirmed="true",cause="playback"} 0`,
		`flodni_read_stalls_total{confirmed="false",cause="seek"} 0`,
		`flodni_read_stalls_total{confirmed="false",cause="playback"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("series missing: %s", want)
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "# HELP flodni_read_stalls_total ") && !strings.Contains(line, "15 s") {
			t.Errorf("HELP does not state the 15 s rule: %s", line)
		}
	}
	keepaliveCounters.stalls[1][stallCauseSeek].Add(3)
	if body = scrape(t).Body.String(); !strings.Contains(body, `flodni_read_stalls_total{confirmed="true",cause="seek"} 3`) {
		t.Error("confirmed seek series did not count")
	}
}

// /api/sessions carries stalls_after_seek next to stalls; the [TTFF] close line ends
// with seek_stalls.
func TestStallCauseSessionFieldAndTTFFLine(t *testing.T) {
	s, clock := stallEnv(t)
	stallRead(srcFetchBlock, time.Millisecond, 500*stallMiB)
	*clock = clock.Add(time.Second)
	stallRead(srcFetchBlock, slow, 4000*stallMiB)
	*clock = clock.Add(time.Minute)
	stallRead(srcFetchBlock, slow, 4001*stallMiB) // playback
	r := s.record(time.Time{}, false)
	if r.Stalls != 2 || r.StallsAfterSeek != 1 {
		t.Fatalf("record stalls=%d stalls_after_seek=%d, want 2 and 1", r.Stalls, r.StallsAfterSeek)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"stalls":2,`) || !strings.Contains(string(b), `"stalls_after_seek":1`) {
		t.Fatalf("json = %s", b)
	}

	buf := captureSessionLog(t)
	s.bytesRead.Store(2 << 20)
	s.closeSession()
	got := linesWith(buf, "[TTFF]")
	if len(got) != 1 || !strings.Contains(got[0], " stalls=2 bytes=") || !strings.HasSuffix(got[0], " seek_stalls=1") {
		t.Fatalf("[TTFF] line = %q", got)
	}
}
