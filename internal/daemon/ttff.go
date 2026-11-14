package daemon

import (
	"flodni/internal/gostorm/native"
	"flodni/internal/warmup"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// ttffSource identifies which Read() path served the data. Used as histogram key.
type ttffSource uint8

const (
	srcWarmupHead ttffSource = iota
	srcWarmupTail
	srcRACacheHit
	srcFetchBlock
)

func (s ttffSource) String() string {
	switch s {
	case srcWarmupHead:
		return "warmup_head"
	case srcWarmupTail:
		return "warmup_tail"
	case srcRACacheHit:
		return "racache_hit"
	case srcFetchBlock:
		return "fetch_block"
	}
	return "unknown"
}

// Fixed logarithmic bucket upper bounds, milliseconds. Bucket i covers
// (buckets[i-1], buckets[i]]; the final implicit bucket is [10000,∞).
var ttffBucketsMS = [...]int64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 3000, 10000}

// TTFFHistogram is a fixed-bucket latency histogram with zero allocations.
// Percentiles are computed on demand by a cumulative scan (O(len(buckets))).
type TTFFHistogram struct {
	buckets [len(ttffBucketsMS) + 1]atomic.Int64
	total   atomic.Int64
	sumMS   atomic.Int64
	maxMS   atomic.Int64
}

func (h *TTFFHistogram) Add(d time.Duration) {
	ms := d.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	idx := 0
	for idx < len(ttffBucketsMS) && ms > ttffBucketsMS[idx] {
		idx++
	}
	h.buckets[idx].Add(1)
	h.total.Add(1)
	h.sumMS.Add(ms)
	for {
		cur := h.maxMS.Load()
		if ms <= cur || h.maxMS.CompareAndSwap(cur, ms) {
			break
		}
	}
}

func (h *TTFFHistogram) Count() int64 { return h.total.Load() }

// snapshot copies the per-bucket counts (not cumulative; the last one is everything
// above the final bound) and the sum in ms. Add updates bucket, total and sum as three
// separate atomics, so a scrape taking the count from total could disagree with the
// buckets it just read; callers take the count from these buckets instead.
func (h *TTFFHistogram) snapshot() (counts [len(ttffBucketsMS) + 1]int64, sumMS int64) {
	for i := range h.buckets {
		counts[i] = h.buckets[i].Load()
	}
	return counts, h.sumMS.Load()
}

func (h *TTFFHistogram) Avg() float64 {
	total := h.total.Load()
	if total == 0 {
		return 0
	}
	return float64(h.sumMS.Load()) / float64(total)
}

func (h *TTFFHistogram) Max() float64 { return float64(h.maxMS.Load()) }

// Pct returns the bucket upper bound (ms) at which the cumulative count
// reaches p*100% of samples. p must be in [0,1]; the result is an
// approximate snapshot under concurrent Add. The final bucket reports
// Max() as representative.
func (h *TTFFHistogram) Pct(p float64) float64 {
	total := h.total.Load()
	if total == 0 {
		return 0
	}
	// Floored at 1: p*total truncates to 0 for a single sample, and the scan below would
	// then be satisfied by the first (empty) bucket, reporting 1ms whatever the sample was.
	need := int64(p * float64(total))
	if need < 1 {
		need = 1
	}
	var cum int64
	for i := 0; i < len(h.buckets); i++ {
		cum += h.buckets[i].Load()
		if cum >= need {
			if i < len(ttffBucketsMS) {
				return float64(ttffBucketsMS[i])
			}
			return float64(h.maxMS.Load())
		}
	}
	return 0
}

const (
	ttffIdleClose     = 60 * time.Second
	ttffMaxLife       = 30 * time.Minute
	ttffProbeMinRead  = int64(1 << 20)  // 1MB: Plex probes never read this much
	ttffTailMinRead   = int64(64 << 10) // 64KB: scanners read at most one Cues probe chunk
	ttffDeepReadOff   = int64(8 << 20)  // 8MB: header/seek probes go deeper
	ttffStallDuration = time.Second
	// sessionVerdictFloor: below this a session that served nothing proves nothing,
	// because the caller did not wait out even one full fetch cycle (3 x
	// fetchBlockTimeout = 24s). Measured over 217 sessions: 58% end under 20s, those
	// are scanners reading the header and closing, and a single one falls between 20
	// and 30. Not 30: a failed playback closes right around 30s and the measurement
	// has one-second granularity, so the boundary would cut through the cases that
	// matter most.
	sessionVerdictFloor = 25 * time.Second
)

// TTFFSession tracks one playback open of a virtual MKV (per-path, shared by
// primary/secondary handles like playbackRegistry).
type TTFFSession struct {
	path            string
	size            int64
	hash            string
	fileIndex       int    // the index the stub carries (0 = none), for the session log line
	id              uint64 // process-unique, for the session log and resumed_from
	resumedFrom     uint64 // id of the session this one continues; 0 = a fresh open
	warmupHeadReady atomic.Bool
	warmupTailReady atomic.Bool
	// coldEvicted: the session opened with no ready head on a file quota eviction had
	// removed. The webhook that confirms the play counts it, once (see filestate.go).
	coldEvicted atomic.Bool

	openedAt    atomic.Int64 // unixNano
	firstDataAt atomic.Int64 // 0 = unset (CAS-once)
	// When the player first read deep (off>=8MB) or in the tail, not how long 8MB took
	// to arrive: a client that probes the Cues stamps it at once, one that plays from 0
	// only once playback has consumed that much.
	firstDeepReadAt atomic.Int64 // 0 = unset (CAS-once)
	lastReadAt      atomic.Int64
	bytesRead       atomic.Int64
	reads           atomic.Int64 // served reads, for /api/sessions
	// Bytes handed to the caller per serving path, for /api/sessions and the
	// [Session] line. They name the path that answered, not where the bytes came from.
	bytesFetchBlock atomic.Int64
	bytesRACacheHit atomic.Int64
	bytesWarmupHead atomic.Int64
	bytesWarmupTail atomic.Int64
	failedReads     atomic.Int64 // reads that gave up with EAGAIN/EIO, for /api/sessions
	lastOff         atomic.Int64 // start offset of previous read; -1 = none
	seekCount       atomic.Int64
	stallCount      atomic.Int64
	maxStallMS      atomic.Int64
	tailOrDeep      atomic.Bool // read at off>=8MB or inside last 16MB (tail region)

	reopens atomic.Int64 // read-path wakes of a closed torrent, for /api/sessions
	// confirmed is sticky: a webhook confirmed the path while the session was open, or
	// it was confirmed at register. The state at close would read false after a stop.
	confirmed atomic.Bool

	// readFailed records that a read gave up with EAGAIN or EIO, the two exits that
	// mean the swarm did not deliver. EINTR (the caller left) and ETIMEDOUT (our own
	// slot semaphore was full) are deliberately not recorded: neither says anything
	// about the release, and the second fires precisely under the load that would
	// make a healthy title look dead.
	readFailed atomic.Bool
	// failReason is why the last failed read gave up, classified at that read:
	// by verdict time the engine may have expired the torrent and everything would
	// read as no peers. Nil until a read fails.
	failReason atomic.Pointer[string]
	// servedByNet records that a read was served straight from the swarm
	// (srcFetchBlock). Its counterpart above condemns; this one acquits. Deliberately
	// not bytesRead: that counts what the player received, and the SSD warmup and the
	// read-ahead cache feed it too, so a release nobody is sharing any more would keep
	// clearing its own counter off a 64MB file on local disk.
	servedByNet atomic.Bool

	// Reader budget (reader_budget.go): bytes outside the head and tail windows taken while
	// the file had no confirmed playback, split by the read path that answered (engine
	// or read-ahead cache), for the log line. overMarginAt is when the tally crossed the margin (CAS-once,
	// unixNano); confirmedLateAt is when a playback webhook then confirmed the file.
	unconfirmedSwarm atomic.Int64
	unconfirmedCache atomic.Int64
	overMarginAt     atomic.Int64
	confirmedLateAt  atomic.Int64
	budgetResolved   atomic.Bool            // sessions_total counted (CAS-once)
	callerPid        atomic.Uint32          // the latest Open's caller, from the FUSE context
	overMarginComm   atomic.Pointer[string] // the caller's process name when the session crossed the margin
	callerUID        atomic.Uint32

	// budgetFloor is the tally when a webhook last confirmed the file: refusal counts
	// from there, so a stopped play does not leave the next reader pre-spent.
	// refusedReads is how many reads the gate refused; confirmedAfterRefusal makes the
	// confirmed-after-refusal counter once per session.
	budgetFloor           atomic.Int64
	refusedReads          atomic.Int64
	confirmedAfterRefusal atomic.Bool

	closeOnce sync.Once
	closed    atomic.Bool
}

// recordRead updates session state for one served FUSE read. lastReadAt is
// refreshed on every served read.
func (s *TTFFSession) recordRead(src ttffSource, d time.Duration, n int, off int64) {
	if s.closed.Load() {
		return
	}
	nowN := time.Now().UnixNano()
	s.lastReadAt.Store(nowN)
	s.reads.Add(1)
	if n > 0 {
		s.firstDataAt.CompareAndSwap(0, nowN)
		s.bytesRead.Add(int64(n))
		switch src {
		case srcWarmupHead:
			s.bytesWarmupHead.Add(int64(n))
		case srcWarmupTail:
			s.bytesWarmupTail.Add(int64(n))
		case srcRACacheHit:
			s.bytesRACacheHit.Add(int64(n))
		case srcFetchBlock:
			s.bytesFetchBlock.Add(int64(n))
		}
		s.countUnconfirmed(src, n, off)
		// The source alone is not enough: the fetch path also hands back whatever is
		// already local, and a dead release once returned 38 bytes of residue through
		// it and acquitted itself with them, wiping six recorded failures. A release
		// with no connection has nobody to have answered, whatever came out of the
		// pipe. Stats() is only consulted until the flag is set, so a healthy session
		// pays for it once.
		if src == srcFetchBlock && !s.servedByNet.Load() && sessionActivePeers(s.hash) > 0 {
			s.servedByNet.Store(true)
		}
		if off >= ttffDeepReadOff || (s.size > 0 && off >= warmup.TailStart(s.size)) {
			s.tailOrDeep.Store(true)
			s.firstDeepReadAt.CompareAndSwap(0, nowN)
		}
		if prev := s.lastOff.Load(); prev > 0 {
			if b := gc(); b != nil && shouldInterruptForSeek(prev, off, b.ReadAheadBudget) {
				s.seekCount.Add(1)
				ttffStats.seekLatency.Add(d)
			}
		} else if prev == 0 {
			// Previous data read was the header at offset 0 — a served read,
			// not an uninitialized offset (those are seeded -1 by ttffRegister,
			// and shouldInterruptForSeek ignores prevOff<=0). A jump beyond the
			// read-ahead budget from the header is a real seek.
			if b := gc(); b != nil && off > b.ReadAheadBudget {
				s.seekCount.Add(1)
				ttffStats.seekLatency.Add(d)
			}
		}
		s.lastOff.Store(off)
	}
	if d > ttffStallDuration {
		s.stallCount.Add(1)
		ms := d.Milliseconds()
		for {
			cur := s.maxStallMS.Load()
			if ms <= cur || s.maxStallMS.CompareAndSwap(cur, ms) {
				break
			}
		}
	}
}

// ttffIsReal decides at close time whether a session was a real playback
// (vs a Plex/Samba scan probe). Scanners read almost nothing — even a tail
// Cues probe stays within one chunk (see tailProbeZoneSize docs in pump.go);
// any 1MB+ of data, or a tail/deep read of more than 64KB, is playback.
func ttffIsReal(tailOrDeep bool, bytesRead int64) bool {
	if bytesRead >= ttffProbeMinRead {
		return true
	}
	return tailOrDeep && bytesRead > ttffTailMinRead
}

// sessionActivePeers reports the connections a release currently has. A var so the
// verdict can be exercised without a live torrent.
var sessionActivePeers = native.ActivePeers

// reportSwarmVerdict turns one finished session into a verdict. It acquits on bytes
// that came from the swarm, and condemns a release when a whole session, however long its
// caller chose to keep it open, ended without a single byte served and with a read
// that gave up. The observation window is the session rather than one 8s fetch:
// that timeout exists to keep a FUSE read under the smbd D-state watchdog, and
// reading a swarm's health out of it was answering a question it was never asked.
//
// What it will not do is acquit on any byte served: a session fed entirely by the
// SSD warmup would clear the counter of a release the swarm never touched, which is
// how a half-dead title stays invisible. Only srcFetchBlock counts as an answer.
//
// Runs before the ttffIsReal filter below on purpose: a session that served nothing
// is exactly what that filter drops, and exactly what this needs to see.
//
// Returns true exactly when it logged the [DeadSwarm] line and condemned the release;
// /api/sessions reports that session as dead_swarm.
func (s *TTFFSession) reportSwarmVerdict() bool {
	if native.ReachabilityOutcome == nil || s.hash == "" {
		return false
	}
	// The swarm answered at some point in this session: that clears whatever the
	// counter had gathered, however the session ended afterwards. Without this the
	// count only ever grows, and a release broken for a day by a tracker outage is
	// reaped after it has already recovered.
	if s.servedByNet.Load() {
		native.ReachabilityOutcome(native.Outcome{Hash: s.hash, Kind: native.OutcomeSwarmRead})
		return false
	}
	// Deliberately not bytesRead: it counts what the player received, and the SSD
	// warmup feeds it too. A dead release left a 38-byte warmup file behind, served it
	// back on the next session, and suppressed its own condemnation with it. The same
	// measure decides both directions: only the swarm answers for a release.
	if !s.readFailed.Load() {
		return false
	}
	if opened := s.openedAt.Load(); opened == 0 ||
		time.Since(time.Unix(0, opened)) < sessionVerdictFloor {
		return false
	}
	logger.Printf("[DeadSwarm] session of %s ended after %s with a failed read and nothing served",
		filepath.Base(s.path), time.Since(time.Unix(0, s.openedAt.Load())).Round(time.Second))
	reason := failReasonUnknown
	if r := s.failReason.Load(); r != nil {
		reason = *r
	}
	native.ReachabilityOutcome(native.Outcome{
		Hash: s.hash, Kind: native.OutcomeFailed, Reason: reason, Source: failSourceMount,
	})
	return true
}

// closeSession records the session in the ring, writes its [Session] line, then
// aggregates it into the global histograms and logs one [TTFF] line (real
// playbacks only). Idempotent via closeOnce. Removes itself from the registry.
func (s *TTFFSession) closeSession() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		if sessions.CompareAndDelete(s.path, s) {
			lastClosed.Store(s.path, closedInfo{id: s.id, size: s.size, hash: s.hash,
				fileIndex: s.fileIndex, headReady: s.warmupHeadReady.Load(), tailReady: s.warmupTailReady.Load()})
		}
		s.resolveReaderBudget()
		dead := s.reportSwarmVerdict()
		// Before the ttffIsReal filter: probes and unserved sessions get a line too.
		rec := s.record(time.Now(), dead)
		countSessionClosed(rec.Outcome)
		closedSessions.add(rec)
		logSessionRecord(rec)
		if !ttffIsReal(s.tailOrDeep.Load(), s.bytesRead.Load()) {
			ttffStats.sessionsFiltered.Add(1)
			return
		}
		ttffStats.sessionsCompleted.Add(1)
		opened := s.openedAt.Load()
		if fd := s.firstDataAt.Load(); fd > 0 {
			ttffStats.openToHeader.Add(time.Duration(fd - opened))
		}
		if dr := s.firstDeepReadAt.Load(); dr > 0 {
			ttffStats.openToDeepRead.Add(time.Duration(dr - opened))
		}
		if sc := s.stallCount.Load(); sc > 0 {
			ttffStats.stallCount.Add(sc)
		}
		if ms := s.maxStallMS.Load(); ms > 0 {
			for {
				cur := ttffStats.maxStallMS.Load()
				if ms <= cur || ttffStats.maxStallMS.CompareAndSwap(cur, ms) {
					break
				}
			}
		}
		logger.Printf("[TTFF] path=%s head=%d tail=%d header=%s deepread=%s seek=%d stalls=%d bytes=%d",
			filepath.Base(s.path),
			boolToInt(s.warmupHeadReady.Load()), boolToInt(s.warmupTailReady.Load()),
			durOrDash(s.firstDataAt.Load(), opened), durOrDash(s.firstDeepReadAt.Load(), opened),
			s.seekCount.Load(), s.stallCount.Load(), s.bytesRead.Load())
	})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func durOrDash(t, opened int64) string {
	if t == 0 {
		return "-"
	}
	d := time.Duration(t - opened)
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// --- Registry ---

var sessions sync.Map // path -> *TTFFSession

type ttffAgg struct {
	sessionsCompleted atomic.Int64
	sessionsFiltered  atomic.Int64
	openToHeader      TTFFHistogram
	openToDeepRead    TTFFHistogram
	seekLatency       TTFFHistogram
	warmupHead        TTFFHistogram
	warmupTail        TTFFHistogram
	raCacheHit        TTFFHistogram
	fetchBlock        TTFFHistogram
	stallCount        atomic.Int64
	maxStallMS        atomic.Int64
	// readBytes is what every served read handed to the kernel, per serving path
	// (indexed by ttffSource). Unlike the per-session byte fields it also counts reads
	// after a session closed and reads with no session, so it can exceed their sum.
	readBytes [srcFetchBlock + 1]atomic.Int64
}

var ttffStats ttffAgg

// sessionOutcomes lists the outcomes a closed session can have, in the order the
// metrics print them. The open outcome never reaches a close.
var sessionOutcomes = [...]string{outcomeServed, outcomeProbe, outcomeUnserved, outcomeDeadSwarm}

var sessionsClosedByOutcome [len(sessionOutcomes)]atomic.Int64

func countSessionClosed(outcome string) {
	for i, o := range sessionOutcomes {
		if o == outcome {
			sessionsClosedByOutcome[i].Add(1)
			return
		}
	}
}

// ttffRegister opens (or reopens) the session for a path. Reopening refreshes
// lastReadAt and the warmup flags, but openedAt is refreshed only while the
// session has not served data yet (probe-then-player): refreshing it after the
// first read would produce a negative or distorted openToHeader (observed
// header=-75ms: a second Open mid-session refreshed openedAt past firstDataAt).
// Non-torrent files (hash=="") are skipped.
func ttffRegister(path string, size int64, hash string, fileIndex int, headReady, tailReady bool) {
	if hash == "" {
		return
	}
	nowN := time.Now().UnixNano()
	ns := &TTFFSession{path: path, size: size, hash: hash, fileIndex: fileIndex, id: sessionSeq.Add(1)}
	confirmed := playbackConfirmed(path)
	ns.confirmed.Store(confirmed)
	ns.warmupHeadReady.Store(headReady)
	ns.warmupTailReady.Store(tailReady)
	ns.openedAt.Store(nowN)
	ns.lastReadAt.Store(nowN)
	ns.lastOff.Store(-1)
	// A fresh open is not a continuation of whatever closed before it.
	lastClosed.Delete(path)
	if actual, loaded := sessions.LoadOrStore(path, ns); loaded {
		s := actual.(*TTFFSession)
		if s.closed.Load() {
			sessions.Store(path, ns) // resurrect after close
			return
		}
		if s.firstDataAt.Load() == 0 {
			s.openedAt.Store(nowN)
			s.lastOff.Store(-1)
		}
		s.lastReadAt.Store(nowN)
		s.warmupHeadReady.Store(headReady)
		s.warmupTailReady.Store(tailReady)
		if confirmed {
			s.confirmed.Store(true)
		}
	}
}

// ttffRead records one served read: per-source histogram always; session state
// when a session exists for the path.
func ttffRead(path string, src ttffSource, d time.Duration, n int, off int64) {
	if n > 0 && int(src) < len(ttffStats.readBytes) {
		ttffStats.readBytes[src].Add(int64(n))
	}
	switch src {
	case srcWarmupHead:
		ttffStats.warmupHead.Add(d)
	case srcWarmupTail:
		ttffStats.warmupTail.Add(d)
	case srcRACacheHit:
		ttffStats.raCacheHit.Add(d)
	case srcFetchBlock:
		ttffStats.fetchBlock.Add(d)
	}
	if d > ttffStallDuration {
		countStall(path)
	}
	if s := sessionForRead(path); s != nil {
		s.recordRead(src, d, n, off)
	}
}

// ttffReadFailed marks the session behind path as having had a read give up. Only
// the two exits that mean the swarm did not deliver call it (EAGAIN, EIO).
func ttffReadFailed(path string) {
	if s := sessionForRead(path); s != nil {
		s.failedReads.Add(1)
		// Classified before the flag is set: a session closing meanwhile then sees
		// either no failure or a failure that already has its reason.
		reason := classifyFailure(s.hash)
		s.failReason.Store(&reason)
		s.readFailed.Store(true)
	}
}

// closedInfo is what a closed session leaves behind so a later read on a handle that
// is still open can continue it: the identity to link to and the file's facts.
type closedInfo struct {
	id                   uint64
	size                 int64
	hash                 string
	fileIndex            int
	headReady, tailReady bool
}

var (
	sessionSeq atomic.Uint64
	// lastClosed holds the newest closed session per path until a read resumes it, a
	// fresh Open replaces it, or a release closes the path's last handle. A path that
	// closes and is never touched again keeps one small entry.
	lastClosed sync.Map // path -> closedInfo
	resumeMu   sync.Mutex
)

// sessionForRead returns the session a read on path belongs to. A session closes
// after 60s idle (30s past ttffMaxLife) or when a probe releases, while a handle may
// still be open: a paused playback. The first read after such a close opens a new
// session on the same handle, linked to the one that closed (resumed_from), so the
// pause is one session per stretch of reads and each verdict judges its own reads.
// Only a read gets here: a probe's release never opens one. resumeMu is taken only
// when no session exists, never on the hot path.
func sessionForRead(path string) *TTFFSession {
	if val, ok := sessions.Load(path); ok {
		return val.(*TTFFSession)
	}
	resumeMu.Lock()
	defer resumeMu.Unlock()
	if val, ok := sessions.Load(path); ok { // another read resumed it first
		return val.(*TTFFSession)
	}
	v, ok := lastClosed.LoadAndDelete(path)
	if !ok {
		return nil
	}
	ci := v.(closedInfo)
	nowN := time.Now().UnixNano()
	ns := &TTFFSession{path: path, size: ci.size, hash: ci.hash, fileIndex: ci.fileIndex,
		id: sessionSeq.Add(1), resumedFrom: ci.id}
	ns.confirmed.Store(playbackConfirmed(path))
	ns.warmupHeadReady.Store(ci.headReady)
	ns.warmupTailReady.Store(ci.tailReady)
	ns.openedAt.Store(nowN)
	ns.lastReadAt.Store(nowN)
	ns.lastOff.Store(-1)
	sessions.Store(path, ns)
	return ns
}

// ttffReleaseClose closes the session immediately on Release when nothing was
// ever read (scan probe). Guarded by the open-handle tracker: a probe handle
// releasing while a player has the same path open (even with 0 bytes read —
// cold-start Wake window) must not kill the player's session; those sessions
// are reaped by ttffCleanupLoop instead. Note: at this point the releasing
// handle is still counted by the tracker (Dec runs later in Release), so only
// slot-less probes close immediately — exactly the intended behavior.
func ttffReleaseClose(path string) {
	if globalOpenTracker.IsPathOpen(path) {
		return
	}
	if val, ok := sessions.Load(path); ok {
		if s := val.(*TTFFSession); s.bytesRead.Load() == 0 {
			s.closeSession()
			// Nobody has the path open: nothing is left to resume.
			lastClosed.Delete(s.path)
		}
	}
}

// Test-only overrides of the session idle close and the cleanup tick, read once at
// start. They exist so the end-to-end variants that wait for a session to close do not
// wait minutes; they are not configuration.
const (
	envTestSessionIdleClose = "FLODNI_TEST_SESSION_IDLE_CLOSE"
	envTestSessionTick      = "FLODNI_TEST_SESSION_TICK"
	ttffTestMin             = time.Second
	ttffTestMax             = 10 * time.Minute
	ttffDefaultTick         = 60 * time.Second
)

// ttffTiming returns the idle close and the cleanup tick: the defaults, or the
// test-only environment values when they parse as Go durations within 1s..10m. A value
// outside that is ignored. notes says what was applied or ignored, for one log line each.
func ttffTiming(getenv func(string) string) (idle, tick time.Duration, notes []string) {
	idle, tick = ttffIdleClose, ttffDefaultTick
	for _, o := range []struct {
		name string
		dst  *time.Duration
	}{{envTestSessionIdleClose, &idle}, {envTestSessionTick, &tick}} {
		raw := getenv(o.name)
		if raw == "" {
			continue
		}
		d, err := time.ParseDuration(raw)
		if err != nil || d < ttffTestMin || d > ttffTestMax {
			notes = append(notes, fmt.Sprintf("%s=%q ignored: want a duration from %s to %s", o.name, raw, ttffTestMin, ttffTestMax))
			continue
		}
		*o.dst = d
		notes = append(notes, fmt.Sprintf("%s=%s (test only)", o.name, d))
	}
	return idle, tick, notes
}

// ttffCleanupLoop closes sessions idle for the idle close (ttffIdleClose unless a test
// overrides it), or idle beyond 30s once older than ttffMaxLife (zombie handles with
// periodic probe reads); active playbacks are never closed by age.
func ttffCleanupLoop() {
	idleClose, tickEvery, notes := ttffTiming(os.Getenv)
	for _, n := range notes {
		logger.Printf("[Sessions] %s", n)
	}
	ticker := time.NewTicker(tickEvery)
	defer ticker.Stop()
	for range ticker.C {
		nowN := time.Now().UnixNano()
		sessions.Range(func(key, val interface{}) bool {
			s := val.(*TTFFSession)
			idle := nowN - s.lastReadAt.Load()
			if idle > int64(idleClose) ||
				(idle > int64(30*time.Second) && nowN-s.openedAt.Load() > int64(ttffMaxLife)) {
				s.closeSession()
			}
			return true
		})
	}
}

// histJSON renders a histogram as JSON for /metrics/ttff.
func histJSON(h *TTFFHistogram) string {
	return fmt.Sprintf(`{"avg":%.0f,"p50":%.0f,"p95":%.0f,"p99":%.0f,"max":%.0f,"count":%d}`,
		h.Avg(), h.Pct(0.50), h.Pct(0.95), h.Pct(0.99), h.Max(), h.Count())
}
