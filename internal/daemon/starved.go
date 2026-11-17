package daemon

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"flodni/internal/flog"
	"flodni/internal/gostorm/torr"
)

// Re-announce of a starved torrent during a confirmed playback.
//
// After an engine rebuild a playing torrent can come back with one peer while its tracker
// lists many, and only a fresh announce finds the others. A torrent whose playback a webhook
// confirmed, whose connected peers stay under starvedPeerFloor for starvedWindow and whose
// player is feeling it (a read stall in that window, or ahead_bytes shrinking) is announced
// to its trackers once, then not again before a gap that doubles with every announce that did
// not cure it.
//
// Hard rule 3: a private torrent is announced only to the trackers of its own stored record
// (Torrent.AnnounceWhenDue: never DHT, PEX or a tracker we add), and only once a tracker's own
// announce interval is over, on top of the gap here. A torrent whose privacy is not known yet
// counts as private.
const (
	starvedPeerFloor = 3
	starvedWindow    = 60 * time.Second
	// The least time between two forced announces of one torrent, doubled for each announce
	// that did not end the starvation, up to starvedMaxGap. A private torrent waits longer.
	starvedPublicGap  = 2 * time.Minute
	starvedPrivateGap = 5 * time.Minute
	starvedMaxGap     = 30 * time.Minute
	// starvedRecovery is how long a torrent must stay at or over the floor to start from
	// the shortest gap again.
	starvedRecovery = 2 * time.Minute
	starvedTick     = 5 * time.Second
)

// reannounces counts forced announces, index 1 for a private torrent:
// flodni_reannounce_total{reason="starved",private=...}.
var reannounces [2]atomic.Int64

// starveObs is what is known of one playing torrent at one sample.
type starveObs struct {
	peers   int
	stalls  int64 // read stalls of its open sessions so far
	ahead   int64 // the least ahead_bytes of its sessions; meaningful when aheadOK
	aheadOK bool
}

// starveState follows one torrent through the window and the backoff.
type starveState struct {
	since    time.Time // start of the current window; zero while at or over the floor
	stalls0  int64
	ahead0   int64
	aheadOK0 bool
	lastFire time.Time
	fires    int       // forced announces since the last recovery
	healthy  time.Time // when the peers came back to the floor; zero while starved
}

func (s *starveState) rebase(now time.Time, o starveObs) {
	s.since, s.stalls0, s.ahead0, s.aheadOK0 = now, o.stalls, o.ahead, o.aheadOK
}

// gap is the least time since the last forced announce before another.
func (s *starveState) gap(private bool) time.Duration {
	g := starvedPublicGap
	if private {
		g = starvedPrivateGap
	}
	for i := 1; i < s.fires && g < starvedMaxGap; i++ {
		g *= 2
	}
	return min(g, starvedMaxGap)
}

// step takes one sample and says whether to announce now. It changes nothing a caller
// that finds the announce deferred cannot put back by restoring a copy.
func (s *starveState) step(now time.Time, o starveObs, private bool) bool {
	if o.peers >= starvedPeerFloor {
		s.since = time.Time{}
		if s.healthy.IsZero() {
			s.healthy = now
		} else if now.Sub(s.healthy) >= starvedRecovery {
			s.fires = 0
		}
		return false
	}
	s.healthy = time.Time{}
	if s.since.IsZero() || o.stalls < s.stalls0 { // a new window, or a session that restarted its count
		s.rebase(now, o)
		return false
	}
	if now.Sub(s.since) < starvedWindow {
		return false
	}
	evidence := o.stalls > s.stalls0 || (o.aheadOK && s.aheadOK0 && o.ahead < s.ahead0)
	if !evidence {
		s.rebase(now, o)
		return false
	}
	if !s.lastFire.IsZero() && now.Sub(s.lastFire) < s.gap(private) {
		return false
	}
	s.lastFire = now
	s.fires++
	s.rebase(now, o)
	return true
}

// starveTarget is the torrent a forced announce goes to.
type starveTarget interface {
	Private() bool
	// Announce starts the announce and returns how many trackers it goes to: 0 when none is
	// due (nothing was sent), -1 when the engine does not say.
	Announce(o starveObs) int
}

type starveSample struct {
	hash string
	obs  starveObs
}

// starveController keeps a state per playing torrent. target returns nil for a hash the
// engine does not hold.
type starveController struct {
	states map[string]*starveState
	target func(hash string) starveTarget
}

func newStarveController(target func(hash string) starveTarget) *starveController {
	return &starveController{states: map[string]*starveState{}, target: target}
}

func (c *starveController) tick(now time.Time, samples []starveSample) {
	seen := make(map[string]bool, len(samples))
	for _, sm := range samples {
		tg := c.target(sm.hash)
		if tg == nil {
			continue
		}
		seen[sm.hash] = true
		st := c.states[sm.hash]
		if st == nil {
			st = &starveState{}
			c.states[sm.hash] = st
		}
		private := tg.Private()
		prev := *st
		if !st.step(now, sm.obs, private) {
			continue
		}
		n := tg.Announce(sm.obs)
		if n == 0 {
			// Every tracker is still inside its interval: nothing went out, so nothing is
			// used up; ask again at the next sample.
			*st = prev
			continue
		}
		reannounces[boolIdx(private)].Add(1)
	}
	for h := range c.states {
		if !seen[h] {
			delete(c.states, h) // playback ended or the torrent closed
		}
	}
}

func boolIdx(b bool) int {
	if b {
		return 1
	}
	return 0
}

// starveSamples reduces the open sessions to one sample per torrent with a confirmed
// playback: stalls add up, the ahead is the least of the sessions that have one.
func starveSamples(views []sessionView, confirmed func(string) bool, peers func(string) int) []starveSample {
	var out []starveSample
	idx := map[string]int{}
	for _, v := range views {
		if v.Hash == "" || !confirmed(v.Hash) {
			continue
		}
		i, ok := idx[v.Hash]
		if !ok {
			i = len(out)
			idx[v.Hash] = i
			out = append(out, starveSample{hash: v.Hash, obs: starveObs{peers: peers(v.Hash)}})
		}
		o := &out[i].obs
		o.stalls += v.Stalls
		if v.AheadBytes != nil && (!o.aheadOK || *v.AheadBytes < o.ahead) {
			o.ahead, o.aheadOK = *v.AheadBytes, true
		}
	}
	return out
}

// announcer is the part of the engine torrent that announces.
type announcer interface {
	AnnounceTracked(onDone func(peers, errs, total int, failures []string))
	AnnounceWhenDue(onDone func(peers, errs, total int, failures []string)) int
}

// forceAnnounce announces to the torrent's trackers. A private torrent goes through
// AnnounceWhenDue only, which honours each tracker's interval; the call that does not is
// for a public torrent.
func forceAnnounce(a announcer, private bool, onDone func(peers, errs, total int, failures []string)) int {
	if private {
		return a.AnnounceWhenDue(onDone)
	}
	a.AnnounceTracked(onDone)
	return -1
}

// starvePrivate is BEP 27 as the engine reads it, except that unknown metadata counts as
// private.
func starvePrivate(haveInfo bool, flag *bool) bool {
	if !haveInfo {
		return true
	}
	return flag != nil && *flag
}

func starveLogLine(private bool, o starveObs, trackers int) string {
	line := fmt.Sprintf("[Starved] re-announce reason=starved private=%t peers=%d stalls=%d", private, o.peers, o.stalls)
	if o.aheadOK {
		line += fmt.Sprintf(" ahead_mb=%d", o.ahead>>20)
	}
	if trackers >= 0 {
		line += fmt.Sprintf(" trackers=%d", trackers)
	}
	return line
}

// engineStarveTarget adapts a live engine torrent.
type engineStarveTarget struct{ t *torr.Torrent }

func (e engineStarveTarget) Private() bool {
	info := e.t.Torrent.Info()
	if info == nil {
		return starvePrivate(false, nil)
	}
	return starvePrivate(true, info.Private)
}

func (e engineStarveTarget) Announce(o starveObs) int {
	private := e.Private()
	n := forceAnnounce(e.t.Torrent, private, nil)
	if n != 0 {
		flog.Std.Info(starveLogLine(private, o, n), "reason", "starved", "private", private, "peers", o.peers)
	}
	return n
}

func engineStarveTargetFor(hash string) starveTarget {
	t := torr.PeekTorrent(hash)
	if t == nil || t.Torrent == nil {
		return nil
	}
	return engineStarveTarget{t}
}

// runStarvedReannounce samples the confirmed playbacks every starvedTick.
func runStarvedReannounce(ctx context.Context) {
	c := newStarveController(engineStarveTargetFor)
	tk := time.NewTicker(starvedTick)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tk.C:
			if set, _, _ := confirmedSnapshot(); len(set) == 0 {
				c.tick(now, nil)
				continue
			}
			c.tick(now, starveSamples(openSessionViews(now), hashConfirmed, sessionActivePeers))
		}
	}
}
