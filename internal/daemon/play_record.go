package daemon

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Per-play record: one [Play] line when a confirmed playback ends, so "did the viewer
// wait, and why" needs no log analysis. It reads what the sessions already hold; it
// changes no read, pump, cache or decision.
//
// A play is the chain of confirmed sessions of one path (a pause or a long gap closes a
// session and the next read opens one linked by resumed_from). Each closed confirmed
// session that was real playback (ttffIsReal) is folded into the path's playAcc as it
// closes. The play ends at the stop webhook (stopPlay, which adds a read-only snapshot
// of the still-open session), at the session close that leaves the path with no open
// handle (playClosed, the idle close that already existed), or at shutdown. No session's
// lifecycle changes. Nothing here runs on the read path.

// playAcc is the sum of a play's sessions so far.
type playAcc struct {
	hash, path string
	sessions   int
	resumed    bool  // a merged session continued an earlier one of the same play
	startMS    int64 // first session's open to first byte; -1 when it never got one
	watchNS    int64
	stalledNS  int64
	byOrigin   [numStallOrigins]int64
	byRole     [numStallRoles]int64
	bytesFetch int64
	bytesRA    int64
	bytesHead  int64
	bytesTail  int64
}

func (a *playAcc) stalls() int64 {
	var n int64
	for _, c := range a.byOrigin {
		n += c
	}
	return n
}

// plays is the table of plays in progress, by path. Its lock is taken at a session
// close, a stop, a last release and shutdown, never on a read.
var plays struct {
	mu sync.Mutex
	m  map[string]*playAcc
}

// playStats are the series of flodni_plays_total and flodni_play_start_seconds.
var playStats struct {
	byStalled [2]atomic.Int64 // index 1: the play had at least one stall
	start     TTFFHistogram
}

// playMerge folds a session's numbers into its path's play. It only reads the
// session's atomics, so it serves a closed session and a snapshot of an open one.
func (s *TTFFSession) playMerge() {
	plays.mu.Lock()
	defer plays.mu.Unlock()
	if plays.m == nil {
		plays.m = make(map[string]*playAcc)
	}
	a := plays.m[s.path]
	if a == nil {
		a = &playAcc{hash: s.hash, path: s.path, startMS: -1}
		if fd, opened := s.firstDataAt.Load(), s.openedAt.Load(); fd > 0 {
			a.startMS = max(time.Duration(fd-opened).Milliseconds(), 0)
		}
		plays.m[s.path] = a
	}
	a.sessions++
	if s.resumedFrom != 0 && !s.afterPlayEnd {
		a.resumed = true
	}
	a.watchNS += s.watchNS.Load()
	a.stalledNS += s.stalledNS.Load()
	for i := range a.byOrigin {
		a.byOrigin[i] += s.stallByOrigin[i].Load()
	}
	for i := range a.byRole {
		a.byRole[i] += s.stallByRole[i].Load()
	}
	a.bytesFetch += s.bytesFetchBlock.Load()
	a.bytesRA += s.bytesRACacheHit.Load()
	a.bytesHead += s.bytesWarmupHead.Load()
	a.bytesTail += s.bytesWarmupTail.Load()
}

// playClosed is closeSession's hook. A confirmed session that was real playback is
// folded into its play, once (a stop webhook may have taken it already). The play ends
// here when the path then has no open handle: the natural end, the idle close after the
// player let go. With a handle open (a pause) the play goes on. It changes nothing about
// the session.
func (s *TTFFSession) playClosed(real bool) {
	if !s.confirmed.Load() {
		return
	}
	if real && s.playReported.CompareAndSwap(false, true) {
		s.playMerge()
	}
	if !globalOpenTracker.IsPathOpen(s.path) {
		endPlay(s.path)
	}
}

// noteStall adds one stall to the session's own tallies (countStall calls it).
func (s *TTFFSession) noteStall(origin, role int, d time.Duration) {
	s.stallByOrigin[origin].Add(1)
	s.stallByRole[role].Add(1)
	s.stalledNS.Add(max(d.Nanoseconds(), 0))
}

// stopPlay ends the play on path at the stop webhook. The folded closed sessions are
// already in the play; the open confirmed session, if it was real playback, is added
// from a read-only snapshot of its counters and marked reported, so its later natural
// close adds nothing and starts no second play. The session itself is not touched.
func stopPlay(path string) {
	if v, ok := sessions.Load(path); ok {
		s := v.(*TTFFSession)
		if s.confirmed.Load() && s.playReported.CompareAndSwap(false, true) {
			s.playStopped.Store(true)
			if ttffIsReal(s.tailOrDeep.Load(), s.bytesRead.Load()) {
				s.playMerge()
			}
		}
	}
	endPlay(path)
}

// endPlay takes the path's play out of the table and logs it. Taking it out under the
// table's lock is what makes the line exactly once, whichever of the stop webhook, the
// natural close and the shutdown flush gets there first. A path with no play in
// progress logs nothing.
func endPlay(path string) {
	plays.mu.Lock()
	a := plays.m[path]
	delete(plays.m, path)
	plays.mu.Unlock()
	if a == nil {
		return
	}
	markPlayEnded(path)
	a.emit()
}

// markPlayEnded flags the path's last closed session as the end of a play, if a read
// has not resumed it yet, so the next session on the path starts a new play.
func markPlayEnded(path string) {
	resumeMu.Lock()
	defer resumeMu.Unlock()
	if v, ok := lastClosed.Load(path); ok {
		ci := v.(closedInfo)
		ci.playEnded = true
		lastClosed.Store(path, ci)
	}
}

// finishAllPlays ends every play in progress at shutdown, after the sessions are closed.
func finishAllPlays() {
	plays.mu.Lock()
	paths := make([]string, 0, len(plays.m))
	for p := range plays.m {
		paths = append(paths, p)
	}
	plays.mu.Unlock()
	for _, p := range paths {
		endPlay(p)
	}
}

// emit counts the play and writes its [Play] line.
func (a *playAcc) emit() {
	stalls := a.stalls()
	playStats.byStalled[confirmedIdx(stalls > 0)].Add(1)
	if a.startMS >= 0 {
		playStats.start.Add(time.Duration(a.startMS) * time.Millisecond)
	}
	short := a.hash
	if len(short) > 8 {
		short = short[:8]
	}
	file := filepath.Base(a.path)
	logger.Info("[Play]",
		"hash", short,
		"file", file,
		"start_ms", a.startMS,
		"watch_s", roundSeconds(a.watchNS),
		"stalls", stalls,
		"stalls_evicted", a.byOrigin[stallOriginEvicted],
		"stalls_pump_behind", a.byOrigin[stallOriginPumpBehind],
		"stalls_no_pump", a.byOrigin[stallOriginNoPump],
		"stalls_swarm", a.byOrigin[stallOriginSwarm],
		"stalls_primary", a.byRole[stallRolePrimary],
		"stalls_second", a.byRole[stallRoleSecond],
		"stalled_s", roundSeconds(a.stalledNS),
		"sessions", a.sessions,
		"resumed", a.resumed,
		"bytes_fetch_block", a.bytesFetch,
		"bytes_racache_hit", a.bytesRA,
		"bytes_warmup_head", a.bytesHead,
		"bytes_warmup_tail", a.bytesTail,
	)
}

// roundSeconds is ns in seconds to one decimal.
func roundSeconds(ns int64) float64 {
	return float64(ns/int64(100*time.Millisecond)) / 10
}
