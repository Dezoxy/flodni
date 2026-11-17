package daemon

import (
	"strings"
	"testing"
	"time"
)

var starveT0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return starveT0.Add(time.Duration(sec) * time.Second) }

// starved is an observation under the peer floor with the read stalls so far.
func starved(stalls int64) starveObs { return starveObs{peers: 1, stalls: stalls} }

// stepAll feeds one observation per 5 s tick from start to end (seconds) and returns the
// seconds at which step said to announce.
func stepAll(st *starveState, private bool, from, to int, obs func(sec int) starveObs) []int {
	var fires []int
	for s := from; s <= to; s += 5 {
		if st.step(at(s), obs(s), private) {
			fires = append(fires, s)
		}
	}
	return fires
}

// A starved torrent is announced once, after the window, and not before it.
func TestStarveFiresAfterTheWindowNotBefore(t *testing.T) {
	st := &starveState{}
	// Stalls grow from the first sample: the window runs from it.
	fires := stepAll(st, true, 0, 120, func(s int) starveObs { return starved(int64(s / 5)) })
	if len(fires) != 1 || fires[0] != int(starvedWindow/time.Second) {
		t.Fatalf("fires at %v s, want exactly one at %v", fires, int(starvedWindow/time.Second))
	}
}

// Low peers alone are not enough: it needs a stall in the window or a shrinking ahead.
func TestStarveNeedsEvidenceOfAStarvingPlayer(t *testing.T) {
	flat := func(int) starveObs { return starveObs{peers: 1, stalls: 4, ahead: 8 << 20, aheadOK: true} }
	if f := stepAll(&starveState{}, false, 0, 600, flat); len(f) != 0 {
		t.Errorf("no stall and a steady ahead announced at %v", f)
	}
	shrinking := func(s int) starveObs {
		return starveObs{peers: 1, stalls: 4, ahead: int64(8<<20 - s*1000), aheadOK: true}
	}
	if f := stepAll(&starveState{}, false, 0, 90, shrinking); len(f) != 1 {
		t.Errorf("a shrinking ahead announced at %v, want once", f)
	}
	unknownAhead := func(int) starveObs { return starveObs{peers: 1, stalls: 4} }
	if f := stepAll(&starveState{}, false, 0, 600, unknownAhead); len(f) != 0 {
		t.Errorf("an unknown ahead and no stall announced at %v", f)
	}
}

// A swarm at or over the floor never announces, whatever the stalls say, and a recovery
// restarts the window.
func TestStarveNotForAHealthySwarm(t *testing.T) {
	healthy := func(s int) starveObs { return starveObs{peers: starvedPeerFloor, stalls: int64(s)} }
	if f := stepAll(&starveState{}, true, 0, 600, healthy); len(f) != 0 {
		t.Errorf("a swarm at the peer floor announced at %v", f)
	}
	st := &starveState{}
	// 50 s starved, one sample healthy, then starved again: the 60 s window starts over.
	stepAll(st, false, 0, 50, func(s int) starveObs { return starved(int64(s)) })
	st.step(at(55), starveObs{peers: 5, stalls: 55}, false)
	if f := stepAll(st, false, 60, 110, func(s int) starveObs { return starved(int64(s)) }); len(f) != 0 {
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

// Backoff on repeated starvation: a private torrent waits at least 5 minutes, then 10, 20,
// and never more than 30 between forced announces; a public one starts at 2.
func TestStarveBackoff(t *testing.T) {
	always := func(s int) starveObs { return starved(int64(s)) }
	private := stepAll(&starveState{}, true, 0, 4*3600, always)
	if len(private) < 6 {
		t.Fatalf("private fires = %v", private)
	}
	for i, g := range gaps(private)[:5] {
		want := []int{300, 600, 1200, 1800, 1800}[i]
		if g < want {
			t.Errorf("private gap %d = %d s, under the %d s minimum", i, g, want)
		}
	}
	for _, g := range gaps(private) {
		if g < 300 || g > 1800+int(starvedWindow/time.Second)+5 {
			t.Errorf("private gap %d s is outside 300..%d", g, 1800+int(starvedWindow/time.Second)+5)
		}
	}
	public := stepAll(&starveState{}, false, 0, 3600, always)
	if g := gaps(public); len(g) < 2 || g[0] < 120 || g[0] >= 300 || g[1] < 240 {
		t.Errorf("public gaps = %v, want the first 120..299 s and the second at least 240 s", g)
	}
	// Nothing in the first 5 minutes after a private fire, however starved.
	st := &starveState{}
	f := stepAll(st, true, 0, 290, always)
	if len(f) != 1 {
		t.Errorf("private fires within 290 s = %v, want one", f)
	}
}

// A torrent that recovers for a while starts again from the shortest gap.
func TestStarveBackoffResetsAfterRecovery(t *testing.T) {
	st := &starveState{}
	always := func(s int) starveObs { return starved(int64(s)) }
	stepAll(st, true, 0, 700, always) // fires at 60 and 360+
	if st.fires < 2 {
		t.Fatalf("setup: fires = %d", st.fires)
	}
	for s := 700; s <= 700+int(starvedRecovery/time.Second)+10; s += 5 {
		st.step(at(s), starveObs{peers: 8}, true)
	}
	if st.fires != 0 {
		t.Errorf("fires = %d after a recovery, want 0", st.fires)
	}
}

type fakeStarveTarget struct {
	private bool
	started int // what Announce reports
	calls   int
}

func (f *fakeStarveTarget) Private() bool          { return f.private }
func (f *fakeStarveTarget) Announce(starveObs) int { f.calls++; return f.started }

func newTestController(targets map[string]*fakeStarveTarget) *starveController {
	return newStarveController(func(hash string) starveTarget {
		if t, ok := targets[hash]; ok {
			return t
		}
		return nil
	})
}

func resetReannounceCount() {
	for i := range reannounces {
		reannounces[i].Store(0)
	}
}

// One announce for a starved confirmed torrent after the window, none before, none for a
// torrent that is not starved or not sampled (not playing), and counted by privacy.
func TestStarveControllerAnnouncesOnceAndOnlyTheStarved(t *testing.T) {
	resetReannounceCount()
	priv := &fakeStarveTarget{private: true, started: 2}
	pub := &fakeStarveTarget{started: 1}
	fine := &fakeStarveTarget{private: true, started: 1}
	idle := &fakeStarveTarget{private: true, started: 1} // in the engine but never sampled
	c := newTestController(map[string]*fakeStarveTarget{"priv": priv, "pub": pub, "fine": fine, "idle": idle})
	for s := 0; s <= 120; s += 5 {
		c.tick(at(s), []starveSample{
			{hash: "priv", obs: starved(int64(s))},
			{hash: "pub", obs: starved(int64(s))},
			{hash: "fine", obs: starveObs{peers: 10, stalls: int64(s)}},
		})
		if s < int(starvedWindow/time.Second) && priv.calls+pub.calls != 0 {
			t.Fatalf("announced at %d s, before the window", s)
		}
	}
	if priv.calls != 1 || pub.calls != 1 {
		t.Errorf("announces: private %d, public %d, want 1 each", priv.calls, pub.calls)
	}
	if fine.calls != 0 || idle.calls != 0 {
		t.Errorf("announces: healthy %d, not playing %d, want 0", fine.calls, idle.calls)
	}
	if reannounces[1].Load() != 1 || reannounces[0].Load() != 1 {
		t.Errorf("counter private/public = %d/%d, want 1/1", reannounces[1].Load(), reannounces[0].Load())
	}
}

// A private torrent whose trackers are all still inside their interval starts no announce:
// that is not counted, does not use up the backoff, and is tried again at the next sample.
func TestStarveControllerDeferredAnnounceIsNotAnAnnounce(t *testing.T) {
	resetReannounceCount()
	tg := &fakeStarveTarget{private: true, started: 0}
	c := newTestController(map[string]*fakeStarveTarget{"h": tg})
	for s := 0; s <= 120; s += 5 {
		c.tick(at(s), []starveSample{{hash: "h", obs: starved(int64(s))}})
	}
	if tg.calls < 2 {
		t.Fatalf("calls = %d, want a retry at every sample once starved", tg.calls)
	}
	if reannounces[1].Load() != 0 {
		t.Errorf("counted %d announces that never went out", reannounces[1].Load())
	}
	tg.started = 1
	c.tick(at(125), []starveSample{{hash: "h", obs: starved(25)}})
	if reannounces[1].Load() != 1 {
		t.Errorf("announce after the interval was not counted")
	}
	// The gap now runs from that announce.
	before := tg.calls
	for s := 130; s <= 130+250; s += 5 {
		c.tick(at(s), []starveSample{{hash: "h", obs: starved(int64(s))}})
	}
	if tg.calls != before {
		t.Errorf("announced again %d times within 5 minutes", tg.calls-before)
	}
}

// A hash that left the samples (playback stopped, torrent closed) is forgotten.
func TestStarveControllerForgetsGoneTorrents(t *testing.T) {
	c := newTestController(map[string]*fakeStarveTarget{"h": {started: 1}})
	c.tick(at(0), []starveSample{{hash: "h", obs: starved(0)}})
	if len(c.states) != 1 {
		t.Fatal("no state kept")
	}
	c.tick(at(5), nil)
	if len(c.states) != 0 {
		t.Errorf("states = %d after the playback ended", len(c.states))
	}
}

// Samples come only from open sessions with a confirmed playback; the ahead is the least of
// a torrent's sessions and stalls add up.
func TestStarveSamplesOnlyConfirmedPlayback(t *testing.T) {
	a1, a2, b := int64(100), int64(50), int64(10)
	views := []sessionView{
		{sessionRecord: sessionRecord{Hash: "a", Stalls: 2}, AheadBytes: &a1},
		{sessionRecord: sessionRecord{Hash: "a", Stalls: 3}, AheadBytes: &a2},
		{sessionRecord: sessionRecord{Hash: "b", Stalls: 9}, AheadBytes: &b},
		{sessionRecord: sessionRecord{Hash: ""}},
	}
	confirmed := func(h string) bool { return h == "a" }
	peers := func(h string) int { return map[string]int{"a": 1, "b": 7}[h] }
	got := starveSamples(views, confirmed, peers)
	if len(got) != 1 || got[0].hash != "a" {
		t.Fatalf("samples = %+v, want only a", got)
	}
	if o := got[0].obs; o.peers != 1 || o.stalls != 5 || o.ahead != 50 || !o.aheadOK {
		t.Errorf("obs = %+v, want peers 1, stalls 5, ahead 50", o)
	}
}

type fakeAnnouncer struct {
	tracked, due int
	dueStarts    int
}

func (f *fakeAnnouncer) AnnounceTracked(func(int, int, int, []string)) { f.tracked++ }
func (f *fakeAnnouncer) AnnounceWhenDue(func(int, int, int, []string)) int {
	f.due++
	return f.dueStarts
}

// Hard rule 3: a torrent that is private, or whose privacy is not known yet, is announced
// only through the interval-honouring call that reaches its stored trackers; never through
// the one that ignores the interval.
func TestForceAnnouncePrivateNeverUsesTheIntervalBlindCall(t *testing.T) {
	for _, tc := range []struct {
		name    string
		private bool
	}{{"private", true}} {
		a := &fakeAnnouncer{dueStarts: 2}
		if n := forceAnnounce(a, tc.private, nil); n != 2 {
			t.Errorf("%s: started %d, want 2", tc.name, n)
		}
		if a.tracked != 0 || a.due != 1 {
			t.Errorf("%s: AnnounceTracked %d, AnnounceWhenDue %d, want 0 and 1", tc.name, a.tracked, a.due)
		}
	}
	pub := &fakeAnnouncer{}
	if n := forceAnnounce(pub, false, nil); n == 0 || pub.tracked != 1 || pub.due != 0 {
		t.Errorf("public: started %d, AnnounceTracked %d, AnnounceWhenDue %d", n, pub.tracked, pub.due)
	}
}

// Torrent privacy: unknown metadata counts as private.
func TestStarvePrivacyOfUnknownMetadataIsPrivate(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name string
		info *bool
		have bool
		want bool
	}{{"no metadata", nil, false, true}, {"flagged", &yes, true, true}, {"not flagged", &no, true, false}, {"no flag", nil, true, false}} {
		if got := starvePrivate(tc.have, tc.info); got != tc.want {
			t.Errorf("%s: private = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The log line names the reason and the peer counts and never a tracker.
func TestStarveLogLineHasNoTrackerURL(t *testing.T) {
	line := starveLogLine(true, starveObs{peers: 1, stalls: 4, ahead: 3 << 20, aheadOK: true}, 2)
	for _, want := range []string{"reason=starved", "private=true", "peers=1", "trackers=2"} {
		if !strings.Contains(line, want) {
			t.Errorf("%q lacks %q", line, want)
		}
	}
	if strings.Contains(line, "http") || strings.Contains(line, "passkey") {
		t.Errorf("log line leaks a tracker: %q", line)
	}
}
