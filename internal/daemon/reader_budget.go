package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"flodni/internal/warmup"
)

// Reader budget. A file that no media server has confirmed as playing can still be read
// end to end by a scanner, a hasher or a copy. This file counts what such a reader takes
// past the head and tail windows (which the warmup cache serves anyway) and logs a
// session once when it crosses a margin (phase a, measuring).
//
// Phase b, opt-in refusal: the predicate (budgetBlocksFetch), the per-handle hold
// (budgetGate), the metrics and the log lines. handle.go asks the gate at the first cache
// miss and the predicate at its four prefetch sites; pump.go parks on the predicate. With
// refuse_unconfirmed_reads off (the default) nothing here refuses or slows a read.
//
// The Prometheus counter is what the reader took past the windows, whichever path
// served it: outside the exempt windows every such byte is swarm cost, even when it
// was filled earlier. The pump and the prefetches fill ahead of the reader. Their fills
// are attributed to the session of the file in a second tally (noteFillAttributed),
// always counted and exported; it moves the margin, and so when refusal starts, only
// with attribute_fills_to_reader on. The log line splits the served bytes by the read
// path that answered, for debugging: swarm_bytes waited for the engine (srcFetchBlock,
// which can answer from pieces it already holds), cache_bytes came from the read-ahead
// cache (srcRACacheHit). Neither is a count of bytes downloaded.
//
// Read-ahead overshoot: a miss inside the head window is never refused, and its
// read-ahead can run past the window edge into the middle. noteReadaheadOvershoot
// counts those bytes (always on); unconfirmed_readahead_clamp stops the read-ahead at
// the edge (clampReadahead, opt-in).

const readerBudgetPrefix = "[ReaderBudget] "

// Indexes into the global counters.
const (
	rbUnconfirmed   = 0
	rbConfirmedLate = 1
)

var (
	// unconfirmedMargin is a var so tests can change it.
	unconfirmedMargin = func() int64 {
		if c := gc(); c != nil {
			return c.UnconfirmedReadMargin
		}
		return 64 << 20
	}

	// refuseUnconfirmedReads is the opt-in switch, read at every call. A var so tests
	// can flip it.
	refuseUnconfirmedReads = func() bool {
		c := gc()
		return c != nil && c.RefuseUnconfirmedReads
	}

	// unconfirmedReadHold is how long the first refused read of a handle waits for a
	// webhook before it is refused; unconfirmedHoldPoll is how often the wait re-asks.
	// Vars so tests can shorten them; not config keys.
	unconfirmedReadHold = 15 * time.Second
	unconfirmedHoldPoll = 100 * time.Millisecond

	// The opt-in switches of the follow-ups, read at every call. Vars so tests can flip
	// them.
	unconfirmedReadaheadClamp = func() bool {
		c := gc()
		return c != nil && c.UnconfirmedReadaheadClamp
	}
	attributeFillsToReader = func() bool {
		c := gc()
		return c != nil && c.AttributeFillsToReader
	}
)

// The reader budget counters live on App (app.go): readaheadOvershootBytes is what the
// engine was asked to fetch as read-ahead past the head window edge for a read that
// started inside the window, on a file with no confirmed playback; attributedFillBytes is
// what the pump and the prefetches filled past the windows for a file with a session and
// no confirmed playback; the unconfirmed* counters count refusals and holds.

// Indexes into unconfirmedHolds.
const (
	rbHoldLifted  = 0
	rbHoldRefused = 1
)

// errnoBudgetRefused is what budgetGate returns for a refusal. It is private: MkvHandle.Read
// must map it to EIO before the kernel sees it (and before it reaches ttffReadFailed or the
// short-read repair), because a refusal is not the swarm failing to deliver. EDQUOT is a
// valid errno, so a leak would still be an answer the kernel can complete a request with.
// MkvHandle.Read maps it at both readInner call sites.
const errnoBudgetRefused = syscall.EDQUOT

// The rule that resolved a play or resume webhook, in the order they are tried.
// ruleLinked is a tie-break, not a step: it names an IMDb, bootstrap or fuzzy match
// that had several candidates and was settled by the one a library symlink points at.
const (
	ruleBasename = iota
	rulePathMap
	ruleIMDB
	ruleFuzzy
	ruleLinked
	ruleNone
	numWebhookRules
)

// webhookRuleNames are the values of the rule label of
// flodni_playback_webhook_match_rule_total, indexed by the constants above.
var webhookRuleNames = [numWebhookRules]string{"basename", "path_map", "imdb", "fuzzy", "linked", "none"}

// App.playbackWebhookRules counts play and resume webhooks by the rule that resolved
// them, since process start.

func countPlaybackWebhook(matched bool) {
	if matched {
		app.playbackWebhooksMatched.Add(1)
	} else {
		app.playbackWebhooksUnmatched.Add(1)
	}
}

// tailWindowLen is the length of the tail window of a file of this size, from the
// page-aligned start every window user shares (warmup.TailStart).
func tailWindowLen(size int64) int64 {
	if size <= 0 {
		return 0
	}
	return size - warmup.TailStart(size)
}

// bytesOutsideWindows is how many bytes of the read [off, off+n) fall outside the head
// window [0, head) and the tail window [size-tail, size). Windows are clamped to the
// file, and where they overlap (a short file) nothing is outside.
func bytesOutsideWindows(off, n, size, head, tail int64) int64 {
	if n <= 0 || size <= 0 {
		return 0
	}
	midStart := min(head, size)
	midEnd := max(size-tail, 0)
	lo, hi := max(off, midStart), min(off+n, midEnd)
	if hi <= lo {
		return 0
	}
	return hi - lo
}

// countUnconfirmed adds one served read to the session's tally when the engine or the
// read-ahead cache answered it, it lies past the windows, and the file has no confirmed
// playback. Called from recordRead, the funnel every served read passes.
func (s *TTFFSession) countUnconfirmed(src ttffSource, n int, off int64) {
	if src != srcFetchBlock && src != srcRACacheHit {
		return
	}
	out := bytesOutsideWindows(off, int64(n), s.size, warmup.FileSize, tailWindowLen(s.size))
	if out == 0 || playbackConfirmed(s.path) {
		return
	}
	app.unconfirmedReadBytes.Add(out)
	if src == srcFetchBlock {
		s.unconfirmedSwarm.Add(out)
	} else {
		s.unconfirmedCache.Add(out)
	}
	s.checkOverMargin()
}

// checkOverMargin logs the session once when its tally reaches the margin. Called after
// every addition to the tally.
func (s *TTFFSession) checkOverMargin() {
	if s.overMarginAt.Load() == 0 && s.budgetTally() >= unconfirmedMargin() &&
		s.overMarginAt.CompareAndSwap(0, time.Now().UnixNano()) {
		s.logReaderBudget("over_margin", nil, 0)
		// A read that passed the closed check can cross after the close resolved.
		if s.closed.Load() {
			s.resolveReaderBudget()
		}
	}
}

// budgetTally is what the margin is compared with: the bytes the reader took past the
// windows. With attribute_fills_to_reader on, the bytes the pump and the prefetches
// filled for it count too, as the larger of the two figures, not their sum: a filled
// byte the reader then reads is one byte of cost, seen from both ends.
func (s *TTFFSession) budgetTally() int64 {
	t := s.unconfirmedSwarm.Load() + s.unconfirmedCache.Load()
	if attributeFillsToReader() {
		t = max(t, s.unconfirmedFill.Load())
	}
	return t
}

// budgetSince is what the session has taken past its windows since the last confirmation:
// the served bytes since their floor, or with attribute_fills_to_reader on the fills since
// theirs if that is more. Each figure has its own floor, so the switch can be flipped while
// sessions are open without one tally being measured from the other's floor.
func (s *TTFFSession) budgetSince() int64 {
	d := s.unconfirmedSwarm.Load() + s.unconfirmedCache.Load() - s.budgetFloor.Load()
	if attributeFillsToReader() {
		d = max(d, s.unconfirmedFill.Load()-s.fillFloor.Load())
	}
	return d
}

// noteFillAttributed attributes a fill of [off, off+n) by the pump or a prefetch to the
// session of path, when the file has a session and no confirmed playback and the bytes
// lie past the windows. The windows are exempt, as for reads. Called after the fill
// reached the read-ahead cache. Always counted; it changes the margin only with
// attribute_fills_to_reader on.
func noteFillAttributed(path string, off, n, size int64) {
	v, ok := sessions.Load(path)
	if !ok {
		return
	}
	out := bytesOutsideWindows(off, n, size, warmup.FileSize, tailWindowLen(size))
	if out == 0 || playbackConfirmed(path) {
		return
	}
	s := v.(*TTFFSession)
	app.attributedFillBytes.Add(out)
	s.unconfirmedFill.Add(out)
	// Phase a's over_margin line and its session count follow the margin, which fills move
	// only with the switch on: with it off a pump fill must not log a reader that took
	// nothing (at a margin of 0 any byte would).
	if attributeFillsToReader() {
		s.checkOverMargin()
	}
}

// readaheadWindowStart reports whether a read starting at off lies inside the head or
// tail window of a file of this size.
func readaheadWindowStart(off, size int64) bool {
	return bytesOutsideWindows(off, 1, size, warmup.FileSize, tailWindowLen(size)) == 0
}

// readaheadOvershoot is how many bytes of a fetch [off, off+fetchSize) lie past the
// windows beyond what the read itself asked for [off, off+target): the read-ahead the
// budget never saw. Zero unless the read started inside a window.
func readaheadOvershoot(off, target, fetchSize, size int64) int64 {
	if !readaheadWindowStart(off, size) {
		return 0
	}
	head, tail := warmup.FileSize, tailWindowLen(size)
	return max(bytesOutsideWindows(off, fetchSize, size, head, tail)-bytesOutsideWindows(off, target, size, head, tail), 0)
}

// noteReadaheadOvershoot counts the overshoot of one fetch the engine was asked for, on
// a file with no confirmed playback. Always on; it only counts. The size is what was
// asked of the engine, not what it has downloaded.
func noteReadaheadOvershoot(path string, off, target, fetchSize, size int64) {
	if over := readaheadOvershoot(off, target, fetchSize, size); over > 0 && !playbackConfirmed(path) {
		app.readaheadOvershootBytes.Add(over)
	}
}

// clampReadahead returns the last byte a read-ahead fetch may reach. With
// unconfirmed_readahead_clamp on, a read that starts inside the head window of a file
// with no confirmed playback is read ahead only to the window edge, and never short of
// the bytes the read itself asked for (end). Otherwise it returns fetchEnd as it is.
func clampReadahead(path string, off, end, fetchEnd, size int64) int64 {
	if !unconfirmedReadaheadClamp() {
		return fetchEnd
	}
	head := min(warmup.FileSize, size)
	if off >= head || fetchEnd < head || playbackConfirmed(path) {
		return fetchEnd
	}
	return min(fetchEnd, max(end, head-1))
}

// pastMargin reports whether the session has taken at least the margin past its windows
// since the last confirmation (budgetSince). With margin 0 that holds from
// the start.
func (s *TTFFSession) pastMargin() bool {
	return s.budgetSince() >= unconfirmedMargin()
}

// overUnconfirmedMargin reports whether this session has taken at least the margin past
// its windows since the last confirmation and the file is unconfirmed now.
func (s *TTFFSession) overUnconfirmedMargin() bool {
	return s.pastMargin() && !playbackConfirmed(s.path)
}

// refusalEnforced reports whether the gate would refuse now: the switch is on and a play
// webhook has matched a file since start.
func refusalEnforced() bool {
	return refuseUnconfirmedReads() && app.playbackWebhooksMatched.Load() > 0
}

// blocks asks the predicate for one session: would a fetch for [off, off+n) of a file of
// size be refused? Cheapest checks first; the switch, the arming and the session lookup
// are the caller's.
func (s *TTFFSession) blocks(off, n, size int64) bool {
	if !s.pastMargin() {
		return false
	}
	if bytesOutsideWindows(off, n, size, warmup.FileSize, tailWindowLen(size)) == 0 {
		return false
	}
	return !playbackConfirmed(s.path)
}

// enforcedSession returns the session behind path while refusal is enforced, else nil.
func enforcedSession(path string) *TTFFSession {
	if !refusalEnforced() {
		return nil
	}
	if v, ok := sessions.Load(path); ok {
		return v.(*TTFFSession)
	}
	return nil
}

// budgetBlocksFetch is the refusal predicate: true when a fetch for [off, off+n) of path
// must not start. The switch is on and a play webhook has matched since start, the path
// has a session, the session is past its margin since the last confirmation, some
// requested byte lies outside the windows, and the file has no confirmed playback. Bytes
// already cached are the caller's to serve; the windows are exempt by offset.
func budgetBlocksFetch(path string, off, n, size int64) bool {
	s := enforcedSession(path)
	return s != nil && s.blocks(off, n, size)
}

// budgetHold is one handle's wait for a webhook. The first refused read starts it; reads
// that arrive meanwhile share its deadline; when it runs out every later read on the
// handle is refused at once. A confirmation (or the predicate letting go for any other
// reason) re-arms it. The zero value is ready.
type budgetHold struct {
	mu       sync.Mutex
	deadline time.Time // zero = no hold running
	expired  bool
	held     atomic.Bool // deadline or expired is set; lets rearm skip the lock
}

// begin returns the running hold's deadline, starting a hold when none runs. expired
// reports a hold that already ran out: the read is refused at once.
func (b *budgetHold) begin(now time.Time) (deadline time.Time, expired bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.expired {
		return time.Time{}, true
	}
	if b.deadline.IsZero() {
		b.deadline = now.Add(unconfirmedReadHold)
		b.held.Store(true)
	}
	return b.deadline, false
}

// lift ends a running hold; true for the one caller that ended it (it counts the lift).
func (b *budgetHold) lift() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	had := !b.deadline.IsZero() && !b.expired
	b.deadline, b.expired = time.Time{}, false
	b.held.Store(false)
	return had
}

// expire marks the hold run out; true for the one caller that did (it counts the refusal).
func (b *budgetHold) expire() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	first := !b.expired
	b.expired = true
	return first
}

// rearm forgets a hold without counting it (a gate call that finds nothing to refuse);
// cheap when nothing is held.
func (b *budgetHold) rearm() {
	if !b.held.Load() {
		return
	}
	b.lift()
}

// budgetGate is asked before a fetch for [off, off+n) starts. It returns 0 when the fetch
// may go ahead and errnoBudgetRefused when it must not (the caller maps that to EIO).
// A refused read is first held, once per handle, for unconfirmedReadHold: a webhook that
// arrives late confirms the file and lets the read through, so a play the media server
// confirmed after the margin is not cut. The hold polls the predicate; EINTR when ctx
// ends first. After the hold runs out, reads on the handle are refused at once, until a
// confirmation, a fresh margin or the switch going off re-arms it (a read inside a
// window neither starts nor re-arms a hold). The hold is tied to the session it started under: if that
// session closes, the hold is refused, not lifted (a closed session no longer has a
// tally to compare with). Each refusal and the hold's start refresh the session's
// lastReadAt, so a refused session is not idle-closed under a handle that is still
// open. Refusals never reach ttffReadFailed.
func (h *MkvHandle) budgetGate(ctx context.Context, off int64, n int) syscall.Errno {
	s := enforcedSession(h.path)
	if s == nil || !s.pastMargin() || playbackConfirmed(h.path) {
		h.budgetHold.rearm()
		return 0
	}
	if !s.blocks(off, int64(n), h.size) {
		return 0 // inside the windows: always served, hold untouched
	}
	start := time.Now()
	s.lastReadAt.Store(start.UnixNano())
	deadline, expired := h.budgetHold.begin(start)
	if expired {
		return s.refuse(off, 0)
	}
	tick := time.NewTicker(unconfirmedHoldPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return syscall.EINTR
		case <-tick.C:
		}
		if s.closed.Load() {
			if h.budgetHold.expire() {
				app.unconfirmedHolds[rbHoldRefused].Add(1)
			}
			return s.refuse(off, time.Since(start))
		}
		if !refusalEnforced() || !s.blocks(off, int64(n), h.size) {
			if h.budgetHold.lift() {
				app.unconfirmedHolds[rbHoldLifted].Add(1)
			}
			return 0
		}
		if !time.Now().Before(deadline) {
			if h.budgetHold.expire() {
				app.unconfirmedHolds[rbHoldRefused].Add(1)
			}
			return s.refuse(off, time.Since(start))
		}
	}
}

// refuse accounts one refused read of the session and returns the refusal errno. The
// first refusal of a session writes the "refused" line.
func (s *TTFFSession) refuse(off int64, held time.Duration) syscall.Errno {
	s.lastReadAt.Store(time.Now().UnixNano())
	app.unconfirmedRefusedReads.Add(1)
	if s.refusedReads.Add(1) == 1 {
		s.logReaderBudget("refused", &off, held.Milliseconds())
	}
	return errnoBudgetRefused
}

// noteConfirmedLate is called when a webhook matched path as playing. It starts the
// session's refusal tally afresh (budgetFloor) and counts a session that had been refused
// once as confirmed after refusal. If the session had already crossed the margin, that crossing was a playback the budget would have
// cut: log it once. The session counter follows at close.
func noteConfirmedLate(path string) {
	v, ok := sessions.Load(path)
	if !ok {
		return
	}
	s := v.(*TTFFSession)
	s.confirmed.Store(true)
	// Before the early returns: a refusal needs the fresh start whether or not the
	// session ever crossed in phase a's sense.
	s.budgetFloor.Store(s.unconfirmedSwarm.Load() + s.unconfirmedCache.Load())
	s.fillFloor.Store(s.unconfirmedFill.Load())
	if s.refusedReads.Load() > 0 && s.confirmedAfterRefusal.CompareAndSwap(false, true) {
		app.unconfirmedConfirmedAfterRefusal.Add(1)
	}
	if s.overMarginAt.Load() == 0 || s.closed.Load() || s.budgetResolved.Load() {
		return
	}
	if s.confirmedLateAt.CompareAndSwap(0, time.Now().UnixNano()) {
		s.logReaderBudget("confirmed_late", nil, 0)
	}
}

// resolveReaderBudget counts a session that crossed the margin exactly once, at close,
// as confirmed_late when a webhook confirmed it afterwards and unconfirmed otherwise.
// closeSession calls it inside its closeOnce.
func (s *TTFFSession) resolveReaderBudget() {
	if s.overMarginAt.Load() == 0 || !s.budgetResolved.CompareAndSwap(false, true) {
		return
	}
	if s.confirmedLateAt.Load() != 0 {
		app.unconfirmedSessions[rbConfirmedLate].Add(1)
	} else {
		app.unconfirmedSessions[rbUnconfirmed].Add(1)
		// The comm was read at the crossing (logReaderBudget): the caller may be gone by now.
		comm := ""
		if p := s.overMarginComm.Load(); p != nil {
			comm = *p
		}
		webhookStatus.noteUnconfirmedRead(comm)
	}
}

// noteReaderCaller records who opened path, from the FUSE request context. It is the
// latest Open on the path, not necessarily the process that reads.
func noteReaderCaller(path string, pid, uid uint32) {
	if v, ok := sessions.Load(path); ok {
		s := v.(*TTFFSession)
		s.callerPid.Store(pid)
		s.callerUID.Store(uid)
	}
}

// readerComm is the process name of pid, or "" when it cannot be read (no pid, a
// process in another PID namespace, or one that already exited).
func readerComm(pid uint32) string {
	if pid == 0 {
		return ""
	}
	b, err := os.ReadFile(filepath.Join("/proc", strconv.FormatUint(uint64(pid), 10), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

type readerBudgetRecord struct {
	Event      string  `json:"event"`
	Hash       string  `json:"hash"`
	FileIndex  int     `json:"file_index"`
	Size       int64   `json:"size"`
	SwarmBytes int64   `json:"swarm_bytes"`
	CacheBytes int64   `json:"cache_bytes"`
	Margin     *int64  `json:"margin,omitempty"`
	OpenAgeMS  *int64  `json:"open_age_ms,omitempty"`
	Comm       *string `json:"comm,omitempty"`
	Pid        *uint32 `json:"pid,omitempty"`
	UID        *uint32 `json:"uid,omitempty"`
	Enforced   *bool   `json:"enforced,omitempty"`
	LateMS     *int64  `json:"late_ms,omitempty"`
	// refused event: the offset of the first refused read and how long it was held.
	Offset *int64 `json:"offset,omitempty"`
	HeldMS *int64 `json:"held_ms,omitempty"`
	// confirmed_late event: reads the gate refused before the confirmation.
	RefusedReads *int64 `json:"refused_reads,omitempty"`
	// Bytes the pump and the prefetches filled past the windows for the session; omitted
	// at 0. They count toward the margin only with attribute_fills_to_reader on.
	FillBytes int64 `json:"fill_bytes,omitempty"`
}

// logReaderBudget writes one line with no path or file name: the hash and file index
// identify the file, as in the [Session] line. comm is read here, not per read.
//
// off and heldMS are set for the "refused" event only.
func (s *TTFFSession) logReaderBudget(event string, off *int64, heldMS int64) {
	rec := readerBudgetRecord{
		Event: event, Hash: s.hash, FileIndex: s.fileIndex, Size: s.size,
		SwarmBytes: s.unconfirmedSwarm.Load(), CacheBytes: s.unconfirmedCache.Load(), FillBytes: s.unconfirmedFill.Load(),
	}
	if event == "confirmed_late" {
		late := max((s.confirmedLateAt.Load()-s.overMarginAt.Load())/int64(time.Millisecond), 0)
		refused := s.refusedReads.Load()
		rec.LateMS, rec.RefusedReads = &late, &refused
	} else {
		margin := unconfirmedMargin()
		age := max(time.Since(time.Unix(0, s.openedAt.Load())).Milliseconds(), 0)
		comm, pid, uid, enforced := readerComm(s.callerPid.Load()), s.callerPid.Load(), s.callerUID.Load(), refusalEnforced()
		rec.Margin, rec.OpenAgeMS = &margin, &age
		rec.Comm, rec.Pid, rec.UID, rec.Enforced = &comm, &pid, &uid, &enforced
		if event == "over_margin" {
			s.overMarginComm.Store(&comm)
		}
		if event == "refused" {
			rec.Offset, rec.HeldMS = off, &heldMS
		}
	}
	line, err := json.Marshal(rec)
	if err != nil {
		logger.Warn(fmt.Sprintf("WARNING: reader budget line for %s not written: %v", s.hash, err), "hash", s.hash, "err", err)
		return
	}
	logger.Info(fmt.Sprintf("%s%s", readerBudgetPrefix, line))
}
