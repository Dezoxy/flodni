package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"flodni/internal/promtext"
)

// fakePaceClock is the pacer's clock in a test: Sleep returns at once and the test moves
// time itself, so a round of parks (one per pump, all at the same moment) is one step.
type fakePaceClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakePaceClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakePaceClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func (c *fakePaceClock) clock() paceClock {
	return paceClock{Now: c.now, Sleep: func(context.Context, time.Duration) {}}
}

// usePaceClock swaps the process pacer for one on a fake clock, restored on cleanup.
func usePaceClock(t *testing.T) *fakePaceClock {
	t.Helper()
	clk := &fakePaceClock{t: time.Unix(1_000_000, 0)}
	prev := fillPace
	fillPace = newFillPacer(clk.clock())
	t.Cleanup(func() { fillPace = prev })
	return clk
}

// useFillPaceConfig sets the pacing keys the way a default config has them.
func useFillPaceConfig(t *testing.T, mbit int, marginMB int64) {
	t.Helper()
	cfg := *gc()
	cfg.FillPaceEnabled, cfg.FillPaceMbit, cfg.FillPaceMarginMB = true, mbit, marginMB
	globalConfig.Store(&cfg)
}

const (
	paceChunk = 4 * mb
	testBurst = paceBurstChunks * paceChunk // what the gate derives from a 4 MiB read_ahead_base
)

// drain empties a fresh pacer's bucket (it starts with its burst, two chunks).
func drain(t *testing.T, p *fillPacer, rate int64) {
	t.Helper()
	for i := 0; i < paceBurstChunks; i++ {
		if !p.Draw("drain", 1000*mb, 10*mb, paceChunk, rate, testBurst) {
			t.Fatalf("burst draw %d was denied", i)
		}
	}
	if p.Draw("drain", 1000*mb, 10*mb, paceChunk, rate, testBurst) {
		t.Fatal("a third draw was granted from an empty bucket")
	}
}

func TestPaceMarginAndAnchor(t *testing.T) {
	t.Parallel()
	// M = min(margin, limit/2), never below one chunk.
	for _, c := range []struct{ margin, limit, chunk, want int64 }{
		{128 * mb, 1000 * mb, paceChunk, 128 * mb},
		{128 * mb, 100 * mb, paceChunk, 50 * mb},
		{128 * mb, 4 * mb, paceChunk, paceChunk},
		{0, 1000 * mb, paceChunk, paceChunk},
	} {
		if got := paceMargin(c.margin, c.limit, c.chunk); got != c.want {
			t.Errorf("paceMargin(%d, %d, %d) = %d, want %d", c.margin, c.limit, c.chunk, got, c.want)
		}
	}
	// Case 8: with no player position the lead is measured from where the pump started.
	if got := paceAnchor(0, 3*gb); got != 3*gb {
		t.Errorf("playerOff 0: anchor %d, want the start offset", got)
	}
	if got := paceAnchor(-1, 3*gb); got != 3*gb {
		t.Errorf("playerOff -1: anchor %d, want the start offset", got)
	}
	if got := paceAnchor(5*gb, 3*gb); got != 5*gb {
		t.Errorf("playerOff set: anchor %d, want the player", got)
	}
}

// Case 2: with the bucket empty, a pump inside the margin is granted, every time, and takes nothing.
func TestPaceEmptyBucketStillServesTheMargin(t *testing.T) {
	clk := &fakePaceClock{t: time.Unix(1, 0)}
	p := newFillPacer(clk.clock())
	const rate = 10 * mb
	drain(t, p, rate)
	for i := 0; i < 50; i++ {
		if !p.Draw("a", 10*mb, 10*mb, paceChunk, rate, testBurst) {
			t.Fatalf("draw %d at the margin was denied with an empty bucket", i)
		}
		if !p.Draw("b", 0, 10*mb, paceChunk, rate, testBurst) {
			t.Fatalf("draw %d at lead 0 was denied", i)
		}
	}
	if p.Draw("c", 10*mb+1, 10*mb, paceChunk, rate, testBurst) {
		t.Fatal("bulk was granted from an empty bucket")
	}
	if got := p.draws.Load(); got != paceBurstChunks {
		t.Fatalf("draws = %d, want %d (the margin takes no tokens)", got, paceBurstChunks)
	}
}

// Case 3: seven pumps hungry for bulk are admitted at most rate*dt + burst.
func TestPaceSevenPumpsAdmitAtMostRateTimesTimePlusBurst(t *testing.T) {
	clk := &fakePaceClock{t: time.Unix(1, 0)}
	p := newFillPacer(clk.clock())
	const rate, margin = 10 * mb, 10 * mb
	leads := make([]int64, 7)
	for i := range leads {
		leads[i] = 200*mb + int64(i)*mb
	}
	var granted int64
	const secs = 60
	for step := 0; step < secs*10; step++ {
		for i := range leads {
			if p.Draw(fmt.Sprint("p", i), leads[i], margin, paceChunk, rate, testBurst) {
				granted += paceChunk
				leads[i] += paceChunk
			}
		}
		clk.advance(100 * time.Millisecond)
	}
	if limit := rate*secs + paceBurstChunks*paceChunk; granted > limit {
		t.Fatalf("admitted %d bytes in %d s, over rate*dt+burst = %d", granted, secs, limit)
	}
	if floor := rate*secs - 2*paceChunk; granted < floor {
		t.Fatalf("admitted %d bytes in %d s, under %d: the bucket is stuck", granted, secs, floor)
	}
}

// Case 4: a pump that joins late and sits nearest its player is served before the older ones.
func TestPaceLateJoinerIsServedFirst(t *testing.T) {
	clk := &fakePaceClock{t: time.Unix(1, 0)}
	p := newFillPacer(clk.clock())
	const rate, margin = 40 * mb, 10 * mb // a chunk of tokens every 100 ms
	drain(t, p, rate)
	for _, c := range []struct {
		id   string
		lead int64
	}{{"old-a", 100 * mb}, {"old-b", 120 * mb}, {"late", margin + paceChunk}} {
		if p.Draw(c.id, c.lead, margin, paceChunk, rate, testBurst) {
			t.Fatalf("%s drew from an empty bucket", c.id)
		}
	}
	clk.advance(100 * time.Millisecond) // one chunk of tokens, and the three waiters are still fresh
	if p.Draw("old-a", 100*mb, margin, paceChunk, rate, testBurst) || p.Draw("old-b", 120*mb, margin, paceChunk, rate, testBurst) {
		t.Fatal("an older pump was served ahead of the late joiner")
	}
	if !p.Draw("late", margin+paceChunk, margin, paceChunk, rate, testBurst) {
		t.Fatal("the late joiner was not served first")
	}
}

// Case 5: leads equalize: after enough admitted bytes the spread is within one chunk.
func TestPaceLeadsEqualizeWithinAChunk(t *testing.T) {
	clk := &fakePaceClock{t: time.Unix(1, 0)}
	p := newFillPacer(clk.clock())
	const rate, margin = 40 * mb, 10 * mb
	leads := []int64{20 * mb, 40 * mb, 60 * mb}
	for step := 0; step < 600; step++ {
		for i := range leads {
			if p.Draw(fmt.Sprint("p", i), leads[i], margin, paceChunk, rate, testBurst) {
				leads[i] += paceChunk
			}
		}
		clk.advance(100 * time.Millisecond)
	}
	lo, hi := leads[0], leads[0]
	for _, l := range leads {
		lo, hi = min(lo, l), max(hi, l)
	}
	if hi-lo > paceChunk {
		t.Fatalf("leads %v differ by %d MiB, more than one chunk", leads, (hi-lo)/mb)
	}
}

// Case 6: a pump that jumps (its lead falls to the margin) or ends leaves no waiter that
// holds the others back.
func TestPaceJumpLeavesNoStuckWaiter(t *testing.T) {
	const rate, margin = 40 * mb, 10 * mb
	t.Run("jump", func(t *testing.T) {
		clk := &fakePaceClock{t: time.Unix(1, 0)}
		p := newFillPacer(clk.clock())
		drain(t, p, rate)
		if p.Draw("a", 50*mb, margin, paceChunk, rate, testBurst) || p.Draw("b", 100*mb, margin, paceChunk, rate, testBurst) {
			t.Fatal("drew from an empty bucket")
		}
		if !p.Draw("a", margin, margin, paceChunk, rate, testBurst) { // a's player seeked: a is at the margin now
			t.Fatal("the jumped pump was denied")
		}
		clk.advance(100 * time.Millisecond)
		if !p.Draw("b", 100*mb, margin, paceChunk, rate, testBurst) {
			t.Fatal("b is held back by the waiter a left behind")
		}
	})
	t.Run("gone", func(t *testing.T) {
		clk := &fakePaceClock{t: time.Unix(1, 0)}
		p := newFillPacer(clk.clock())
		drain(t, p, rate)
		p.Draw("a", 50*mb, margin, paceChunk, rate, testBurst) // a waits once and never comes back
		p.Draw("b", 100*mb, margin, paceChunk, rate, testBurst)
		clk.advance(100 * time.Millisecond)
		if p.Draw("b", 100*mb, margin, paceChunk, rate, testBurst) {
			t.Fatal("b was granted while a was still a fresh waiter")
		}
		clk.advance(paceWaiterTTL)
		if !p.Draw("b", 100*mb, margin, paceChunk, rate, testBurst) {
			t.Fatal("b is still held back by a waiter that has gone quiet")
		}
	})
	t.Run("forget", func(t *testing.T) {
		clk := &fakePaceClock{t: time.Unix(1, 0)}
		p := newFillPacer(clk.clock())
		drain(t, p, rate)
		p.Draw("a", 50*mb, margin, paceChunk, rate, testBurst)
		p.Draw("b", 100*mb, margin, paceChunk, rate, testBurst)
		p.Forget("a") // a's pump ended
		clk.advance(100 * time.Millisecond)
		if !p.Draw("b", 100*mb, margin, paceChunk, rate, testBurst) {
			t.Fatal("b is held back by an ended pump")
		}
	})
}

// Case 7: pacing disengages once every playing pump is at its soft limit or older than 300 s.
func TestPaceDisengagesAtTheSoftLimit(t *testing.T) {
	useEngineReadPath(t)
	clk := usePaceClock(t)
	registerPump(t, "/test/pace-a.mkv", true)
	registerPump(t, "/test/pace-b.mkv", true)
	p := fillPace
	const limit = 100 * mb
	start := clk.now()
	if !p.Observe("/test/pace-a.mkv", "/test/pace-a.mkv", start, 10*mb, limit, 2) || !p.engaged.Load() {
		t.Fatal("a filling pump did not engage pacing")
	}
	if !p.Observe("/test/pace-b.mkv", "/test/pace-b.mkv", start, 90*mb, limit, 2) {
		t.Fatal("one pump at its soft limit disengaged pacing while the other still fills")
	}
	if p.Observe("/test/pace-a.mkv", "/test/pace-a.mkv", start, 71*mb, limit, 2) || p.engaged.Load() {
		t.Fatal("pacing stayed engaged with every pump at its soft limit")
	}
	// A pump below the soft limit but older than 300 s is not filling.
	clk.advance(paceFillWindow + time.Second)
	if p.Observe("/test/pace-a.mkv", "/test/pace-a.mkv", start, 10*mb, limit, 2) {
		t.Fatal("a pump older than 300 s still counts as filling")
	}
	if !p.Observe("/test/pace-a.mkv", "/test/pace-a.mkv", clk.now(), 10*mb, limit, 2) {
		t.Fatal("a fresh pump did not re-engage")
	}
	clk.advance(paceViewTTL + time.Second) // a pump that stops reporting drops out
	if p.Observe("/test/pace-b.mkv", "/test/pace-b.mkv", start, 90*mb, limit, 2) {
		t.Fatal("a silent pump's view still counts")
	}
}

// pacedPair registers two playing pumps and returns the first one's handle, state and path.
func pacedPair(t *testing.T, clk *fakePaceClock, startOff int64) (*MkvHandle, *NativePumpState) {
	t.Helper()
	const a, b = "/test/gate-a.mkv", "/test/gate-b.mkv"
	h := engineHandle(a, 8*gb)
	registerPump(t, a, true)
	registerPump(t, b, true)
	ps := pumpStateFor(a)
	ps.startOff.Store(startOff)
	ps.startedAt.Store(clk.now().UnixNano())
	return h, ps
}

// Case 8: playerOff == 0 falls back to the start offset. The fallback is live only for an
// offset within the lead limit of 0 (a deeper pump is parked by the hard limit before the gate,
// which measures from 0), and there it can only shrink the lead.
func TestPaceGateAnchorsAtTheStartWhenThePlayerIsUnknown(t *testing.T) {
	useEngineReadPath(t)
	clk := usePaceClock(t)
	useFillPaceConfig(t, 500, 128)
	h, ps := pacedPair(t, clk, 3*gb)
	const limit = 200 * mb // M = min(128, 100) = 100 MiB
	margin := paceMargin(128*mb, limit, paceChunk)

	if denied, drawn := h.paceGate(ps, 2, 3*gb+margin, paceChunk, 0, limit); denied || drawn {
		t.Fatalf("a chunk at the margin from the start offset drew (denied=%v drawn=%v)", denied, drawn)
	}
	if denied, drawn := h.paceGate(ps, 2, 3*gb+margin+paceChunk, paceChunk, 0, limit); denied || !drawn {
		t.Fatalf("a chunk past the margin from the start offset did not draw (denied=%v drawn=%v)", denied, drawn)
	}
	// With the player known, its position is the anchor instead.
	if _, drawn := h.paceGate(ps, 2, 3*gb+margin+2*paceChunk, paceChunk, 3*gb+paceChunk, limit); !drawn {
		t.Fatal("a known player position did not take over as the anchor")
	}
}

// Case 9: probes and scans never draw: a path with no live playback is never gated, whatever
// the other pumps are doing, and neither takes part in the queue.
func TestPaceProbesAndScansNeverDraw(t *testing.T) {
	useEngineReadPath(t)
	clk := usePaceClock(t)
	useFillPaceConfig(t, 8, 128) // a slow bucket, so a draw would be visible
	h, ps := pacedPair(t, clk, 0)
	const scan = "/test/gate-scan.mkv"
	registerPump(t, scan, false)
	scanH := engineHandle(scan, 8*gb)
	scanPS := pumpStateFor(scan)
	const limit = 200 * mb
	for i := int64(0); i < 40; i++ { // a scan reading far ahead of anything
		if denied, drawn := scanH.paceGate(scanPS, 3, 2*gb+i*paceChunk, paceChunk, 0, limit); denied || drawn {
			t.Fatalf("the scan pump was gated at chunk %d (denied=%v drawn=%v)", i, denied, drawn)
		}
	}
	if fillPace.draws.Load() != 0 || fillPace.waitNS.Load() != 0 {
		t.Fatalf("a scan drew: %d draws, %d ns waited", fillPace.draws.Load(), fillPace.waitNS.Load())
	}
	// A playing pump is unaffected by the scan's presence.
	if _, drawn := h.paceGate(ps, 3, 120*mb, paceChunk, 0, limit); !drawn {
		t.Fatal("the playing pump no longer draws")
	}
}

// Case 1: a single playing pump never enters the gate: no wait, no draw, no pacing state,
// and its reads are untouched, whatever the lead.
func TestPaceSinglePumpHasNoWaitsOrDraws(t *testing.T) {
	useEngineReadPath(t) // budget 64 MiB, 4 MiB chunks
	usePaceClock(t)
	useFillPaceConfig(t, 1, 0) // the slowest bucket and the smallest margin: any gate would show
	const path = "/test/pace-single.mkv"
	h := engineHandle(path, 8*gb)
	registerPump(t, path, true)
	rd := &fakeReader{}
	player := 100 * mb
	offset := player
	limit := streamLeadLimit(gc().ReadAheadBudget, 1, paceChunk)
	for offset-player < limit-paceChunk {
		stop, next := h.nativePumpChunk(rd, offset, paceChunk, player)
		if stop || next != offset+paceChunk {
			t.Fatalf("chunk at lead %d MiB: stop=%v next=%d", (offset-player)/mb, stop, next)
		}
		offset = next
	}
	if got := len(rd.readCalls()); got < 3 {
		t.Fatalf("only %d reads", got)
	}
	if d, w, e := fillPace.draws.Load(), fillPace.waitNS.Load(), fillPace.engaged.Load(); d != 0 || w != 0 || e {
		t.Fatalf("single pump: draws=%d wait=%d engaged=%v", d, w, e)
	}
	// The bytes are still counted by class.
	if fillPace.mustBytes.Load() == 0 || fillPace.bulkBytes.Load() == 0 {
		t.Fatalf("fill bytes not counted: must %d bulk %d", fillPace.mustBytes.Load(), fillPace.bulkBytes.Load())
	}
}

// The simulator: seven playing paths on the fake clock, each pump the real nativePumpChunk,
// the fake engine behind it. Every pump gets its must-serve margin before any pacing starts;
// the bulk beyond it is admitted at no more than the configured rate (plus the burst) while
// pacing is engaged, and pacing disengages once every pump holds its soft-limit lead.
func TestPaceSimulatorSevenPaths(t *testing.T) {
	runPaceSim(t, 256*mb, 4*mb, 7) // seven leads must fit the budget, or the cache evicts what the test checks
}

// At the default 16 MiB chunk with a 1 GiB budget, four streams still have bulk to pace.
func TestPaceSimulatorDefaultChunkLargeBudget(t *testing.T) {
	runPaceSim(t, 1024*mb, 16*mb, 4)
}

func runPaceSim(t *testing.T, budget, chunk int64, streams int) {
	useEngineReadPath(t)
	cfg := *gc()
	cfg.ReadAheadBudget, cfg.ReadAheadBase = budget, chunk
	globalConfig.Store(&cfg)
	prevPool := readBufferPool
	readBufferPool = &sync.Pool{New: func() any { b := make([]byte, chunk); return &b }}
	t.Cleanup(func() { readBufferPool = prevPool })
	const rateMbit = 8 // 1 MB/s
	const rate = rateMbit * 1_000_000 / 8
	useFillPaceConfig(t, rateMbit, 128)
	clk := usePaceClock(t)
	limit := streamLeadLimit(gc().ReadAheadBudget, int64(streams), chunk)
	margin := paceMargin(128*mb, limit, chunk)

	type simPump struct {
		path   string
		h      *MkvHandle
		rd     *fakeReader
		player int64
		offset int64
	}
	pumps := make([]*simPump, streams)
	for i := range pumps {
		path := fmt.Sprintf("/test/pace-sim-%d.mkv", i)
		registerPump(t, path, true)
		ps := pumpStateFor(path)
		player := int64(1+i) * gb
		ps.startOff.Store(player)
		ps.startedAt.Store(clk.now().UnixNano())
		pumps[i] = &simPump{path: path, h: engineHandle(path, 16*gb), rd: &fakeReader{}, player: player, offset: player}
	}
	step := func(sp *simPump) (advanced bool) {
		stop, next := sp.h.nativePumpChunk(sp.rd, sp.offset, chunk, sp.player)
		if stop {
			t.Fatalf("%s: the pump stopped", sp.path)
		}
		moved := next != sp.offset
		sp.offset = next
		return moved
	}

	// Phase 1: no time passes. Each pump reads its must-serve margin with an untouched bucket.
	span := int64(0)
	for _, sp := range pumps {
		for sp.offset-sp.player <= margin {
			if !step(sp) {
				t.Fatalf("%s: a chunk inside the margin was not read", sp.path)
			}
		}
		span = sp.offset - sp.player
		if !cachedAcross(sp.path, sp.player, sp.offset-1) {
			t.Fatalf("%s: the margin [0, %d MiB) is not in the cache before pacing", sp.path, span/mb)
		}
	}
	if d, b := fillPace.draws.Load(), fillPace.bulkBytes.Load(); d != 0 || b != 0 {
		t.Fatalf("before pacing: %d draws, %d bulk bytes", d, b)
	}
	if got, want := fillPace.mustBytes.Load(), int64(streams)*span; got != want {
		t.Fatalf("must-serve bytes %d, want %d", got, want)
	}

	// Phase 2: the bulk. Every round is 100 ms: each pump tries once, then the clock moves.
	startOfBulk := clk.now()
	var bulkAtEngagedEnd int64
	for round := 0; ; round++ {
		if round > 20000 {
			t.Fatal("the pumps never filled")
		}
		pending := 0
		for _, sp := range pumps {
			if !chunkWithinLead(sp.offset, sp.player, limit) {
				continue // parked at its hard limit: the loop would sleep there
			}
			pending++
			step(sp)
		}
		if pending == 0 {
			break
		}
		clk.advance(100 * time.Millisecond)
		if fillPace.engaged.Load() {
			elapsed := clk.now().Sub(startOfBulk)
			max := int64(float64(rate)*elapsed.Seconds()) + paceBurstChunks*chunk
			bulk := fillPace.bulkBytes.Load()
			if bulk > max {
				t.Fatalf("round %d: %d bulk bytes in %v while engaged, over rate*dt+burst = %d", round, bulk, elapsed, max)
			}
			bulkAtEngagedEnd = bulk
		}
	}
	if fillPace.engaged.Load() {
		t.Fatal("pacing is still engaged with every pump at its limit")
	}
	if bulkAtEngagedEnd < int64(streams)*(limit*7/10-margin)/2 {
		t.Fatalf("only %d bulk bytes went through the bucket", bulkAtEngagedEnd)
	}
	if fillPace.waitNS.Load() == 0 {
		t.Fatal("no pump was ever parked: the bucket did nothing at this rate")
	}
	for _, sp := range pumps {
		if lead := sp.offset - sp.player; lead < limit*7/10 {
			t.Errorf("%s ended at lead %d MiB, under its soft limit", sp.path, lead/mb)
		}
	}
	// The series render, with the class labels present.
	var w promtext.Writer
	collectPaceProm(&w)
	body := string(w.Bytes())
	for _, want := range []string{
		`flodni_pump_fill_bytes_total{class="must_serve"}`, `flodni_pump_fill_bytes_total{class="bulk"}`,
		"flodni_pump_fill_rate_bytes ", "flodni_pump_pacing_engaged 0\n", "flodni_pump_paced_wait_seconds_total ",
		"flodni_pump_time_to_full_lead_seconds_count ", "flodni_pump_paced_stalls_total 0\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("series %q missing from:\n%s", want, body)
		}
	}
}

// A read that misses the cache while the pacer has its pump parked is a paced stall; one
// that misses with the pump running is not.
func TestPaceStallsCountOnlyWhileParked(t *testing.T) {
	useEngineReadPath(t)
	clk := usePaceClock(t)
	const path = "/test/pace-stall.mkv"
	registerPump(t, path, true)
	noteFetchStall(path, false)
	if fillPace.stalls.Load() != 0 {
		t.Fatal("a stall was counted with the pump running")
	}
	pumpStateFor(path).pacedUntil.Store(clk.now().Add(pacePark).UnixNano())
	noteFetchStall(path, true)
	if fillPace.stalls.Load() != 0 {
		t.Fatal("a probe read was counted as a paced stall")
	}
	noteFetchStall(path, false)
	if fillPace.stalls.Load() != 1 {
		t.Fatal("a stall under the park was not counted")
	}
	clk.advance(time.Second)
	noteFetchStall(path, false)
	if fillPace.stalls.Load() != 1 {
		t.Fatal("a stall long after the park was counted")
	}
}

// Only a chunk that will be read draws: after a backward seek into cached territory the pump
// walks cached chunks, and the walk makes no draw and no park.
func TestPaceCachedChunksNeverDraw(t *testing.T) {
	useEngineReadPath(t)
	clk := usePaceClock(t)
	useFillPaceConfig(t, 1, 8)
	h, _ := pacedPair(t, clk, 0)
	player := 48 * mb // two playing pumps share 64 MiB: a 20 MiB lead limit, M = 8 MiB
	rd := &fakeReader{}
	for off := player; off < player+20*mb; off += paceChunk {
		raCache.Put(h.path, off, off+paceChunk-1, make([]byte, paceChunk))
	}
	for off := player; off <= player+16*mb; off += paceChunk {
		if _, next := h.nativePumpChunk(rd, off, paceChunk, player); next != off+paceChunk {
			t.Fatalf("the cached chunk at lead %d MiB was not skipped (next %d)", (off-player)/mb, next)
		}
	}
	if len(rd.readCalls()) != 0 {
		t.Fatal("a cached chunk was read")
	}
	if d, w := fillPace.draws.Load(), fillPace.waitNS.Load(); d != 0 || w != 0 {
		t.Fatalf("cached chunks made %d draws and %d ns of parks", d, w)
	}
}

// Tokens are taken for a chunk and given back for the part that was not read.
func TestPaceRefundsAShortRead(t *testing.T) {
	useEngineReadPath(t)
	clk := usePaceClock(t)
	useFillPaceConfig(t, 500, 8)
	h, _ := pacedPair(t, clk, 0)
	rd := &fakeReader{readFn: func(int, []byte, int64) (int, error) { return int(mb), nil }}
	player := 48 * mb
	if _, _ = h.nativePumpChunk(rd, player+12*mb, paceChunk, player); fillPace.draws.Load() != 1 {
		t.Fatalf("draws = %d, want 1", fillPace.draws.Load())
	}
	fillPace.mu.Lock()
	tokens := fillPace.tokens
	fillPace.mu.Unlock()
	if want := float64(testBurst - mb); tokens != want {
		t.Fatalf("tokens = %.0f, want %.0f (one MiB was read)", tokens, want)
	}
}

// A denied park is one park long for the stall counter, and a probe's read never counts.
func TestPaceDeniedParkIsExactlyOnePark(t *testing.T) {
	useEngineReadPath(t)
	clk := usePaceClock(t)
	useFillPaceConfig(t, 8, 8)
	h, ps := pacedPair(t, clk, 0)
	drain(t, fillPace, 1_000_000)
	if denied, _ := h.paceGate(ps, 2, 100*mb, paceChunk, 90*mb, 20*mb*10); !denied {
		t.Fatal("an empty bucket did not deny")
	}
	if got, want := ps.pacedUntil.Load(), clk.now().Add(pacePark).UnixNano(); got != want {
		t.Fatalf("pacedUntil = %d, want exactly one park ahead (%d)", got, want)
	}
}

// The burst is set by read_ahead_base, not by the chunk size of whoever calls: a 9 MiB caller
// neither shrinks it nor is starved beside a 16 MiB one, and tokens that accumulated are kept.
func TestPaceBurstDoesNotFollowTheCaller(t *testing.T) {
	clk := &fakePaceClock{t: time.Unix(1, 0)}
	p := newFillPacer(clk.clock())
	const burst, rate, margin = 32 * mb, 40 * mb, 10 * mb
	small, big := int64(9*mb), int64(16*mb)
	if !p.Draw("s", 100*mb, margin, small, rate, burst) { // arms the bucket, takes 9
		t.Fatal("first draw denied")
	}
	if p.Draw("s", 100*mb, margin, small, rate, burst) != true { // 32-9-9: still granted
		t.Fatal("second small draw denied")
	}
	clk.advance(time.Minute) // fills to the burst, not to two 9 MiB chunks
	for i := 0; i < 2; i++ {
		if !p.Draw("b", 100*mb, margin, big, rate, burst) {
			t.Fatalf("16 MiB draw %d was denied: the tokens were clamped to the last caller's chunk", i)
		}
	}
	// Fairness across sizes: lead goes to whoever is behind, whatever the chunk.
	leads := map[string]int64{"s": 50 * mb, "b": 50 * mb}
	size := map[string]int64{"s": small, "b": big}
	got := map[string]int{}
	for step := 0; step < 1200; step++ {
		for _, id := range []string{"s", "b"} {
			if p.Draw(id, leads[id], margin, size[id], rate, burst) {
				leads[id] += size[id]
				got[id]++
			}
		}
		clk.advance(100 * time.Millisecond)
	}
	if got["s"] == 0 || got["b"] == 0 || leads["s"]-leads["b"] > big || leads["b"]-leads["s"] > big {
		t.Fatalf("unfair across chunk sizes: grants %v, leads %d and %d MiB", got, leads["s"]/mb, leads["b"]/mb)
	}
}

// Views and waiters belong to the pump state: a pump whose entry was deleted elsewhere
// (terminateOrphanPump, a stop) still leaves the pacer when it ends, and pacing disengages at once.
func TestPaceForgetsAPumpWhoseEntryWasDeleted(t *testing.T) {
	useEngineReadPath(t)
	clk := usePaceClock(t)
	h, ps := pacedPair(t, clk, 0)
	other := pumpStateFor("/test/gate-b.mkv")
	const limit = 100 * mb
	fillPace.Observe(ps, h.path, clk.now(), 10*mb, limit, 2)
	fillPace.Observe(other, "/test/gate-b.mkv", clk.now(), 90*mb, limit, 2)
	if !fillPace.engaged.Load() {
		t.Fatal("not engaged")
	}
	app.activePumps.Delete(h.path) // deleted elsewhere: the "still registered" exit does not run
	h.hasSlot.Store(false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.nativePump(ctx, &fakeReader{}, 0, ps)
	fillPace.mu.Lock()
	_, stale := fillPace.views[any(ps)]
	fillPace.mu.Unlock()
	if stale || fillPace.engaged.Load() {
		t.Fatalf("the ended pump is still known to the pacer (view %v, engaged %v)", stale, fillPace.engaged.Load())
	}
}

// Disengaging and engaging again does not hand out a fresh burst each time: over any span the
// grants stay within rate*dt + burst.
func TestPaceFlappingDoesNotMintTokens(t *testing.T) {
	useEngineReadPath(t)
	clk := usePaceClock(t)
	registerPump(t, "/test/flap-a.mkv", true)
	registerPump(t, "/test/flap-b.mkv", true)
	p := fillPace
	const rate, margin, limit = 10 * mb, 10 * mb, 100 * mb
	lead := 200 * mb
	var granted int64
	const steps = 600
	for i := 0; i < steps; i++ {
		if i%2 == 0 { // a filling pump: engaged
			p.Observe("a", "/test/flap-a.mkv", clk.now(), 10*mb, limit, 2)
		} else { // everyone at the soft limit: disengaged
			p.Observe("a", "/test/flap-a.mkv", clk.now(), 90*mb, limit, 2)
		}
		if p.engaged.Load() && p.Draw("x", lead, margin, paceChunk, rate, testBurst) {
			granted += paceChunk
			lead += paceChunk
		}
		clk.advance(100 * time.Millisecond)
	}
	if max := int64(rate*steps/10) + testBurst; granted > max {
		t.Fatalf("granted %d over rate*dt+burst = %d: every engage minted a burst", granted, max)
	}
}

// primaryFollowOffset is 0 whenever no primary handle has read yet: no primary registered
// (a handle handoff, a watchdog start) or a primary whose lastOff is still -1, which the
// function floors at 0. A pump can therefore start with playerOff <= 0 and the pacer has to
// anchor the lead at the pump's start offset.
func TestPrimaryFollowOffsetCanBeZeroAtPumpStart(t *testing.T) {
	newTestApp(t)
	const path = "/test/follow-zero.mkv"
	if off, found := primaryFollowOffsetFound(path); off != 0 || found {
		t.Fatalf("no handle: offset %d found %v", off, found)
	}
	h := readHandle(path, gb)
	h.isPrimaryHandle.Store(true)
	app.activeHandles.Store(h, struct{}{})
	t.Cleanup(func() { app.activeHandles.Delete(h) })
	if off, found := primaryFollowOffsetFound(path); off != 0 || !found {
		t.Fatalf("a primary that has not read (lastOff -1): offset %d found %v, want 0 true", off, found)
	}
}

// pacedChunks is how many chunks of one pump's lead lie beyond M, the ones pacing can meter.
func pacedChunks(budget, chunk, streams int64) int {
	limit := streamLeadLimit(budget, streams, chunk)
	margin := paceMargin(128*mb, limit, chunk)
	n := 0
	for lead := chunk; lead <= limit; lead += chunk {
		if lead > margin {
			n++
		}
	}
	return n
}

// The README's table: at the default 256 MiB budget and 16 MiB chunk pacing is a no-op from
// five streams up, while a 1 or 2 GiB budget leaves bulk to meter for many more.
func TestPaceHasBulkAtTheDocumentedBudgets(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		budget  int64
		streams [4]int64 // 2, 4, 8 and 16 streams
		want    [4]int
	}{
		{256 * mb, [4]int64{2, 4, 8, 16}, [4]int{3, 1, 0, 0}},
		{1024 * mb, [4]int64{2, 4, 8, 16}, [4]int{21, 7, 3, 1}},
		{2048 * mb, [4]int64{2, 4, 8, 16}, [4]int{53, 21, 7, 3}},
	} {
		for i, n := range c.streams {
			if got := pacedChunks(c.budget, 16*mb, n); got != c.want[i] {
				t.Errorf("budget %d MiB, %d streams: %d paced chunks, want %d", c.budget/mb, n, got, c.want[i])
			}
		}
	}
	if pacedChunks(256*mb, 16*mb, 3) == 0 {
		t.Error("three streams at the default budget have no paced chunk")
	}
}
