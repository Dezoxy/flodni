package daemon

import (
	"sync"
	"sync/atomic"
	"time"
)

// Keep-alive counters (Reliability step 8, phase a): how often the engine closes an
// idle torrent, how often a read wakes one again and how often a served read stalls,
// each split by whether a file of the torrent had a webhook-confirmed playback. They
// change no behaviour. Index 1 is confirmed, 0 is not.
var keepaliveCounters struct {
	idleCloses [2]atomic.Int64
	reopens    [2]atomic.Int64
	// stalls is [confirmed][cause][origin][role]: cause is stallCausePlayback or stallCauseSeek,
	// origin one of the stallOrigin constants, role one of the stallRole constants. Summed over
	// role it is the count the combination had before the role label, summed over origin the
	// count the pair had before the origin label.
	stalls [2][2][numStallOrigins][numStallRoles]atomic.Int64
	// Keep-alive hold (phase b), in keepalive.go: releases by reason (0 stop, 1
	// ceiling) and torrents that lost to the cap.
	releases [2]atomic.Int64
	evicted  atomic.Int64
}

// Causes of a read stall. A stall is seek when it began after a jump of the session's
// read position and before the first cache-served read after it, within
// seekStallWindow of the jump (see recordRead); every other stall is playback.
const (
	stallCausePlayback = 0
	stallCauseSeek     = 1
)

var stallCauseNames = [2]string{stallCausePlayback: "playback", stallCauseSeek: "seek"}

// Origins of a read stall: what the engine was doing when a served read took over a second.
// Checked in this order, so a stall has exactly one.
const (
	// stallOriginEvicted: a piece of the read's span completed again while the read ran, so it
	// had been evicted from the in-memory cache and the swarm delivered it twice.
	stallOriginEvicted = iota
	// stallOriginPumpBehind: a pump exists for the session and its fill offset is at or below
	// the read offset, so the read overtook what the pump had fetched.
	stallOriginPumpBehind
	// stallOriginNoPump: no pump exists for the session.
	stallOriginNoPump
	// stallOriginSwarm: everything else, a pump ahead of the read and a swarm that was slow.
	stallOriginSwarm
	numStallOrigins
)

var stallOriginNames = [numStallOrigins]string{
	stallOriginEvicted:    "evicted",
	stallOriginPumpBehind: "pump_behind",
	stallOriginNoPump:     "no_pump",
	stallOriginSwarm:      "swarm",
}

// watchCreditCap is the most one gap between two served reads of a session adds to
// confirmedPlaybackNS. Players read in bursts, so counting only the seconds that hold a
// read would undercount by an amount that depends on the client; a capped gap does not.
const watchCreditCap = 30 * time.Second

// confirmedPlaybackNS is the watch time of confirmed torrents in nanoseconds: the
// denominator for flodni_read_stalls_total{confirmed="true"}. It changes no behaviour.
var confirmedPlaybackNS atomic.Int64

// watchCreditStep is the least time between two moves of a session's previous-read
// time. A read less than a step after the last move changes nothing and its time stays
// in the next gap, so the confirmed lookup below runs at most about once per session per
// step instead of on every read.
const watchCreditStep = time.Second

// creditWatchTime is called with the time of each served read. The first read stores its
// time and credits nothing. A later read at least watchCreditStep after the stored time
// moves it forward to nowN and, when the torrent is confirmed by the same test that
// labels a stall, credits the gap, at most watchCreditCap. An unconfirmed read moves the
// time too, so a later confirmation does not credit the time before it.
//
// hashConfirmed takes confirmedHashes.mu, and rebuilds the snapshot under it once a
// second by walking the playback registry. Calling it on every read would put that lock
// on the read path, so it is reached once per step only; the reads in between cost one
// atomic load. The time only moves forward (a compare-and-swap loop), so reads racing on
// the session credit each stretch once, and nothing is allocated here.
func (s *TTFFSession) creditWatchTime(nowN int64) {
	for {
		prev := s.watchPrevAt.Load()
		if prev != 0 && nowN-prev < int64(watchCreditStep) {
			return
		}
		if !s.watchPrevAt.CompareAndSwap(prev, nowN) {
			continue
		}
		if prev == 0 {
			return
		}
		if gap := min(time.Duration(nowN-prev), watchCreditCap); hashConfirmed(s.hash) {
			confirmedPlaybackNS.Add(int64(gap))
		}
		return
	}
}

// Roles of the handle that served a stalled read. The role is read when the read finishes.
const (
	// stallRolePrimary: the handle was its file's primary (the one the pump follows).
	stallRolePrimary = iota
	// stallRoleSecond: anything else, a handle on a file with no primary and a probe read included.
	stallRoleSecond
	numStallRoles
)

var stallRoleNames = [numStallRoles]string{
	stallRolePrimary: "primary",
	stallRoleSecond:  "second",
}

// stallRoleOf is the role of the handle h serving a read; probe is the read's probe flag.
// One atomic load: it runs on every read, stalled or not.
func stallRoleOf(h *MkvHandle, probe bool) int {
	if !probe && h.isPrimaryHandle.Load() {
		return stallRolePrimary
	}
	return stallRoleSecond
}

func confirmedIdx(confirmed bool) int {
	if confirmed {
		return 1
	}
	return 0
}

// confirmedHashes is the per-torrent snapshot of confirmed playback, rebuilt at most
// once a second like playbackOpenCache: scanners register thousands of paths and the
// idle ticker asks every second, so a walk per call would be quadratic. A torrent is in
// it when a file of it is confirmed and not stopped; its value is the last activity of
// those files, the later of the last read and the last play/resume webhook (a pause
// does not touch either). A webhook that confirms or stops a file zeroes `at`
// (invalidateConfirmed), so those take effect at once; a read shows within a second.
// The map is replaced, never changed in place, so a caller may use it after unlocking.
var (
	confirmedHashesNow = time.Now
	confirmedHashes    struct {
		mu  sync.Mutex
		at  time.Time
		gen uint64
		set map[string]time.Time
	}
)

// confirmedSnapshot returns the current snapshot and its generation, rebuilding it when
// it is over a second old. It takes no engine lock.
func confirmedSnapshot() (map[string]time.Time, uint64, time.Time) {
	confirmedHashes.mu.Lock()
	defer confirmedHashes.mu.Unlock()
	now := confirmedHashesNow()
	if confirmedHashes.set == nil || now.Sub(confirmedHashes.at) >= time.Second {
		set := make(map[string]time.Time)
		app.playbackRegistry.Range(func(_, v any) bool {
			ps, ok := v.(*PlaybackState)
			if !ok || ps.Hash == "" {
				return true
			}
			ps.mu.RLock()
			live := ps.IsHealthy && !ps.IsStopped
			last := ps.ConfirmedAt
			if ps.LastReadAt.After(last) {
				last = ps.LastReadAt
			}
			ps.mu.RUnlock()
			if cur, seen := set[ps.Hash]; live && (!seen || last.After(cur)) {
				set[ps.Hash] = last
			}
			return true
		})
		confirmedHashes.set, confirmedHashes.at = set, now
		confirmedHashes.gen++
	}
	return confirmedHashes.set, confirmedHashes.gen, now
}

// invalidateConfirmed makes the next lookup rebuild the snapshot. The webhook handler
// calls it after a confirm or a stop.
func invalidateConfirmed() {
	confirmedHashes.mu.Lock()
	confirmedHashes.at = time.Time{}
	confirmedHashes.mu.Unlock()
}

// hashConfirmed reports whether any file of the torrent has a webhook-confirmed
// playback. It takes no engine lock, so the idle-close hook may call it.
func hashConfirmed(hash string) bool {
	if hash == "" {
		return false
	}
	set, _, _ := confirmedSnapshot()
	_, ok := set[hash]
	return ok
}

// countIdleClose is the torr.OnIdleClose hook.
func countIdleClose(hash string) {
	keepaliveCounters.idleCloses[confirmedIdx(hashConfirmed(hash))].Add(1)
}

// countReopen counts a read-path wake of a closed torrent. The wake at Open is not one.
func countReopen(path, hash string) {
	keepaliveCounters.reopens[confirmedIdx(hashConfirmed(hash))].Add(1)
	if v, ok := sessions.Load(path); ok {
		v.(*TTFFSession).reopens.Add(1)
	}
}

// countStall counts a served read over the stall threshold, labelled by the torrent
// of the path: the session's hash, else the registry state's, by cause, origin and role. off and
// n are the read's offset and returned bytes, d how long it took.
func countStall(path string, cause, role int, off int64, n int, d time.Duration) {
	hash, fileIdx := "", 0
	if v, ok := sessions.Load(path); ok {
		s := v.(*TTFFSession)
		hash, fileIdx = s.hash, s.fileIndex
	}
	if hash == "" {
		if v, ok := app.playbackRegistry.Load(path); ok {
			if ps, ok := v.(*PlaybackState); ok {
				hash = ps.Hash
			}
		}
	}
	origin := stallOriginOf(path, hash, fileIdx, off, n, d)
	keepaliveCounters.stalls[confirmedIdx(hashConfirmed(hash))][cause][origin][role].Add(1)
}
