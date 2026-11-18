package torrent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// Hard rule 3: a private torrent announces only to the trackers it carries, never to the DHT,
// and never to one tracker twice within its interval or twice at once. These tests drive the
// regular loop (Run) and the forced announce (AnnounceWhenDue) against a fake tracker in real
// time: the tracker sends interval 4 s and min interval 1 s, the client floor is 100 ms.

const (
	fakeInterval    = 4 * time.Second
	fakeMinInterval = 1 * time.Second
)

type fakeHit struct{ start, end time.Time }

// fakeTracker answers every announce with a bencoded response and records when each began and
// ended. delay holds a request open; fail makes it answer with a failure reason.
type fakeTracker struct {
	srv *httptest.Server
	mu  sync.Mutex
	// interval and minInterval are what the response says; minInterval < 0 omits the key.
	interval, minInterval time.Duration
	delay                 time.Duration
	fail                  bool
	hits                  []fakeHit
}

func newFakeTracker(t *testing.T) *fakeTracker {
	ft := &fakeTracker{interval: fakeInterval, minInterval: fakeMinInterval}
	ft.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		start := time.Now()
		ft.mu.Lock()
		delay, fail := ft.delay, ft.fail
		body := fmt.Sprintf("d8:completei1e10:incompletei1e8:intervali%de", int(ft.interval/time.Second))
		if ft.minInterval >= 0 {
			body += fmt.Sprintf("12:min intervali%de", int(ft.minInterval/time.Second))
		}
		body += "5:peers0:e"
		if fail {
			body = "d14:failure reason4:nopee"
		}
		ft.mu.Unlock()
		time.Sleep(delay)
		w.Write([]byte(body))
		ft.mu.Lock()
		ft.hits = append(ft.hits, fakeHit{start, time.Now()})
		ft.mu.Unlock()
	}))
	t.Cleanup(ft.srv.Close)
	return ft
}

func (ft *fakeTracker) url() string { return ft.srv.URL + "/announce" }

func (ft *fakeTracker) snapshot() []fakeHit {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	hits := append([]fakeHit(nil), ft.hits...)
	sort.Slice(hits, func(i, j int) bool { return hits[i].start.Before(hits[j].start) })
	return hits
}

func (ft *fakeTracker) set(f func(*fakeTracker)) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	f(ft)
}

func newPrivateTorrent(t *testing.T, tr *fakeTracker) (*Client, *Torrent, *fakeDht) {
	t.Helper()
	cfg := NewDefaultClientConfig()
	cfg.DataDir = t.TempDir()
	cfg.ListenHost = func(string) string { return "127.0.0.1" }
	cfg.ListenPort = 0
	cfg.NoDefaultPortForwarding = true
	cfg.DisableIPv6 = true
	cfg.NoDHT = true
	cfg.PeriodicallyAnnounceTorrentsToDht = true
	cfg.MinAnnounceInterval = 100 * time.Millisecond
	cfg.FailedAnnounceMinInterval = 300 * time.Millisecond
	cfg.FailedAnnounceMaxInterval = 5 * time.Second
	cl, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	fd := &fakeDht{}
	cl.AddDhtServer(fd)

	yes := true
	info := metainfo.Info{
		Name:        "private-forced",
		Length:      16384,
		PieceLength: 16384,
		Pieces:      make([]byte, 20),
		Private:     &yes,
	}
	b, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	spec := &TorrentSpec{
		InfoHash:  metainfo.HashBytes(b),
		InfoBytes: b,
		Trackers:  [][]string{{tr.url()}},
	}
	tor, _, err := cl.AddTorrentSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	return cl, tor, fd
}

func scraperOf(t *testing.T, tor *Torrent) *trackerScraper {
	t.Helper()
	tor.cl.rLock()
	defer tor.cl.rUnlock()
	for _, ta := range tor.trackerAnnouncers {
		if ts, ok := ta.(*trackerScraper); ok {
			return ts
		}
	}
	t.Fatal("no tracker scraper")
	return nil
}

func waitFor(t *testing.T, what string, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Run and a forced announce, polled every 20 ms for three intervals against a tracker that
// holds each request 200 ms: never two announces at once, never two within the min interval,
// never a regular announce within the interval of the forced one before it, and the forced one
// does happen while the regular one is still far away.
func TestForcedAnnounceNeverDoublesTheRegularOne(t *testing.T) {
	tr := newFakeTracker(t)
	tr.set(func(ft *fakeTracker) { ft.delay = 200 * time.Millisecond })
	_, tor, _ := newPrivateTorrent(t, tr)

	type call struct {
		start, end time.Time
		sent       int
	}
	var calls []call
	stop := time.Now().Add(3*fakeInterval + 2*time.Second)
	for time.Now().Before(stop) {
		c := call{start: time.Now()}
		c.sent = tor.AnnounceWhenDue()
		c.end = time.Now()
		calls = append(calls, c)
		time.Sleep(20 * time.Millisecond)
	}
	hits := tr.snapshot()
	forcedHit := func(h fakeHit) bool {
		for _, c := range calls {
			if c.sent > 0 && !h.start.Before(c.start) && !h.start.After(c.end) {
				return true
			}
		}
		return false
	}
	if len(hits) < 4 {
		t.Fatalf("only %d announces in %v", len(hits), 3*fakeInterval)
	}
	var forced int
	for i, h := range hits {
		if i == 0 {
			continue
		}
		p := hits[i-1]
		if h.start.Before(p.end) {
			t.Errorf("announce %d began at +%v while announce %d was in flight until +%v",
				i, h.start.Sub(hits[0].start), i-1, p.end.Sub(hits[0].start))
		}
		if gap := h.start.Sub(p.start); gap < fakeMinInterval {
			t.Errorf("announce %d came %v after announce %d, inside the min interval %v", i, gap, i-1, fakeMinInterval)
		}
		if forcedHit(h) {
			forced++
			if gap := h.start.Sub(p.end); gap < fakeMinInterval {
				t.Errorf("forced announce %d only %v after the previous one finished", i, gap)
			}
		} else if gap := h.start.Sub(p.end); gap < fakeInterval {
			t.Errorf("regular announce %d came %v after announce %d (forced=%v) finished, inside the interval %v",
				i, gap, i-1, forcedHit(p), fakeInterval)
		}
	}
	if forced == 0 {
		t.Error("no forced announce happened although the regular one was far away")
	}
	if forced > 3 {
		t.Errorf("%d forced announces in three intervals, want at most one per regular announce", forced)
	}
	for i := 1; i < len(hits); i++ {
		if forcedHit(hits[i]) && i > 1 && forcedHit(hits[i-1]) {
			t.Errorf("announces %d and %d are both forced, with no regular one between", i-1, i)
		}
	}
	if sendsEarly := hits[1].start.Sub(hits[0].end); sendsEarly >= fakeInterval {
		t.Errorf("the second announce came %v after the first: nothing starved was re-announced early", sendsEarly)
	}
}

// A tracker that sends no min interval, or one not smaller than its interval, is never forced.
// A UDP tracker's response carries none, so it takes the first branch.
func TestForcedAnnounceNeedsAMinIntervalBelowTheInterval(t *testing.T) {
	for name, set := range map[string]func(*fakeTracker){
		"none sent":         func(ft *fakeTracker) { ft.minInterval = -1 },
		"equal to interval": func(ft *fakeTracker) { ft.minInterval = ft.interval },
		"above interval":    func(ft *fakeTracker) { ft.minInterval = 2 * ft.interval },
	} {
		t.Run(name, func(t *testing.T) {
			tr := newFakeTracker(t)
			tr.set(set)
			_, tor, _ := newPrivateTorrent(t, tr)
			waitFor(t, "the first announce", 3*time.Second, func() bool { return len(tr.snapshot()) == 1 })
			end := time.Now().Add(fakeMinInterval + 1500*time.Millisecond)
			for time.Now().Before(end) {
				if n := tor.AnnounceWhenDue(); n != 0 {
					t.Fatalf("AnnounceWhenDue sent %d announces to a tracker with no usable min interval", n)
				}
				time.Sleep(25 * time.Millisecond)
			}
			if n := len(tr.snapshot()); n != 1 {
				t.Errorf("%d announces, want just the first", n)
			}
		})
	}
}

// Before the first announce has finished, and while the last one failed, nothing is forced.
func TestForcedAnnounceWaitsForASuccessfulRegularOne(t *testing.T) {
	tr := newFakeTracker(t)
	tr.set(func(ft *fakeTracker) { ft.delay = 500 * time.Millisecond })
	_, tor, _ := newPrivateTorrent(t, tr)
	for end := time.Now().Add(400 * time.Millisecond); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if n := tor.AnnounceWhenDue(); n != 0 {
			t.Fatalf("a forced announce went out before the first announce finished")
		}
	}
}

// A failed forced announce keeps Run's backoff level: it neither resets the failures Run has
// counted nor records a wait of zero, and nothing is forced again until Run has announced.
func TestFailedForcedAnnounceKeepsTheBackoff(t *testing.T) {
	tr := newFakeTracker(t)
	_, tor, _ := newPrivateTorrent(t, tr)
	ts := scraperOf(t, tor)
	waitFor(t, "the first announce", 3*time.Second, func() bool { return len(tr.snapshot()) == 1 })

	// Pretend Run had failed twice before this success: level 2 is the backoff to keep.
	ts.ann.mu.Lock()
	ts.ann.failures = 2
	ts.ann.mu.Unlock()
	ts.consecutiveFailures = 2 // only Run writes it; Run is asleep for 4 s
	tr.set(func(ft *fakeTracker) { ft.fail = true })
	time.Sleep(fakeMinInterval + 100*time.Millisecond)

	if n := tor.AnnounceWhenDue(); n != 1 {
		t.Fatalf("AnnounceWhenDue sent %d, want the one forced announce", n)
	}
	ts.ann.mu.Lock()
	wait, ok, failures := ts.ann.wait, ts.ann.ok, ts.ann.failures
	ts.ann.mu.Unlock()
	want := tor.cl.config.failedAnnounceInterval(2)
	if failures != 2 || ts.consecutiveFailures != 2 {
		t.Errorf("a failed forced announce changed Run's failure count: state %d, scraper %d, want 2", failures, ts.consecutiveFailures)
	}
	if wait < want {
		t.Errorf("a failed forced announce left a wait of %v, below Run's backoff level of %v", wait, want)
	}
	if ok {
		t.Error("a failed forced announce was recorded as a success")
	}
	if n := tor.AnnounceWhenDue(); n != 0 {
		t.Errorf("a second forced announce went out (%d) right after a failed one", n)
	}
}

// The announce returns whether it was sent, and a deferred one costs nothing.
func TestForcedAnnounceCountsOnlyWhatWasSent(t *testing.T) {
	tr := newFakeTracker(t)
	_, tor, _ := newPrivateTorrent(t, tr)
	waitFor(t, "the first announce", 3*time.Second, func() bool { return len(tr.snapshot()) == 1 })
	if n := tor.AnnounceWhenDue(); n != 0 {
		t.Fatalf("sent %d inside the min interval, want 0", n)
	}
	time.Sleep(fakeMinInterval + 50*time.Millisecond)
	if n := tor.AnnounceWhenDue(); n != 1 {
		t.Fatalf("sent %d after the min interval, want 1", n)
	}
	if n := tor.AnnounceWhenDue(); n != 0 {
		t.Fatalf("sent %d again right after, want 0", n)
	}
	if got := len(tr.snapshot()); got != 2 {
		t.Errorf("tracker saw %d announces, want 2", got)
	}
}

// A private torrent reaches its own trackers only: not the DHT, and not a second tracker that
// is reachable but is not in the torrent. The control adds that tracker the way a retracker
// would, to show the assertion could have failed.
func TestPrivateForcedAnnounceReachesOnlyItsOwnTracker(t *testing.T) {
	own, other := newFakeTracker(t), newFakeTracker(t)
	_, tor, fd := newPrivateTorrent(t, own)
	waitFor(t, "the first announce", 3*time.Second, func() bool { return len(own.snapshot()) == 1 })
	time.Sleep(fakeMinInterval + 50*time.Millisecond)
	if n := tor.AnnounceWhenDue(); n != 1 {
		t.Fatalf("forced announce sent %d, want 1", n)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(other.snapshot()); n != 0 {
		t.Errorf("an unrelated tracker was announced to %d times", n)
	}
	if n := fd.announces.Load(); n != 0 {
		t.Errorf("the DHT was announced to %d times", n)
	}
	if n := len(own.snapshot()); n != 2 {
		t.Errorf("own tracker saw %d announces, want the regular and the forced one", n)
	}

	// Control: the other tracker is reachable and counted when a torrent does list it.
	tor.cl.lock()
	tor.addTrackers([][]string{{other.url()}})
	tor.cl.unlock()
	waitFor(t, "the control tracker to be announced to", 3*time.Second, func() bool {
		return len(other.snapshot()) > 0 && strings.HasPrefix(other.url(), "http://")
	})
	if n := fd.announces.Load(); n != 0 {
		t.Errorf("the DHT was announced to %d times after the control", n)
	}
}

// A forced announce still in flight when the regular one falls due holds it back: the two never
// overlap, and the regular one then waits its interval from the forced one's end. The tracker
// holds the forced request 1.5 s, so it straddles Run's due time (interval 3 s, min interval 2 s).
func TestForcedAnnounceStraddlingTheRegularDueTime(t *testing.T) {
	tr := newFakeTracker(t)
	tr.set(func(ft *fakeTracker) { ft.interval, ft.minInterval = 3*time.Second, 2*time.Second })
	_, tor, _ := newPrivateTorrent(t, tr)
	waitFor(t, "the first announce", 3*time.Second, func() bool { return len(tr.snapshot()) == 1 })
	tr.set(func(ft *fakeTracker) { ft.delay = 1500 * time.Millisecond })
	for end := time.Now().Add(9 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		tor.AnnounceWhenDue()
	}
	hits := tr.snapshot()
	if len(hits) < 3 {
		t.Fatalf("%d announces, want at least 3", len(hits))
	}
	for i := 1; i < len(hits); i++ {
		if hits[i].start.Before(hits[i-1].end) {
			t.Errorf("announce %d began while announce %d was in flight", i, i-1)
		}
	}
	// Announce 1 is the forced one (the first one after the min interval); announce 2 is Run's.
	if gap := hits[2].start.Sub(hits[1].end); gap < 3*time.Second {
		t.Errorf("the regular announce came %v after the forced one finished, inside the interval", gap)
	}
}

// What makes a forced announce ineligible, set directly on the shared record so each guard is
// tried alone: every case but the last would be eligible if its one condition were lifted.
func TestForcedAnnounceGuardsOneByOne(t *testing.T) {
	tr := newFakeTracker(t)
	tr.set(func(ft *fakeTracker) { ft.interval = 60 * time.Second })
	_, tor, _ := newPrivateTorrent(t, tr)
	ts := scraperOf(t, tor)
	waitFor(t, "the first announce", 3*time.Second, func() bool { return len(tr.snapshot()) == 1 })
	waitFor(t, "the first announce to be recorded", 3*time.Second, func() bool {
		ts.ann.mu.Lock()
		defer ts.ann.mu.Unlock()
		return !ts.ann.inFlight
	})
	set := func(f func(a *announceState)) {
		ts.ann.mu.Lock()
		defer ts.ann.mu.Unlock()
		a := &ts.ann
		a.lastDone = time.Now().Add(-3 * time.Second) // 3 s ago
		a.wait, a.interval, a.minInterval = 10*time.Second, 10*time.Second, 2*time.Second
		a.ok, a.forcedSinceRun, a.inFlight = true, false, false
		a.done = make(chan struct{})
		f(a)
	}
	for name, f := range map[string]func(*announceState){
		"Run due":              func(a *announceState) { a.wait = 2 * time.Second },
		"last announce failed": func(a *announceState) { a.ok = false },
		"min interval = interval": func(a *announceState) {
			a.minInterval, a.wait, a.lastDone = 10*time.Second, 20*time.Second, time.Now().Add(-11*time.Second)
		},
		"min interval not sent":   func(a *announceState) { a.minInterval = 0 },
		"forced already sent":     func(a *announceState) { a.forcedSinceRun = true },
		"min interval not passed": func(a *announceState) { a.lastDone = time.Now().Add(-time.Second) },
		"in flight":               func(a *announceState) { a.inFlight = true },
	} {
		set(f)
		if ts.announceWhenDue(tor.closedCtx) {
			t.Errorf("%s: a forced announce went out", name)
		}
	}
	set(func(*announceState) {})
	if !ts.announceWhenDue(tor.closedCtx) {
		t.Error("control: an eligible state did not announce")
	}
}

// AnnounceTracked ignores the reservation and the tracker's interval, so it must never reach a
// private torrent's tracker: only AnnounceWhenDue may announce one early.
func TestAnnounceTrackedNeverAnnouncesAPrivateTorrent(t *testing.T) {
	own := newFakeTracker(t)
	_, tor, _ := newPrivateTorrent(t, own)
	waitFor(t, "the first announce", 3*time.Second, func() bool { return len(own.snapshot()) == 1 })
	tor.AnnounceTracked(nil)
	time.Sleep(300 * time.Millisecond)
	if n := len(own.snapshot()); n != 1 {
		t.Errorf("AnnounceTracked reached a private torrent's tracker: %d announces, want only the regular one", n)
	}
}
