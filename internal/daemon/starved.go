package daemon

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"flodni/internal/flog"
	"flodni/internal/gostorm/torr"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
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
// A public torrent is announced to through the normal announce. A private torrent only ever
// goes through the engine's AnnounceWhenDue, which sends nothing before a tracker's min
// interval has passed (and nothing at all when the tracker sent none). A torrent whose
// metadata is not known yet is never announced. An announce that sends nothing is a deferral:
// it is not counted and consumes no backoff, and the torrent is asked again on the next tick.
const (
	starvedPeerFloor = 3
	starvedWindow    = 60 * time.Second
	// The least time between two forced announces of one torrent, doubled for each announce
	// that did not end the starvation, up to starvedMaxGap.
	starvedGap    = 2 * time.Minute
	starvedMaxGap = 30 * time.Minute
	// starvedLowWater: a shrinking ahead counts only under this many buffered bytes.
	starvedLowWater = 32 << 20
	// starvedRecovery is how long a torrent must stay at or over the floor to start from
	// the shortest gap again.
	starvedRecovery = 2 * time.Minute
	starvedTick     = 5 * time.Second
)

// reannounces counts forced announces: flodni_reannounce_total{reason="starved"}.
var reannounces atomic.Int64

// starveObs is what is known of one playing torrent at one sample.
type starveObs struct {
	peers   int
	stalls  int64 // read stalls of its open sessions so far
	ahead   int64 // the least ahead_bytes of its sessions; meaningful when aheadOK
	aheadOK bool
	// aheadMissing: bytes are missing past that ahead, i.e. it stops short of the file's end.
	// Ahead is clamped to the end, so near it a fully buffered file's ahead shrinks without
	// anything being wrong.
	aheadMissing bool
	stuck        bool  // a read has been in flight longer than the stall threshold
	reads        int64 // served reads of its sessions so far
}

// starveState follows one torrent through the window and the backoff.
type starveState struct {
	since    time.Time // start of the current window; zero while at or over the floor
	stalls0  int64
	reads0   int64
	ahead0   int64
	aheadOK0 bool
	lastFire time.Time
	fires    int       // forced announces since the last recovery
	healthy  time.Time // when the peers came back to the floor; zero while starved
}

func (s *starveState) rebase(now time.Time, o starveObs) {
	s.since, s.stalls0, s.reads0, s.ahead0, s.aheadOK0 = now, o.stalls, o.reads, o.ahead, o.aheadOK
}

// gap is the least time since the last forced announce before another.
func (s *starveState) gap() time.Duration {
	g := starvedGap
	for i := 1; i < s.fires && g < starvedMaxGap; i++ {
		g *= 2
	}
	return min(g, starvedMaxGap)
}

// step takes one sample and says whether to announce now. It changes nothing a caller
// that finds the announce deferred cannot put back by restoring a copy.
func (s *starveState) step(now time.Time, o starveObs) bool {
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
	// Evidence the player feels it: a read that completed slowly, one still hanging, or a
	// buffer that is short of the end of the file, under the low-water mark, and either
	// shrinking or empty with no read progress. A stall is counted only when a read ends, so
	// the hung read and the empty buffer cover the worst case.
	lowMissing := o.aheadOK && o.aheadMissing && o.ahead < starvedLowWater
	evidence := o.stalls > s.stalls0 || o.stuck ||
		(lowMissing && s.aheadOK0 && o.ahead < s.ahead0) ||
		(lowMissing && o.ahead == 0 && o.reads == s.reads0)
	if !evidence {
		s.rebase(now, o)
		return false
	}
	if !s.lastFire.IsZero() && now.Sub(s.lastFire) < s.gap() {
		return false
	}
	s.lastFire = now
	s.fires++
	s.rebase(now, o)
	return true
}

// starveTarget is the torrent a forced announce goes to.
type starveTarget interface {
	// Announce returns how many announces were actually sent; 0 means deferred or none.
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

// safeTick is tick with a panic turned into a log line, so one bad torrent cannot stop the
// daemon.
func (c *starveController) safeTick(now time.Time, samples []starveSample) {
	defer func() {
		if r := recover(); r != nil {
			flog.Std.Warn(fmt.Sprintf("[Starved] sampling pass recovered from a panic: %v", r), "panic", fmt.Sprint(r))
		}
	}()
	c.tick(now, samples)
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
		prev := *st // plain values: a copy restores it exactly
		if !st.step(now, sm.obs) {
			continue
		}
		if tg.Announce(sm.obs) == 0 {
			*st = prev // deferred: no backoff consumed, nothing counted
			continue
		}
		reannounces.Add(1)
	}
	for h, st := range c.states {
		if seen[h] {
			continue
		}
		// Gone from the samples: a rebuild, an idle close or a moment without a session. The
		// backoff is kept for as long as it still limits the next announce, so the torrent
		// cannot fire again 60 s after it comes back; its window starts over.
		if !st.lastFire.IsZero() && now.Sub(st.lastFire) < st.gap() {
			st.since, st.healthy = time.Time{}, time.Time{}
			continue
		}
		delete(c.states, h)
	}
}

// starveSamples reduces the open sessions to one sample per torrent with a confirmed
// playback: stalls and reads add up, a hung read in any session counts, and the ahead is that
// of the session with the least of it.
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
		o.reads += v.Reads
		if v.stuckFor > ttffStallDuration {
			o.stuck = true
		}
		if v.AheadBytes != nil && (!o.aheadOK || *v.AheadBytes < o.ahead) {
			o.ahead, o.aheadOK = *v.AheadBytes, true
			o.aheadMissing = v.LastReadEnd != nil && *v.LastReadEnd+*v.AheadBytes < v.Size
		}
	}
	return out
}

func starveLogLine(o starveObs) string {
	line := fmt.Sprintf("[Starved] re-announce reason=starved peers=%d stalls=%d", o.peers, o.stalls)
	if o.aheadOK {
		line += fmt.Sprintf(" ahead_mb=%d", o.ahead>>20)
	}
	return line
}

// engineStarveTarget adapts a live engine torrent. under returns the engine handle, which a
// rebuild nils (drop) at any time: it is read once per call and the copy is used.
type engineStarveTarget struct{ under func() *torrent.Torrent }

func (e engineStarveTarget) Announce(o starveObs) int {
	t := e.under()
	if t == nil {
		return 0
	}
	n := routeStarveAnnounce(t.Info(), t.AnnounceWhenDue, func() { t.AnnounceTracked(nil) })
	if n > 0 {
		flog.Std.Info(starveLogLine(o), "reason", "starved", "peers", o.peers)
	}
	return n
}

// routeStarveAnnounce picks the announce for a torrent and returns how many were sent. Unknown
// metadata sends nothing; a private torrent only goes through due (the min-interval gated
// announce); a public one through tracked.
func routeStarveAnnounce(info *metainfo.Info, due func() int, tracked func()) int {
	if info == nil {
		return 0
	}
	if info.Private != nil && *info.Private {
		return due()
	}
	tracked()
	return 1
}

func engineStarveTargetFor(hash string) starveTarget {
	t := torr.PeekTorrent(hash)
	if t == nil || t.UnderlyingTorrent() == nil {
		return nil
	}
	return engineStarveTarget{under: t.UnderlyingTorrent}
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
			// Only confirmed playback is looked at, and its sessions are chosen before the
			// ahead (an engine walk) is computed.
			var samples []starveSample
			if set, _, _ := confirmedSnapshot(); len(set) > 0 {
				views := openSessionViewsWhere(now, func(s *TTFFSession) bool { return hashConfirmed(s.hash) })
				samples = starveSamples(views, hashConfirmed, sessionActivePeers)
			}
			c.safeTick(now, samples)
		}
	}
}
