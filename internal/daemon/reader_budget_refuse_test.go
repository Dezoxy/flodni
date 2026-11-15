package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"syscall"
	"testing"
	"time"

	"flodni/internal/warmup"
)

// refuseEnv arms the gate on a fresh budget session: the switch on, one matched play
// webhook seen, short hold and poll times. The returned handle reads s.path.
func refuseEnv(t *testing.T, margin int64) (*TTFFSession, *MkvHandle) {
	t.Helper()
	s := budgetEnv(t, margin)
	prevR, prevH, prevP := refuseUnconfirmedReads, unconfirmedReadHold, unconfirmedHoldPoll
	refuseUnconfirmedReads = func() bool { return true }
	unconfirmedReadHold, unconfirmedHoldPoll = 300*time.Millisecond, 10*time.Millisecond
	app.playbackWebhooksMatched.Store(1)
	t.Cleanup(func() {
		refuseUnconfirmedReads, unconfirmedReadHold, unconfirmedHoldPoll = prevR, prevH, prevP
	})
	return s, &MkvHandle{path: s.path, size: s.size}
}

type refuseCounts struct{ refused, lifted, expired, afterRefusal int64 }

func refuseSnapshot() refuseCounts {
	return refuseCounts{
		app.unconfirmedRefusedReads.Load(), app.unconfirmedHolds[rbHoldLifted].Load(),
		app.unconfirmedHolds[rbHoldRefused].Load(), app.unconfirmedConfirmedAfterRefusal.Load(),
	}
}

func TestBudgetBlocksFetchPredicate(t *testing.T) {
	const mid = 200 << 20 // past both windows
	head := warmup.FileSize
	type setup func(t *testing.T, s *TTFFSession)
	over := func(_ *testing.T, s *TTFFSession) { s.unconfirmedSwarm.Store(10 << 20) }
	tests := []struct {
		name    string
		margin  int64
		setup   setup
		path    string
		off, n  int64
		want    bool
		disarm  string // "switch" or "armed"
		prepare func(t *testing.T, s *TTFFSession)
	}{
		{name: "over the margin", margin: 10 << 20, setup: over, off: mid, n: 4096, want: true},
		{name: "switch off", margin: 10 << 20, setup: over, off: mid, n: 4096, disarm: "switch"},
		{name: "not armed: no matched webhook", margin: 10 << 20, setup: over, off: mid, n: 4096, disarm: "armed"},
		{name: "no session", margin: 10 << 20, setup: over, path: "/m/other.mkv", off: mid, n: 4096},
		{name: "under the margin", margin: 10 << 20,
			setup: func(_ *testing.T, s *TTFFSession) { s.unconfirmedSwarm.Store(10<<20 - 1) }, off: mid, n: 4096},
		{name: "confirmed", margin: 10 << 20, setup: func(t *testing.T, s *TTFFSession) {
			over(t, s)
			confirmPlayback(s.path)
		}, off: mid, n: 4096},
		{name: "window only (head)", margin: 10 << 20, setup: over, off: 0, n: 1 << 20},
		{name: "window only (tail)", margin: 10 << 20, setup: over, off: 1<<30 - 4096, n: 4096},
		{name: "straddles the head end", margin: 10 << 20, setup: over, off: head - 4096, n: 8192, want: true},
		{name: "margin 0, nothing counted yet", margin: 0, off: mid, n: 4096, want: true},
		{name: "margin 0, window read", margin: 0, off: 0, n: 4096},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := refuseEnv(t, tt.margin)
			switch tt.disarm {
			case "switch":
				refuseUnconfirmedReads = func() bool { return false }
			case "armed":
				app.playbackWebhooksMatched.Store(0)
			}
			if tt.setup != nil {
				tt.setup(t, s)
			}
			path := tt.path
			if path == "" {
				path = s.path
			}
			if got := budgetBlocksFetch(path, tt.off, tt.n, s.size); got != tt.want {
				t.Fatalf("budgetBlocksFetch = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFreshMarginAfterConfirmThenStop(t *testing.T) {
	s, _ := refuseEnv(t, 10<<20)
	const mid = 200 << 20
	s.countUnconfirmed(srcFetchBlock, 12<<20, mid)
	if !budgetBlocksFetch(s.path, mid, 4096, s.size) {
		t.Fatal("not blocked after crossing")
	}
	confirmPlayback(s.path)
	noteConfirmedLate(s.path) // floor = the tally at confirmation
	if budgetBlocksFetch(s.path, mid, 4096, s.size) {
		t.Fatal("blocked while confirmed")
	}
	app.playbackRegistry.Delete(s.path) // media.stop
	if s.overUnconfirmedMargin() || budgetBlocksFetch(s.path, mid, 4096, s.size) {
		t.Fatal("the pre-confirmation tally still counts after a stop")
	}
	s.countUnconfirmed(srcFetchBlock, 6<<20, mid)
	if budgetBlocksFetch(s.path, mid, 4096, s.size) {
		t.Fatal("blocked at 6 MiB since the stop, margin 10 MiB")
	}
	s.countUnconfirmed(srcFetchBlock, 5<<20, mid)
	if !budgetBlocksFetch(s.path, mid, 4096, s.size) {
		t.Fatal("not blocked at 11 MiB since the stop")
	}
}

func TestFloorIsSetEvenWhenTheSessionNeverCrossed(t *testing.T) {
	s, _ := refuseEnv(t, 10<<20)
	s.countUnconfirmed(srcFetchBlock, 4<<20, 200<<20)
	confirmPlayback(s.path)
	noteConfirmedLate(s.path) // returns early (never crossed) after storing the floor
	if s.budgetFloor.Load() != 4<<20 {
		t.Fatalf("budgetFloor = %d, want 4 MiB", s.budgetFloor.Load())
	}
}

func TestGateIsANoOpWhenNothingBlocks(t *testing.T) {
	_, h := refuseEnv(t, 10<<20)
	start := time.Now()
	if errno := h.budgetGate(context.Background(), 200<<20, 4096); errno != 0 {
		t.Fatalf("errno = %v", errno)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("an unblocked read waited")
	}
}

func TestGateHoldLiftsOnConfirmation(t *testing.T) {
	s, h := refuseEnv(t, 0)
	unconfirmedReadHold = 5 * time.Second
	c0 := refuseSnapshot()
	go func() {
		time.Sleep(60 * time.Millisecond)
		confirmPlayback(s.path)
	}()
	start := time.Now()
	if errno := h.budgetGate(context.Background(), 200<<20, 4096); errno != 0 {
		t.Fatalf("errno = %v, want 0 after the confirmation", errno)
	}
	if d := time.Since(start); d < 50*time.Millisecond || d > time.Second {
		t.Fatalf("lifted after %v", d)
	}
	c := refuseSnapshot()
	if c.lifted-c0.lifted != 1 || c.expired != c0.expired || c.refused != c0.refused || s.refusedReads.Load() != 0 {
		t.Fatalf("counters %+v from %+v, refusedReads %d", c, c0, s.refusedReads.Load())
	}
}

func TestGateHoldExpiresThenRefusesAtOnceAndLogsOnce(t *testing.T) {
	s, h := refuseEnv(t, 0)
	buf := captureSessionLog(t)
	noteReaderCaller(s.path, 0, 1234)
	s.lastReadAt.Store(time.Now().Add(-time.Hour).UnixNano())
	c0 := refuseSnapshot()

	start := time.Now()
	if errno := h.budgetGate(context.Background(), 200<<20, 4096); errno != errnoBudgetRefused {
		t.Fatalf("errno = %v, want the refusal sentinel", errno)
	}
	if d := time.Since(start); d < unconfirmedReadHold || d > 5*unconfirmedReadHold {
		t.Fatalf("expired after %v, hold %v", d, unconfirmedReadHold)
	}
	if time.Since(time.Unix(0, s.lastReadAt.Load())) > time.Second {
		t.Fatal("a refusal did not refresh lastReadAt: the session would idle-close")
	}

	start = time.Now()
	if errno := h.budgetGate(context.Background(), 300<<20, 4096); errno != errnoBudgetRefused {
		t.Fatalf("second read errno = %v", errno)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("the second read on an expired hold waited")
	}

	c := refuseSnapshot()
	if c.refused-c0.refused != 2 || c.expired-c0.expired != 1 || c.lifted != c0.lifted || s.refusedReads.Load() != 2 {
		t.Fatalf("counters %+v from %+v, refusedReads %d", c, c0, s.refusedReads.Load())
	}
	lines := linesWith(buf, rbPrefix)
	if len(lines) != 1 {
		t.Fatalf("%d lines, want one refused line per session: %v", len(lines), lines)
	}
	if strings.Contains(lines[0], "x.mkv") || strings.Contains(lines[0], "/m/") {
		t.Fatalf("line carries the path: %s", lines[0])
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[0], rbPrefix)), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["event"] != "refused" || rec["enforced"] != true || rec["offset"] != float64(200<<20) ||
		rec["uid"] != float64(1234) {
		t.Fatalf("unexpected record: %s", lines[0])
	}
	if held, ok := rec["held_ms"].(float64); !ok || held < float64(unconfirmedReadHold.Milliseconds())-20 {
		t.Fatalf("held_ms = %v: %s", rec["held_ms"], lines[0])
	}
	for _, k := range []string{"hash", "file_index", "size", "swarm_bytes", "cache_bytes", "margin", "open_age_ms", "comm", "pid"} {
		if _, ok := rec[k]; !ok {
			t.Errorf("key %q missing: %s", k, lines[0])
		}
	}
}

func TestGateReturnsEINTRWhenTheCallerLeaves(t *testing.T) {
	s, h := refuseEnv(t, 0)
	unconfirmedReadHold = 5 * time.Second
	c0 := refuseSnapshot()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()
	if errno := h.budgetGate(ctx, 200<<20, 4096); errno != syscall.EINTR {
		t.Fatalf("errno = %v, want EINTR", errno)
	}
	if c := refuseSnapshot(); c != c0 || s.refusedReads.Load() != 0 {
		t.Fatalf("an interrupted read counted: %+v from %+v", c, c0)
	}
}

func TestNewHandleGetsItsOwnHold(t *testing.T) {
	s, h := refuseEnv(t, 0)
	if errno := h.budgetGate(context.Background(), 200<<20, 4096); errno != errnoBudgetRefused {
		t.Fatalf("errno = %v", errno)
	}
	h2 := &MkvHandle{path: s.path, size: s.size}
	start := time.Now()
	if errno := h2.budgetGate(context.Background(), 200<<20, 4096); errno != errnoBudgetRefused {
		t.Fatalf("errno = %v", errno)
	}
	if time.Since(start) < unconfirmedReadHold {
		t.Fatal("a new handle was refused at once instead of getting its own hold")
	}
}

func TestHoldRearmsAfterAConfirmation(t *testing.T) {
	s, h := refuseEnv(t, 0)
	if errno := h.budgetGate(context.Background(), 200<<20, 4096); errno != errnoBudgetRefused {
		t.Fatalf("errno = %v", errno)
	}
	confirmPlayback(s.path)
	if errno := h.budgetGate(context.Background(), 200<<20, 4096); errno != 0 {
		t.Fatalf("confirmed read errno = %v", errno)
	}
	app.playbackRegistry.Delete(s.path) // stop, with no floor: blocked again at margin 0
	start := time.Now()
	if errno := h.budgetGate(context.Background(), 200<<20, 4096); errno != errnoBudgetRefused {
		t.Fatalf("errno = %v", errno)
	}
	if time.Since(start) < unconfirmedReadHold {
		t.Fatal("the hold did not re-arm after the confirmation")
	}
}

func TestSessionClosedMidHoldDoesNotLift(t *testing.T) {
	s, h := refuseEnv(t, 0)
	unconfirmedReadHold = 5 * time.Second
	c0 := refuseSnapshot()
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		time.Sleep(40 * time.Millisecond)
		s.closeSession() // leaves the sessions map: a lookup now finds nothing
	}()
	defer func() { <-closed }() // closeSession logs; do not let it outlive the test
	start := time.Now()
	if errno := h.budgetGate(context.Background(), 200<<20, 4096); errno != errnoBudgetRefused {
		t.Fatalf("errno = %v, want a refusal: the session is gone, not the budget satisfied", errno)
	}
	if time.Since(start) > time.Second {
		t.Fatal("the hold outlived its closed session")
	}
	if c := refuseSnapshot(); c.lifted != c0.lifted {
		t.Fatal("a hold whose session closed counted as lifted")
	}
}

func TestConfirmationAfterARefusalCountsOncePerSession(t *testing.T) {
	s, h := refuseEnv(t, 0)
	buf := captureSessionLog(t)
	s.countUnconfirmed(srcFetchBlock, 1<<20, 200<<20) // margin 0: crosses at the first byte
	c0 := refuseSnapshot()
	if errno := h.budgetGate(context.Background(), 200<<20, 4096); errno != errnoBudgetRefused {
		t.Fatalf("errno = %v", errno)
	}
	confirmPlayback(s.path)
	noteConfirmedLate(s.path)
	noteConfirmedLate(s.path)
	if c := refuseSnapshot(); c.afterRefusal-c0.afterRefusal != 1 {
		t.Fatalf("confirmed_after_refusal +%d, want 1", c.afterRefusal-c0.afterRefusal)
	}
	var late map[string]any
	for _, l := range linesWith(buf, rbPrefix) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(l, rbPrefix)), &rec); err != nil {
			t.Fatal(err)
		}
		if rec["event"] == "confirmed_late" {
			late = rec
		}
	}
	if late == nil || late["refused_reads"] != float64(1) {
		t.Fatalf("confirmed_late line missing refused_reads: %v", late)
	}
}

func TestConfirmationWithoutARefusalDoesNotCount(t *testing.T) {
	s, _ := refuseEnv(t, 10<<20)
	c0 := refuseSnapshot()
	confirmPlayback(s.path)
	noteConfirmedLate(s.path)
	if c := refuseSnapshot(); c.afterRefusal != c0.afterRefusal {
		t.Fatal("a session that was never refused counted as confirmed after refusal")
	}
}

func TestOverMarginReportsEnforcedTruthfully(t *testing.T) {
	for _, tt := range []struct {
		name     string
		on       bool
		armed    bool
		wantEnfd bool
	}{
		{"on and armed", true, true, true},
		{"on, not armed", true, false, false},
		{"off", false, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := refuseEnv(t, 1<<20)
			refuseUnconfirmedReads = func() bool { return tt.on }
			if !tt.armed {
				app.playbackWebhooksMatched.Store(0)
			}
			buf := captureSessionLog(t)
			s.countUnconfirmed(srcFetchBlock, 2<<20, 200<<20)
			lines := linesWith(buf, rbPrefix)
			if len(lines) != 1 {
				t.Fatalf("%d lines", len(lines))
			}
			var rec map[string]any
			_ = json.Unmarshal([]byte(strings.TrimPrefix(lines[0], rbPrefix)), &rec)
			if rec["enforced"] != tt.wantEnfd {
				t.Fatalf("enforced = %v, want %v: %s", rec["enforced"], tt.wantEnfd, lines[0])
			}
		})
	}
}

func TestErrnoBudgetRefusedIsAValidErrno(t *testing.T) {
	// Read must map it to EIO; a leak would still be a real errno.
	if errnoBudgetRefused != syscall.EDQUOT || errnoBudgetRefused == syscall.EIO {
		t.Fatalf("errnoBudgetRefused = %v", errnoBudgetRefused)
	}
}

func TestWindowReadNeitherStartsNorRearmsAHold(t *testing.T) {
	_, h := refuseEnv(t, 0)
	if errno := h.budgetGate(context.Background(), 0, 4096); errno != 0 {
		t.Fatalf("window read errno = %v", errno)
	}
	if errno := h.budgetGate(context.Background(), 200<<20, 4096); errno != errnoBudgetRefused {
		t.Fatalf("errno = %v", errno)
	}
	if errno := h.budgetGate(context.Background(), 0, 4096); errno != 0 {
		t.Fatalf("window read after the refusal errno = %v", errno)
	}
	start := time.Now()
	if errno := h.budgetGate(context.Background(), 300<<20, 4096); errno != errnoBudgetRefused {
		t.Fatalf("errno = %v", errno)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("a window read in between re-armed the hold")
	}
}

// Guard, not a regression test: the gate shares no lock with the read path, so a hold on
// one handle does not delay another handle of the same file. It passes without the
// open-flags fix. The end-to-end stall was the kernel's open-time invalidation, which
// only a real kernel (make e2e) can pin.
func TestReadOnAnotherHandleIsNotBlockedWhileOneSleepsInTheGate(t *testing.T) {
	useFreshReadPath(t)
	s, _ := refuseEnv(t, 0) // margin 0: any miss past the windows is held
	unconfirmedReadHold = 5 * time.Second
	chunk := pattern(int(16 * mb))
	raCache.Put(s.path, 0, int64(len(chunk))-1, chunk)

	a := readHandle(s.path, s.size)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = a.Read(ctx, make([]byte, 256*1024), 200*mb)
	}()
	t.Cleanup(func() { cancel(); <-done })

	for limit := time.Now().Add(2 * time.Second); !a.budgetHold.held.Load(); {
		if time.Now().After(limit) {
			t.Fatal("handle A never reached the gate")
		}
		time.Sleep(time.Millisecond)
	}

	b := readHandle(s.path, s.size)
	start := time.Now()
	res, errno := b.Read(context.Background(), make([]byte, 64*1024), 64*1024)
	if errno != 0 || res == nil || res.Size() != 64*1024 {
		t.Fatalf("window read on handle B: res=%v errno=%v", res, errno)
	}
	if errno := b.budgetGate(context.Background(), 0, 4096); errno != 0 {
		t.Fatalf("window gate on handle B: errno = %v", errno)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("handle B waited %v behind handle A's hold", d)
	}
	select {
	case <-done:
		t.Fatal("handle A left the gate early")
	default:
	}
}
