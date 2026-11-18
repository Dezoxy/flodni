package daemon

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"flodni/internal/promtext"
)

// Fill pacing. Several streams that start together each fill their whole lead at the
// speed the swarm allows, which is a burst of bulk traffic that the players do not need
// yet. The pacer meters only that bulk: the part of a pump's lead past a must-serve margin
// M. The bytes a player is about to read (the lead up to M) are never gated, a probe or a
// scan never draws, and with fewer than two playing pumps the gate is not entered.
const (
	paceBurstChunks = 2                      // bucket depth in chunks
	paceWaiterTTL   = 300 * time.Millisecond // a waiter not seen this long no longer holds the line
	paceViewTTL     = 10 * time.Second       // a pump that has not reported in this long is gone
	paceFillWindow  = 300 * time.Second      // a pump older than this is no longer "filling"
	paceEWMAWindow  = 5 * time.Second        // fill_rate_bytes smoothing
	paceFoldEvery   = 250 * time.Millisecond // shortest span one rate sample covers
	pacePark        = 100 * time.Millisecond // the same park as the hard limit
	paceLogEvery    = 10 * time.Second       // [Pace] lines are rate-limited to one per kind per span
)

// paceClock is the pacer's seam for time: the park sleeps through it, so a test runs the
// whole pacer on a fake clock.
type paceClock struct {
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration)
}

func realPaceClock() paceClock {
	return paceClock{
		Now: time.Now,
		Sleep: func(ctx context.Context, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
			case <-t.C:
			}
		},
	}
}

// paceMargin is M, the lead a pump may always build with no tokens: the configured margin,
// at most half the pump's lead limit, never under two chunks.
func paceMargin(margin, leadLimit, chunkSize int64) int64 {
	return max(min(margin, leadLimit/2), 2*chunkSize)
}

// paceAnchor is the position a pump's lead is measured from: the player's, or, when no
// primary handle is reporting one (playerOff <= 0, see primaryFollowOffset), where the
// pump started.
func paceAnchor(playerOff, startOff int64) int64 {
	if playerOff > 0 {
		return playerOff
	}
	return startOff
}

// paceFilling reports whether a pump is still filling its lead: below 70% of its limit (the
// soft limit) and younger than paceFillWindow.
func paceFilling(lead, limit int64, age time.Duration) bool {
	return age < paceFillWindow && lead*10 < limit*7
}

type paceWaiter struct {
	lead int64
	seen time.Time
}

type paceView struct {
	lead, limit int64
	started     time.Time
	seen        time.Time
}

// fillPacer is the global token bucket and its bookkeeping.
type fillPacer struct {
	clock paceClock

	mu       sync.Mutex
	armed    bool // the bucket is filled when the first draw of an engaged span arrives
	tokens   float64
	burst    float64
	last     time.Time
	waiters  map[string]paceWaiter
	views    map[string]paceView
	ewma     float64 // bulk bytes per second
	lastFold time.Time
	// One engaged span, for the [Pace] lines.
	engagedAt   time.Time
	bulkAtStart int64
	peak        float64
	lastLog     map[string]time.Time

	engaged   atomic.Bool
	mustBytes atomic.Int64
	bulkBytes atomic.Int64
	bulkAcc   atomic.Int64 // bulk bytes not yet folded into ewma
	waitNS    atomic.Int64
	stalls    atomic.Int64
	draws     atomic.Int64
	fullLead  paceHist
}

func newFillPacer(c paceClock) *fillPacer {
	return &fillPacer{clock: c, waiters: map[string]paceWaiter{}, views: map[string]paceView{}, lastLog: map[string]time.Time{}}
}

// fillPace is the process's pacer.
var fillPace = newFillPacer(realPaceClock())

// Draw asks for one chunk of bulk. A pump whose lead is at or under margin is always
// granted and touches neither the tokens nor the queue. Otherwise it is granted only when
// the bucket holds a chunk and no waiter seen in the last paceWaiterTTL has a smaller lead
// (the pump closest to its player is served first). rate is bytes per second.
func (p *fillPacer) Draw(id string, lead, margin, chunk, rate int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if lead <= margin {
		delete(p.waiters, id)
		return true
	}
	now := p.clock.Now()
	p.refillLocked(now, rate, chunk)
	p.waiters[id] = paceWaiter{lead: lead, seen: now}
	first := true
	for k, w := range p.waiters {
		if now.Sub(w.seen) > paceWaiterTTL {
			delete(p.waiters, k)
		} else if k != id && w.lead < lead {
			first = false
		}
	}
	if !first || p.tokens < float64(chunk) {
		return false
	}
	p.tokens -= float64(chunk)
	delete(p.waiters, id)
	p.draws.Add(1)
	return true
}

func (p *fillPacer) refillLocked(now time.Time, rate, chunk int64) {
	p.burst = float64(paceBurstChunks * chunk)
	if !p.armed {
		p.armed, p.tokens, p.last = true, p.burst, now
		return
	}
	if dt := now.Sub(p.last); dt > 0 {
		p.tokens = math.Min(p.burst, p.tokens+float64(rate)*dt.Seconds())
		p.last = now
	}
}

// Refund gives back tokens a draw took for bytes that were not read after all (the chunk
// was cached already, or the read came up short).
func (p *fillPacer) Refund(n int64) {
	if n <= 0 {
		return
	}
	p.mu.Lock()
	p.tokens = math.Min(p.burst, p.tokens+float64(n))
	p.mu.Unlock()
}

// Forget drops everything the pacer holds for a pump that ended.
func (p *fillPacer) Forget(id string) {
	p.mu.Lock()
	delete(p.waiters, id)
	delete(p.views, id)
	p.mu.Unlock()
}

// Observe records where a playing pump stands and returns whether pacing is engaged: at
// least one playing pump is still filling. Engaging and disengaging log a [Pace] line.
func (p *fillPacer) Observe(id string, started time.Time, lead, limit, streams int64) bool {
	var msg string
	p.mu.Lock()
	now := p.clock.Now()
	p.views[id] = paceView{lead: lead, limit: limit, started: started, seen: now}
	filling := 0
	for k, v := range p.views {
		if now.Sub(v.seen) > paceViewTTL || !pathPlaybackLive(k) {
			delete(p.views, k)
			continue
		}
		if paceFilling(v.lead, v.limit, paceAge(now, v.started)) {
			filling++
		}
	}
	p.foldLocked(now)
	engaged := filling > 0
	if engaged && p.engaged.CompareAndSwap(false, true) {
		p.armed = false
		p.engagedAt, p.bulkAtStart, p.peak = now, p.bulkBytes.Load(), 0
		msg = p.logLimitedLocked("engaged", now, fmt.Sprintf("[Pace] engaged streams=%d filling=%d", streams, filling))
	} else if !engaged && p.engaged.CompareAndSwap(true, false) {
		msg = p.disengageMsgLocked(now)
	}
	p.mu.Unlock()
	if msg != "" {
		logger.Info(msg)
	}
	return engaged
}

// Idle disengages pacing when fewer than two pumps play or the feature is off. Callers
// check engaged first, so the common case costs one atomic load.
func (p *fillPacer) Idle() {
	if !p.engaged.Load() {
		return
	}
	var msg string
	p.mu.Lock()
	if p.engaged.CompareAndSwap(true, false) {
		now := p.clock.Now()
		p.foldLocked(now)
		msg = p.disengageMsgLocked(now)
	}
	p.mu.Unlock()
	if msg != "" {
		logger.Info(msg)
	}
}

func (p *fillPacer) disengageMsgLocked(now time.Time) string {
	span := now.Sub(p.engagedAt)
	bulk := p.bulkBytes.Load() - p.bulkAtStart
	return p.logLimitedLocked("disengaged", now, fmt.Sprintf("[Pace] disengaged after %.0fs bulk=%d MiB peak=%.0f Mbit/s",
		span.Seconds(), bulk>>20, p.peak*8/1e6))
}

// logLimitedLocked returns msg unless a line of this kind went out within paceLogEvery.
func (p *fillPacer) logLimitedLocked(kind string, now time.Time, msg string) string {
	if t, ok := p.lastLog[kind]; ok && now.Sub(t) < paceLogEvery {
		return ""
	}
	p.lastLog[kind] = now
	return msg
}

func paceAge(now, started time.Time) time.Duration {
	if started.IsZero() {
		return 0
	}
	return now.Sub(started)
}

// NoteFill counts bytes a pump filled, by class, and feeds the bulk rate.
func (p *fillPacer) NoteFill(bulk bool, n int64) {
	if n <= 0 {
		return
	}
	if bulk {
		p.bulkBytes.Add(n)
		p.bulkAcc.Add(n)
		return
	}
	p.mustBytes.Add(n)
}

// foldLocked folds the bulk bytes since the last fold into the 5 s moving average.
func (p *fillPacer) foldLocked(now time.Time) {
	if p.lastFold.IsZero() {
		p.lastFold = now
		return
	}
	dt := now.Sub(p.lastFold)
	if dt < paceFoldEvery {
		return
	}
	inst := float64(p.bulkAcc.Swap(0)) / dt.Seconds()
	p.ewma += (1 - math.Exp(-dt.Seconds()/paceEWMAWindow.Seconds())) * (inst - p.ewma)
	p.lastFold = now
	if p.engaged.Load() && p.ewma > p.peak {
		p.peak = p.ewma
	}
}

// Rate is the 5 s moving average of bulk fill, bytes per second.
func (p *fillPacer) Rate() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.foldLocked(p.clock.Now())
	return p.ewma
}

// Park sleeps one park through the clock and books it as paced wait, on the pacer and on
// the path's open session.
func (p *fillPacer) Park(path string) {
	p.clock.Sleep(context.Background(), pacePark)
	p.waitNS.Add(int64(pacePark))
	if v, ok := sessions.Load(path); ok {
		v.(*TTFFSession).pacedNS.Add(int64(pacePark))
	}
}

// paceHist is the time-to-full-lead histogram, in seconds.
type paceHist struct {
	counts [len(paceFullBounds) + 1]atomic.Int64
	sumMS  atomic.Int64
}

var paceFullBounds = [...]float64{1, 2, 5, 10, 20, 30, 60, 120, 300}

func (h *paceHist) Observe(d time.Duration) {
	s := d.Seconds()
	i := 0
	for i < len(paceFullBounds) && s > paceFullBounds[i] {
		i++
	}
	h.counts[i].Add(1)
	h.sumMS.Add(d.Milliseconds())
}

func (h *paceHist) snapshot() promtext.Histogram {
	counts := make([]int64, len(h.counts))
	for i := range h.counts {
		counts[i] = h.counts[i].Load()
	}
	return promtext.Histogram{Bounds: paceFullBounds[:], Counts: counts, Sum: float64(h.sumMS.Load()) / 1000}
}

// startedTime is when the pump began, on the pacer's clock; zero for a pump that has not set it.
func (ps *NativePumpState) startedTime() time.Time {
	if ns := ps.startedAt.Load(); ns != 0 {
		return time.Unix(0, ns)
	}
	return time.Time{}
}

// pumpStateFor is the pump registered for path, nil when there is none.
func pumpStateFor(path string) *NativePumpState {
	if v, ok := app.activePumps.Load(path); ok {
		if ps, ok := v.(*NativePumpState); ok {
			return ps
		}
	}
	return nil
}

// paceMarginFor is M for this pump under the live configuration.
func paceMarginFor(leadLimit, chunkSize int64) int64 {
	return paceMargin(gc().FillPaceMarginMB<<20, leadLimit, chunkSize)
}

// paceGate runs between the hard and the soft limit of nativePumpChunk. n is the number of
// playing pumps. It returns denied when the pump must wait (the caller returns and the loop
// runs again after the park) and drawn when a chunk's tokens were taken, to be refunded if
// the chunk is not read. Only the bulk beyond M of a playing pump is ever denied.
func (h *MkvHandle) paceGate(ps *NativePumpState, n, offset, chunkSize, playerOff, leadLimit int64) (denied, drawn bool) {
	cfg := gc()
	if !cfg.FillPaceEnabled || n < 2 {
		fillPace.Idle()
		return false, false
	}
	if ps == nil || !h.playbackIsLive() { // a probe or a scan never draws
		return false, false
	}
	lead := offset - paceAnchor(playerOff, ps.startOff.Load())
	margin := paceMargin(cfg.FillPaceMarginMB<<20, leadLimit, chunkSize)
	if !fillPace.Observe(h.path, ps.startedTime(), lead, leadLimit, n) || lead <= margin {
		return false, false
	}
	rate := int64(cfg.FillPaceMbit) * 1_000_000 / 8
	if fillPace.Draw(h.path, lead, margin, chunkSize, rate) {
		return false, true
	}
	ps.pacedUntil.Store(fillPace.clock.Now().Add(2 * pacePark).UnixNano())
	fillPace.Park(h.path)
	return true, false
}

// paceAfterRead updates what a chunk read changed: the fill counters by class, the pump's
// place in the pacer, and the time to a full lead once.
func (h *MkvHandle) paceAfterRead(ps *NativePumpState, n, offset, readLen, chunkSize, playerOff, leadLimit int64) {
	if ps == nil {
		return
	}
	cfg := gc()
	anchor := paceAnchor(playerOff, ps.startOff.Load())
	margin := paceMargin(cfg.FillPaceMarginMB<<20, leadLimit, chunkSize)
	fillPace.NoteFill(offset-anchor > margin, readLen)
	newLead := offset + readLen - anchor
	if newLead*10 >= leadLimit*7 && ps.fullSeen.CompareAndSwap(false, true) {
		if t := ps.startedTime(); !t.IsZero() {
			fillPace.fullLead.Observe(fillPace.clock.Now().Sub(t))
		}
	}
	if n >= 2 && cfg.FillPaceEnabled && h.playbackIsLive() {
		fillPace.Observe(h.path, ps.startedTime(), newLead, leadLimit, n)
	}
}

// noteFetchStall counts a read that missed the read-ahead cache and went to FetchBlock while
// the pacer had its path's pump parked. The counter should stay near 0.
func noteFetchStall(path string) {
	if ps := pumpStateFor(path); ps != nil && fillPace.clock.Now().UnixNano() < ps.pacedUntil.Load() {
		fillPace.stalls.Add(1)
	}
}

// collectPaceProm writes the pacing series. All are present from 0.
func collectPaceProm(w *promtext.Writer) {
	w.Counter("flodni_pump_fill_bytes_total",
		"Bytes the pumps filled, by class: must_serve (within the margin of the player, never paced) or bulk (beyond it, what fill pacing meters).",
		promSample(fillPace.mustBytes.Load(), lbl("class", "must_serve")),
		promSample(fillPace.bulkBytes.Load(), lbl("class", "bulk")))
	w.Gauge("flodni_pump_fill_rate_bytes", "Bulk fill rate in bytes per second, a 5 s moving average.",
		promtext.Sample{Value: fillPace.Rate()})
	engaged := int64(0)
	if fillPace.engaged.Load() {
		engaged = 1
	}
	w.Gauge("flodni_pump_pacing_engaged", "1 while fill pacing is limiting bulk fills (two or more playing pumps, one still filling), else 0.", promSample(engaged))
	w.Counter("flodni_pump_paced_wait_seconds_total", "Seconds pumps spent parked by fill pacing, summed over pumps.",
		promtext.Sample{Value: float64(fillPace.waitNS.Load()) / 1e9})
	w.Histogram("flodni_pump_time_to_full_lead_seconds", "Time from a pump's start to its lead reaching the soft limit (70% of its lead limit), once per pump.",
		fillPace.fullLead.snapshot())
	w.Counter("flodni_pump_paced_stalls_total", "Reads that missed the read-ahead cache and went to FetchBlock while fill pacing had their path's pump parked. Expected to stay near 0.",
		promSample(fillPace.stalls.Load()))
}
