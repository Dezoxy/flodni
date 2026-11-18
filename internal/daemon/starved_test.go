package daemon

import (
	"strings"
	"testing"
	"time"

	"flodni/internal/gostorm/native"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

var starveT0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return starveT0.Add(time.Duration(sec) * time.Second) }

// starved is an observation under the peer floor with the read stalls so far.
func starved(stalls int64) starveObs { return starveObs{peers: 1, stalls: stalls} }

// stepAll feeds one observation per 5 s tick from start to end (seconds) and returns the
// seconds at which step said to announce.
func stepAll(st *starveState, from, to int, obs func(sec int) starveObs) []int {
	var fires []int
	for s := from; s <= to; s += 5 {
		if st.step(at(s), obs(s)) {
			fires = append(fires, s)
		}
	}
	return fires
}

// A starved torrent is announced once, after the window, and not before it.
func TestStarveFiresAfterTheWindowNotBefore(t *testing.T) {
	fires := stepAll(&starveState{}, 0, 120, func(s int) starveObs { return starved(int64(s / 5)) })
	if len(fires) != 1 || fires[0] != int(starvedWindow/time.Second) {
		t.Fatalf("fires at %v s, want exactly one at %v", fires, int(starvedWindow/time.Second))
	}
}

// Low peers alone are not enough: it needs evidence the player feels it.
func TestStarveNeedsEvidenceOfAStarvingPlayer(t *testing.T) {
	flat := func(int) starveObs {
		return starveObs{peers: 1, stalls: 4, ahead: 8 << 20, aheadOK: true, aheadMissing: true, reads: 9}
	}
	if f := stepAll(&starveState{}, 0, 600, flat); len(f) != 0 {
		t.Errorf("no stall and a steady ahead announced at %v", f)
	}
	shrinking := func(s int) starveObs {
		return starveObs{peers: 1, stalls: 4, ahead: int64(8<<20 - s*1000), aheadOK: true, aheadMissing: true, reads: int64(s)}
	}
	if f := stepAll(&starveState{}, 0, 90, shrinking); len(f) != 1 {
		t.Errorf("a shrinking ahead with bytes missing announced at %v, want once", f)
	}
	unknownAhead := func(int) starveObs { return starveObs{peers: 1, stalls: 4} }
	if f := stepAll(&starveState{}, 0, 600, unknownAhead); len(f) != 0 {
		t.Errorf("an unknown ahead and no stall announced at %v", f)
	}
}

// False trigger: ahead is clamped to the file's end, so it shrinks through the last
// stretch of a fully buffered file. That is a healthy play, not starvation.
func TestStarveShrinkingAheadNeedsBytesMissing(t *testing.T) {
	endOfBufferedFile := func(s int) starveObs {
		return starveObs{peers: 1, stalls: 0, ahead: int64(20<<20 - s*1000), aheadOK: true, aheadMissing: false, reads: int64(s)}
	}
	if f := stepAll(&starveState{}, 0, 300, endOfBufferedFile); len(f) != 0 {
		t.Errorf("a fully buffered file's last stretch announced at %v", f)
	}
	// Missing bytes but a comfortable buffer (over the low-water mark) is not starvation either.
	comfortable := func(s int) starveObs {
		return starveObs{peers: 1, ahead: int64(starvedLowWater + 100<<20 - s*1000), aheadOK: true, aheadMissing: true, reads: int64(s)}
	}
	if f := stepAll(&starveState{}, 0, 300, comfortable); len(f) != 0 {
		t.Errorf("a buffer over the low-water mark announced at %v", f)
	}
}

// Missed trigger: a read that hangs never completes, so it never counts as a stall. A read
// in flight past the stall threshold, or an empty buffer with no read progress, is evidence.
func TestStarveEvidenceFromAHungReadOrAnEmptyBuffer(t *testing.T) {
	hung := func(int) starveObs { return starveObs{peers: 0, stuck: true} }
	if f := stepAll(&starveState{}, 0, 90, hung); len(f) != 1 {
		t.Errorf("a read in flight past the threshold announced at %v, want once", f)
	}
	empty := func(int) starveObs {
		return starveObs{peers: 0, ahead: 0, aheadOK: true, aheadMissing: true, reads: 7}
	}
	if f := stepAll(&starveState{}, 0, 90, empty); len(f) != 1 {
		t.Errorf("an empty buffer with no read progress announced at %v, want once", f)
	}
	progressing := func(s int) starveObs {
		return starveObs{peers: 0, ahead: 0, aheadOK: true, aheadMissing: true, reads: int64(s)}
	}
	if f := stepAll(&starveState{}, 0, 90, progressing); len(f) != 0 {
		t.Errorf("an empty buffer whose reads progress announced at %v", f)
	}
	atEnd := func(int) starveObs {
		return starveObs{peers: 0, ahead: 0, aheadOK: true, aheadMissing: false, reads: 7}
	}
	if f := stepAll(&starveState{}, 0, 90, atEnd); len(f) != 0 {
		t.Errorf("the end of the file announced at %v", f)
	}
}

// A swarm at or over the floor never announces, and a recovery restarts the window.
func TestStarveNotForAHealthySwarm(t *testing.T) {
	healthy := func(s int) starveObs { return starveObs{peers: starvedPeerFloor, stalls: int64(s)} }
	if f := stepAll(&starveState{}, 0, 600, healthy); len(f) != 0 {
		t.Errorf("a swarm at the peer floor announced at %v", f)
	}
	st := &starveState{}
	stepAll(st, 0, 50, func(s int) starveObs { return starved(int64(s)) })
	st.step(at(55), starveObs{peers: 5, stalls: 55})
	if f := stepAll(st, 60, 110, func(s int) starveObs { return starved(int64(s)) }); len(f) != 0 {
		t.Errorf("the window survived a recovery: fired at %v", f)
	}
}

func gaps(fires []int) []int {
	var g []int
	for i := 1; i < len(fires); i++ {
		g = append(g, fires[i]-fires[i-1])
	}
	return g
}

// Backoff on repeated starvation: 2 minutes, then 4, 8, 16, never more than 30.
func TestStarveBackoff(t *testing.T) {
	fires := stepAll(&starveState{}, 0, 4*3600, func(s int) starveObs { return starved(int64(s)) })
	g := gaps(fires)
	if len(g) < 6 {
		t.Fatalf("fires = %v", fires)
	}
	for i, want := range []int{120, 240, 480, 960, 1800, 1800} {
		if g[i] < want || g[i] > want+5 {
			t.Errorf("gap %d = %d s, want %d..%d", i, g[i], want, want+5)
		}
	}
}

// A torrent that recovers for a while starts again from the shortest gap.
func TestStarveBackoffResetsAfterRecovery(t *testing.T) {
	st := &starveState{}
	stepAll(st, 0, 700, func(s int) starveObs { return starved(int64(s)) })
	if st.fires < 2 {
		t.Fatalf("setup: fires = %d", st.fires)
	}
	for s := 700; s <= 700+int(starvedRecovery/time.Second)+10; s += 5 {
		st.step(at(s), starveObs{peers: 8})
	}
	if st.fires != 0 {
		t.Errorf("fires = %d after a recovery, want 0", st.fires)
	}
}

// fakeStarveTarget sends nothing (returns 0, a deferral) for its first defers calls, then one
// announce per call.
type fakeStarveTarget struct {
	defers int
	calls  int
	panics bool
}

func (f *fakeStarveTarget) Announce(starveObs) int {
	f.calls++
	if f.panics {
		panic("boom")
	}
	if f.calls <= f.defers {
		return 0
	}
	return 1
}

func newTestController(targets map[string]*fakeStarveTarget) *starveController {
	return newStarveController(func(hash string) starveTarget {
		if t, ok := targets[hash]; ok && t != nil {
			return t
		}
		return nil
	})
}

// One announce for a starved confirmed public torrent after the window, none before, none
// for a healthy or a not-playing one.
func TestStarveControllerAnnouncesOnceAndOnlyTheStarved(t *testing.T) {
	reannounces.Store(0)
	pub := &fakeStarveTarget{}
	fine := &fakeStarveTarget{}
	idle := &fakeStarveTarget{} // in the engine but never sampled
	c := newTestController(map[string]*fakeStarveTarget{"pub": pub, "fine": fine, "idle": idle})
	for s := 0; s <= 120; s += 5 {
		c.tick(at(s), []starveSample{
			{hash: "pub", obs: starved(int64(s))},
			{hash: "fine", obs: starveObs{peers: 10, stalls: int64(s)}},
		})
		if s < int(starvedWindow/time.Second) && pub.calls != 0 {
			t.Fatalf("announced at %d s, before the window", s)
		}
	}
	if pub.calls != 1 || fine.calls != 0 || idle.calls != 0 {
		t.Errorf("announces: starved %d, healthy %d, not playing %d, want 1/0/0", pub.calls, fine.calls, idle.calls)
	}
	if reannounces.Load() != 1 {
		t.Errorf("counter = %d, want 1", reannounces.Load())
	}
}

// A private torrent whose announce is deferred (the tracker's min interval has not passed, or
// it sent none) is asked again every tick once the window is over: nothing is counted and no
// backoff is consumed. The announce that finally goes out is counted once and starts the
// backoff, so the next one waits the first gap from THAT send.
func TestStarveControllerDeferralConsumesNoBackoff(t *testing.T) {
	reannounces.Store(0)
	const defers = 10
	priv := &fakeStarveTarget{defers: defers}
	c := newTestController(map[string]*fakeStarveTarget{"p": priv})
	sentAt, secondAt := -1, -1
	for s := 0; s <= 1200; s += 5 {
		before := priv.calls
		c.tick(at(s), []starveSample{{hash: "p", obs: starved(int64(s))}})
		if priv.calls > before && priv.calls > defers {
			if sentAt < 0 {
				sentAt = s
				if got := reannounces.Load(); got != 1 {
					t.Fatalf("counter = %d at the first send, want 1 (deferrals uncounted)", got)
				}
			} else if secondAt < 0 {
				secondAt = s
			}
		}
		if priv.calls <= defers && reannounces.Load() != 0 {
			t.Fatalf("counter = %d during deferral at %d s", reannounces.Load(), s)
		}
	}
	// Window over at 60 s; deferred on each of the 10 ticks from 60 s, sent on the 11th.
	if sentAt != 60+5*defers {
		t.Errorf("first send at %d s, want %d", sentAt, 60+5*defers)
	}
	if want := sentAt + int(starvedGap/time.Second); secondAt != want {
		t.Errorf("second send at %d s, want %d (the first gap after the send)", secondAt, want)
	}
	if got := priv.calls - defers; reannounces.Load() != int64(got) {
		t.Errorf("counter = %d, want %d sends", reannounces.Load(), got)
	}
}

// A target that never sends (metadata unknown, or a tracker without a min interval) is never
// counted, however long it is starved.
func TestStarveControllerNeverSendingTargetIsNeverCounted(t *testing.T) {
	reannounces.Store(0)
	never := &fakeStarveTarget{defers: 1 << 30}
	c := newTestController(map[string]*fakeStarveTarget{"n": never})
	for s := 0; s <= 1200; s += 5 {
		c.tick(at(s), []starveSample{{hash: "n", obs: starved(int64(s))}})
	}
	if never.calls == 0 || reannounces.Load() != 0 {
		t.Errorf("calls %d (want > 0), counter %d (want 0)", never.calls, reannounces.Load())
	}
}

// The backoff survives a rebuild or a short gap without a session: a torrent that leaves the
// samples (or the engine) is kept for its backoff gap, and refires only after it, with a
// fresh window, never 60 s after it came back.
func TestStarveBackoffSurvivesLeavingTheSamples(t *testing.T) {
	pub := &fakeStarveTarget{}
	targets := map[string]*fakeStarveTarget{"h": pub}
	c := newTestController(targets)
	sample := func(s int) []starveSample { return []starveSample{{hash: "h", obs: starved(int64(s))}} }
	// First announce at 60 s; the second gap is 2 min, the third 4 min.
	for s := 0; s <= 200; s += 5 {
		c.tick(at(s), sample(s))
	}
	if pub.calls != 2 {
		t.Fatalf("setup: %d announces by 200 s, want 2 (at 60 and 180)", pub.calls)
	}
	// Rebuild: no session and no torrent for 30 s, then back and starved.
	targets["h"] = nil
	for s := 205; s <= 230; s += 5 {
		c.tick(at(s), nil)
	}
	targets["h"] = pub
	for s := 235; s <= 400; s += 5 {
		c.tick(at(s), sample(s))
	}
	// The third gap is 240 s after the announce at 180: not before 420.
	if pub.calls != 2 {
		t.Errorf("announced again at %d calls before the backoff gap was over", pub.calls)
	}
	for s := 405; s <= 520; s += 5 {
		c.tick(at(s), sample(s))
	}
	if pub.calls != 3 {
		t.Errorf("calls = %d, want the third announce once the gap is over", pub.calls)
	}
	// A state with nothing to protect is dropped: a long absence forgets it.
	c.tick(at(10000), nil)
	if len(c.states) != 0 {
		t.Errorf("states = %d after a long absence", len(c.states))
	}
}

// A panic in an announce does not take the daemon down: the controller logs it and goes on.
func TestStarveControllerRecoversFromAPanic(t *testing.T) {
	bad := &fakeStarveTarget{panics: true}
	good := &fakeStarveTarget{}
	c := newTestController(map[string]*fakeStarveTarget{"a": bad, "b": good})
	for s := 0; s <= 70; s += 5 {
		c.safeTick(at(s), []starveSample{{hash: "a", obs: starved(int64(s))}, {hash: "b", obs: starved(int64(s))}})
	}
	if bad.calls != 1 {
		t.Errorf("panicking target called %d times", bad.calls)
	}
	// The sample order is fixed, so the target after the panicking one is skipped for that tick
	// only and is announced by a later tick or the same one; it must not stay silent.
	for s := 75; s <= 200; s += 5 {
		c.safeTick(at(s), []starveSample{{hash: "b", obs: starved(int64(s))}})
	}
	if good.calls == 0 {
		t.Error("the controller stopped announcing after a panic")
	}
}

// Samples come only from confirmed playback; stalls add up; the ahead is that of the session
// with the least of it, with its own missing-bytes answer; any hung read counts.
func TestStarveSamplesOnlyConfirmedPlayback(t *testing.T) {
	a1, a2, b := int64(100), int64(50), int64(10)
	e1, e2 := int64(0), int64(0)
	views := []sessionView{
		{sessionRecord: sessionRecord{Hash: "a", Stalls: 2, Size: 1 << 30, Reads: 4}, AheadBytes: &a1, LastReadEnd: &e1},
		{sessionRecord: sessionRecord{Hash: "a", Stalls: 3, Size: 1 << 30, Reads: 5}, AheadBytes: &a2, LastReadEnd: &e2, stuckFor: 2 * time.Second},
		{sessionRecord: sessionRecord{Hash: "b", Stalls: 9}, AheadBytes: &b},
		{sessionRecord: sessionRecord{Hash: ""}},
	}
	confirmed := func(h string) bool { return h == "a" }
	peers := func(h string) int { return map[string]int{"a": 1, "b": 7}[h] }
	got := starveSamples(views, confirmed, peers)
	if len(got) != 1 || got[0].hash != "a" {
		t.Fatalf("samples = %+v, want only a", got)
	}
	o := got[0].obs
	if o.peers != 1 || o.stalls != 5 || o.ahead != 50 || !o.aheadOK || !o.aheadMissing || !o.stuck || o.reads != 9 {
		t.Errorf("obs = %+v", o)
	}
	// Ahead reaching the end of the file is nothing missing.
	endAhead, end, size := int64(1<<20), int64(9<<20), int64(10<<20)
	v := []sessionView{{sessionRecord: sessionRecord{Hash: "a", Size: size}, AheadBytes: &endAhead, LastReadEnd: &end}}
	if o := starveSamples(v, confirmed, peers)[0].obs; o.aheadMissing {
		t.Errorf("ahead that reaches the file end reported bytes missing: %+v", o)
	}
}

// A torrent the engine dropped between the nil check and the use (a rebuild nils the engine
// handle) must not panic: the handle is read once and a nil one announces nothing.
func TestStarveTargetReadsTheEngineHandleOnce(t *testing.T) {
	calls := 0
	nilHandle := engineStarveTarget{under: func() *torrent.Torrent { calls++; return nil }}
	if n := nilHandle.Announce(starved(1)); n != 0 { // must not panic
		t.Errorf("dropped torrent announced %d", n)
	}
	if calls != 1 {
		t.Errorf("handle read %d times for one Announce, want once", calls)
	}
}

// Private torrents only ever go through the min-interval gated call, public ones through the
// plain announce, and unknown metadata through neither.
func TestRouteStarveAnnounce(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		name               string
		info               *metainfo.Info
		due                int
		want               int
		wantDue, wantTrack int
	}{
		{"unknown metadata", nil, 1, 0, 0, 0},
		{"private, due", &metainfo.Info{Private: &yes}, 2, 2, 1, 0},
		{"private, deferred", &metainfo.Info{Private: &yes}, 0, 0, 1, 0},
		{"public", &metainfo.Info{}, 5, 1, 0, 1},
		{"public, flag false", &metainfo.Info{Private: &no}, 5, 1, 0, 1},
	} {
		var due, track int
		got := routeStarveAnnounce(c.info, func() int { due++; return c.due }, func() { track++ })
		if got != c.want || due != c.wantDue || track != c.wantTrack {
			t.Errorf("%s: sent %d, due calls %d, tracked calls %d; want %d/%d/%d",
				c.name, got, due, track, c.want, c.wantDue, c.wantTrack)
		}
	}
}

func TestStarveLogLineHasNoTrackerURL(t *testing.T) {
	line := starveLogLine(starveObs{peers: 1, stalls: 4, ahead: 3 << 20, aheadOK: true})
	for _, want := range []string{"reason=starved", "peers=1", "stalls=4"} {
		if !strings.Contains(line, want) {
			t.Errorf("%q lacks %q", line, want)
		}
	}
	if strings.Contains(line, "http") || strings.Contains(line, "passkey") {
		t.Errorf("log line leaks a tracker: %q", line)
	}
}

// A read in flight is tracked per session: begin and end, the oldest start is kept while
// several overlap, and nothing is reported once they are done.
func TestSessionReadInFlight(t *testing.T) {
	s := &TTFFSession{}
	now := time.Now()
	if d := s.stuckFor(now); d != 0 {
		t.Errorf("idle session stuck for %v", d)
	}
	s.readBegin()
	if d := s.stuckFor(time.Now().Add(3 * time.Second)); d < 2*time.Second {
		t.Errorf("a read in flight 3 s reports %v", d)
	}
	s.readBegin() // a second, later read must not reset the clock
	if d := s.stuckFor(time.Now().Add(3 * time.Second)); d < 2*time.Second {
		t.Errorf("an overlapping read reset the clock: %v", d)
	}
	s.readEnd()
	s.readEnd()
	if d := s.stuckFor(time.Now().Add(time.Hour)); d != 0 {
		t.Errorf("finished reads still report %v", d)
	}
}

// Only the sessions of confirmed playback get the (engine-touching) ahead computation.
func TestOpenSessionViewsWhereFiltersBeforeAhead(t *testing.T) {
	resetKeepaliveState(t)
	asked := map[string]bool{}
	old := sessionFilePieces
	sessionFilePieces = func(hash string, _ int, _, _ int64) *native.FilePieceMap { asked[hash] = true; return nil }
	t.Cleanup(func() { sessionFilePieces = old })
	for _, h := range []string{"aa", "bb"} {
		s := &TTFFSession{path: "/m/" + h, hash: h, size: 1 << 30, fileIndex: 1}
		s.lastOff.Store(0)
		s.lastEnd.Store(1 << 20)
		sessions.Store(s.path, s)
	}
	vs := openSessionViewsWhere(time.Now(), func(s *TTFFSession) bool { return s.hash == "aa" })
	if len(vs) != 1 || vs[0].Hash != "aa" {
		t.Fatalf("views = %+v, want only aa", vs)
	}
	if !asked["aa"] || asked["bb"] {
		t.Error("the ahead of an unconfirmed session was computed")
	}
}
