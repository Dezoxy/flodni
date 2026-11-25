package daemon

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

const playPath = "/m/x.mkv" // the path newTestSession registers

// playEnv isolates the play table, the sessions, the log and the clock, and confirms the
// path the way a play webhook does. It returns the log buffer's lines for [Play] and a
// function that moves the clock.
func playEnv(t *testing.T, confirm bool) (playLines func() []string, advance func(time.Duration)) {
	t.Helper()
	readabilityEnv(t, 0, false)
	buf := captureSessionLog(t)
	plays.mu.Lock()
	plays.m = nil
	plays.mu.Unlock()
	t.Cleanup(func() {
		plays.mu.Lock()
		plays.m = nil
		plays.mu.Unlock()
		app.playbackRegistry.Delete(playPath)
		app.activePumps.Delete(playPath)
		invalidateConfirmed()
	})
	if confirm {
		app.playbackRegistry.Store(playPath, &PlaybackState{Hash: rHash, IsHealthy: true})
		invalidateConfirmed()
	}
	now := time.Now()
	prev := ttffNow
	ttffNow = func() time.Time { return now }
	t.Cleanup(func() { ttffNow = prev })
	return func() []string { return linesWith(buf, "[Play]") },
		func(d time.Duration) { now = now.Add(d) }
}

// playFields parses a text [Play] line into its key=value fields.
func playFields(t *testing.T, line string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range strings.Fields(strings.TrimPrefix(line, "[Play]")) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			t.Fatalf("field %q has no =: %s", f, line)
		}
		out[k] = v
	}
	return out
}

func wantField(t *testing.T, f map[string]string, key, want string) {
	t.Helper()
	if f[key] != want {
		t.Errorf("%s = %q, want %q (line fields %v)", key, f[key], want, f)
	}
}

func playStarted(t *testing.T) *TTFFSession {
	t.Helper()
	s := newTestSession(time.Minute)
	s.id = sessionSeq.Add(1) // a real open takes one; resumed_from 0 means fresh
	s.openedAt.Store(ttffNow().UnixNano())
	return s
}

func TestPlayRecordCountsStallsByOriginAndRole(t *testing.T) {
	lines, advance := playEnv(t, true)
	s := playStarted(t)
	s.confirmed.Store(true)
	ps := &NativePumpState{}
	ps.pumpOff.Store(50)
	app.activePumps.Store(playPath, ps)

	advance(1500 * time.Millisecond)
	// Read at 100 with the pump at 50: it overtook the pump (pump_behind), primary.
	ttffRead(playPath, srcFetchBlock, 2*time.Second, 2<<20, 100, stallRolePrimary)
	advance(10 * time.Second)
	// Read at 0 with the pump at 50: the pump was ahead (swarm), second handle.
	ttffRead(playPath, srcFetchBlock, 3*time.Second, 1024, 0, stallRoleSecond)

	stopPlay(playPath)
	if _, open := sessions.Load(playPath); !open || s.closed.Load() {
		t.Fatal("the stop closed the session")
	}

	got := lines()
	if len(got) != 1 {
		t.Fatalf("[Play] lines = %d, want 1: %v", len(got), got)
	}
	// The session's later natural close folds nothing and emits nothing more.
	s.closeSession()
	if again := lines(); len(again) != 1 {
		t.Fatalf("[Play] lines after the natural close = %d, want 1: %v", len(again), again)
	}
	f := playFields(t, got[0])
	wantField(t, f, "hash", rHash[:8])
	wantField(t, f, "file", "x.mkv")
	wantField(t, f, "start_ms", "1500")
	wantField(t, f, "watch_s", "10")
	wantField(t, f, "stalls", "2")
	wantField(t, f, "stalls_pump_behind", "1")
	wantField(t, f, "stalls_swarm", "1")
	wantField(t, f, "stalls_evicted", "0")
	wantField(t, f, "stalls_no_pump", "0")
	wantField(t, f, "stalls_primary", "1")
	wantField(t, f, "stalls_second", "1")
	wantField(t, f, "stalled_s", "5")
	wantField(t, f, "sessions", "1")
	wantField(t, f, "resumed", "false")
	wantField(t, f, "bytes_fetch_block", strconv.Itoa(2<<20+1024))
}

func TestPlayRecordOnceForStopAndNaturalClose(t *testing.T) {
	for _, order := range []string{"stop then close", "close then stop"} {
		t.Run(order, func(t *testing.T) {
			lines, advance := playEnv(t, true)
			s := playStarted(t)
			s.confirmed.Store(true)
			advance(time.Second)
			ttffRead(playPath, srcFetchBlock, time.Millisecond, 2<<20, 0, stallRolePrimary)

			if order == "stop then close" {
				stopPlay(playPath)
				s.closeSession()
			} else {
				s.closeSession()
				stopPlay(playPath)
			}
			if got := lines(); len(got) != 1 {
				t.Fatalf("[Play] lines = %d, want 1: %v", len(got), got)
			}
		})
	}
}

func TestPlayRecordNaturalCloseWithoutStopEmitsOnce(t *testing.T) {
	lines, advance := playEnv(t, true)
	s := playStarted(t)
	s.confirmed.Store(true)
	advance(time.Second)
	ttffRead(playPath, srcFetchBlock, 2*time.Second, 2<<20, 0, stallRolePrimary)
	s.closeSession() // the idle close, no handle open
	s.closeSession()
	got := lines()
	if len(got) != 1 {
		t.Fatalf("[Play] lines = %d, want 1: %v", len(got), got)
	}
	wantField(t, playFields(t, got[0]), "stalls", "1")
}

func openSessions() int {
	n := 0
	sessions.Range(func(_, _ any) bool { n++; return true })
	return n
}

// A seek-style restart: the handle is released and reopened inside the idle window. The
// release closes nothing, so the session stays open and the viewing stays one play.
func TestPlayRecordReleaseAndReopenIsOnePlayAndClosesNothing(t *testing.T) {
	lines, advance := playEnv(t, true)
	s := playStarted(t)
	s.confirmed.Store(true)
	advance(time.Second)
	ttffRead(playPath, srcFetchBlock, time.Millisecond, 2<<20, 0, stallRolePrimary)
	openBefore := openSessions()

	ttffReleaseClose(playPath) // what Release calls
	if got := openSessions(); got != openBefore || s.closed.Load() {
		t.Fatalf("the release changed the open sessions: %d -> %d, closed=%v", openBefore, got, s.closed.Load())
	}
	if got := lines(); len(got) != 0 {
		t.Fatalf("a release logged %v", got)
	}

	advance(2 * time.Second) // the reopened handle keeps reading the same session
	ttffRead(playPath, srcFetchBlock, 2*time.Second, 2<<20, 0, stallRolePrimary)
	if cur, ok := sessions.Load(playPath); !ok || cur.(*TTFFSession) != s {
		t.Fatal("the reopen did not continue the same session")
	}
	s.closeSession() // the idle close
	got := lines()
	if len(got) != 1 {
		t.Fatalf("[Play] lines = %d, want 1: %v", len(got), got)
	}
	f := playFields(t, got[0])
	wantField(t, f, "sessions", "1")
	wantField(t, f, "stalls", "1")
}

func TestPlayRecordNoneForUnconfirmedSession(t *testing.T) {
	lines, advance := playEnv(t, false)
	before := playStats.byStalled[0].Load() + playStats.byStalled[1].Load()
	s := playStarted(t) // confirmed stays false: a scan or probe
	advance(time.Second)
	ttffRead(playPath, srcFetchBlock, 2*time.Second, 2<<20, 0, stallRolePrimary)
	s.closeSession()
	stopPlay(playPath)

	if got := lines(); len(got) != 0 {
		t.Fatalf("an unconfirmed session logged %v", got)
	}
	if after := playStats.byStalled[0].Load() + playStats.byStalled[1].Load(); after != before {
		t.Fatalf("plays_total moved %d -> %d for an unconfirmed session", before, after)
	}
}

func TestPlayRecordNoneForConfirmedProbe(t *testing.T) {
	lines, advance := playEnv(t, true)
	s := playStarted(t)
	s.confirmed.Store(true)
	advance(time.Second)
	ttffRead(playPath, srcFetchBlock, time.Millisecond, 100, 0, stallRolePrimary) // far under 1 MiB
	stopPlay(playPath)
	s.closeSession()
	if got := lines(); len(got) != 0 {
		t.Fatalf("a probe of a confirmed file logged %v", got)
	}
}

func TestPlayRecordResumedMergesSessions(t *testing.T) {
	lines, advance := playEnv(t, true)
	s1 := playStarted(t)
	s1.confirmed.Store(true)
	advance(time.Second)
	ttffRead(playPath, srcFetchBlock, time.Millisecond, 2<<20, 0, stallRolePrimary)
	globalOpenTracker.Inc(rHash, playPath) // a pause: the player still holds the handle
	t.Cleanup(func() { globalOpenTracker.Dec(rHash, playPath) })
	s1.closeSession() // the idle close; the play goes on
	if got := lines(); len(got) != 0 {
		t.Fatalf("a pause ended the play: %v", got)
	}

	advance(5 * time.Minute)
	ttffRead(playPath, srcFetchBlock, 2*time.Second, 2<<20, 0, stallRolePrimary) // resumes
	v, ok := sessions.Load(playPath)
	if !ok || v.(*TTFFSession).resumedFrom != s1.id {
		t.Fatalf("the read did not resume session %d", s1.id)
	}
	globalOpenTracker.Dec(rHash, playPath) // the player lets go
	v.(*TTFFSession).closeSession()
	globalOpenTracker.Inc(rHash, playPath) // restored for the cleanup's Dec

	got := lines()
	if len(got) != 1 {
		t.Fatalf("[Play] lines = %d, want 1: %v", len(got), got)
	}
	f := playFields(t, got[0])
	wantField(t, f, "sessions", "2")
	wantField(t, f, "resumed", "true")
	wantField(t, f, "stalls", "1")
	wantField(t, f, "start_ms", "1000")

	// A read after the play ended starts a new play, not a resumed one.
	advance(time.Minute)
	app.playbackRegistry.Store(playPath, &PlaybackState{Hash: rHash, IsHealthy: true})
	invalidateConfirmed()
	ttffRead(playPath, srcFetchBlock, time.Millisecond, 2<<20, 0, stallRolePrimary)
	stopPlay(playPath)
	got = lines()
	if len(got) != 2 {
		t.Fatalf("[Play] lines = %d, want 2: %v", len(got), got)
	}
	f = playFields(t, got[1])
	wantField(t, f, "sessions", "1")
	wantField(t, f, "resumed", "false")
}

func TestPlayCountersMove(t *testing.T) {
	_, advance := playEnv(t, true)
	stalled0, clean0 := playStats.byStalled[1].Load(), playStats.byStalled[0].Load()
	start0 := playStats.start.Count()

	// A play with no stall.
	s := playStarted(t)
	s.confirmed.Store(true)
	advance(time.Second)
	ttffRead(playPath, srcFetchBlock, time.Millisecond, 2<<20, 0, stallRolePrimary)
	stopPlay(playPath)
	s.closeSession()
	if playStats.byStalled[0].Load() != clean0+1 || playStats.byStalled[1].Load() != stalled0 {
		t.Fatalf("after a clean play: stalled=false %d (want %d), stalled=true %d (want %d)",
			playStats.byStalled[0].Load(), clean0+1, playStats.byStalled[1].Load(), stalled0)
	}

	// A play with one stall.
	s = playStarted(t)
	s.confirmed.Store(true)
	advance(time.Second)
	ttffRead(playPath, srcFetchBlock, 2*time.Second, 2<<20, 0, stallRolePrimary)
	stopPlay(playPath)
	s.closeSession()
	if playStats.byStalled[0].Load() != clean0+1 || playStats.byStalled[1].Load() != stalled0+1 {
		t.Fatalf("after a stalled play: stalled=false %d (want %d), stalled=true %d (want %d)",
			playStats.byStalled[0].Load(), clean0+1, playStats.byStalled[1].Load(), stalled0+1)
	}
	if got := playStats.start.Count(); got != start0+2 {
		t.Fatalf("play start histogram count = %d, want %d", got, start0+2)
	}
}
