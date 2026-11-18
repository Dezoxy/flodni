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
	return newTorrentWith(t, tr, true, 100*time.Millisecond)
}

// newTorrentWith adds a one-piece torrent listing tr, private or public, to a client whose
// announce floor is floor.
func newTorrentWith(t *testing.T, tr *fakeTracker, private bool, floor time.Duration) (*Client, *Torrent, *fakeDht) {
	t.Helper()
	cfg := NewDefaultClientConfig()
	cfg.DataDir = t.TempDir()
	cfg.ListenHost = func(string) string { return "127.0.0.1" }
	cfg.ListenPort = 0
	cfg.NoDefaultPortForwarding = true
	cfg.DisableIPv6 = true
	cfg.NoDHT = true
	cfg.PeriodicallyAnnounceTorrentsToDht = true
	cfg.MinAnnounceInterval = floor
	cfg.FailedAnnounceMinInterval = 300 * time.Millisecond
	cfg.FailedAnnounceMaxInterval = 5 * time.Second
	cl, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	fd := &fakeDht{}
	cl.AddDhtServer(fd)

	info := metainfo.Info{
		Name:        "private-forced",
		Length:      16384,
		PieceLength: 16384,
		Pieces:      make([]byte, 20),
	}
	if private {
		yes := true
		info.Private = &yes
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
	if !private {
		tor.DownloadAll() // a torrent with nothing to download never wants peers
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

// settled waits until the tracker has seen n announces and Run or a forced announce has
// recorded the last one: the tracker's hit is logged before the client has processed the
// response, so the hit alone says nothing about the engine's state.
func settled(t *testing.T, tr *fakeTracker, ts *trackerScraper, n int) {
	t.Helper()
	waitFor(t, fmt.Sprintf("announce %d to be recorded", n), 8*time.Second, func() bool {
		if len(tr.snapshot()) != n {
			return false
		}
		ts.ann.mu.Lock()
		defer ts.ann.mu.Unlock()
		return !ts.ann.inFlight
	})
}

// sleepSinceLastAnnounce sleeps until d has passed since the last recorded announce finished,
// the instant the engine measures every wait from.
func sleepSinceLastAnnounce(ts *trackerScraper, d time.Duration) {
	ts.ann.mu.Lock()
	until := ts.ann.lastDone.Add(d)
	ts.ann.mu.Unlock()
	time.Sleep(time.Until(until))
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
			settled(t, tr, scraperOf(t, tor), 1)
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

// A failed forced announce moves Run's next announce to the later of its previous due time and
// one min interval after the failure: never a full interval from the failure, never sooner than
// the min interval. Case "Run far off": the previous due time wins. Case "Run close": the due
// time is inside the min interval after the failure, so the min interval wins.
func TestFailedForcedAnnounceDueTime(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		interval, minInterval time.Duration
		keepsPreviousDue      bool
	}{
		{"Run far off", 4 * time.Second, 1 * time.Second, true},
		{"Run close", 3 * time.Second, 2 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newFakeTracker(t)
			tr.set(func(ft *fakeTracker) { ft.interval, ft.minInterval = tc.interval, tc.minInterval })
			_, tor, _ := newPrivateTorrent(t, tr)
			ts := scraperOf(t, tor)
			settled(t, tr, ts, 1)
			ts.ann.mu.Lock()
			prevDue := ts.ann.lastDone.Add(ts.ann.wait)
			ts.ann.mu.Unlock()

			tr.set(func(ft *fakeTracker) { ft.fail = true })
			sleepSinceLastAnnounce(ts, tc.minInterval+50*time.Millisecond)
			if n := tor.AnnounceWhenDue(); n != 1 {
				t.Fatalf("AnnounceWhenDue sent %d, want the one forced announce", n)
			}
			ts.ann.mu.Lock()
			failedAt, wait, ok := ts.ann.lastDone, ts.ann.wait, ts.ann.ok
			ts.ann.mu.Unlock()
			if ok {
				t.Error("a failed forced announce was recorded as a success")
			}
			due := failedAt.Add(wait)
			want := failedAt.Add(tc.minInterval)
			if tc.keepsPreviousDue {
				want = prevDue
			}
			if d := due.Sub(want); d < -time.Microsecond || d > time.Microsecond {
				t.Errorf("Run is next due %v after the failure, want %v (off by %v)",
					due.Sub(failedAt), want.Sub(failedAt), d)
			}
			if n := tor.AnnounceWhenDue(); n != 0 {
				t.Errorf("a second forced announce went out (%d) right after a failed one", n)
			}
		})
	}
}

// The announce returns whether it was sent, and a deferred one costs nothing.
func TestForcedAnnounceCountsOnlyWhatWasSent(t *testing.T) {
	tr := newFakeTracker(t)
	_, tor, _ := newPrivateTorrent(t, tr)
	ts := scraperOf(t, tor)
	settled(t, tr, ts, 1)
	if n := tor.AnnounceWhenDue(); n != 0 {
		t.Fatalf("sent %d inside the min interval, want 0", n)
	}
	sleepSinceLastAnnounce(ts, fakeMinInterval+50*time.Millisecond)
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
	ts := scraperOf(t, tor)
	settled(t, own, ts, 1)
	sleepSinceLastAnnounce(ts, fakeMinInterval+50*time.Millisecond)
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
	settled(t, tr, scraperOf(t, tor), 1)
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
	settled(t, tr, ts, 1)
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
	settled(t, own, scraperOf(t, tor), 1)
	tor.AnnounceTracked(nil)
	time.Sleep(300 * time.Millisecond)
	if n := len(own.snapshot()); n != 1 {
		t.Errorf("AnnounceTracked reached a private torrent's tracker: %d announces, want only the regular one", n)
	}
}

// Run's own announce re-arms the forced one: with interval 2 s and min interval 1 s the tracker
// sees forced, regular, forced, regular-or-later in turn, so a second forced announce goes out
// after the first regular one that followed the first forced one.
func TestForcedAnnounceIsRearmedByTheNextRegularOne(t *testing.T) {
	tr := newFakeTracker(t)
	tr.set(func(ft *fakeTracker) { ft.interval, ft.minInterval = 2*time.Second, 1*time.Second })
	_, tor, _ := newPrivateTorrent(t, tr)
	var forced int
	for end := time.Now().Add(4600 * time.Millisecond); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		forced += tor.AnnounceWhenDue()
	}
	// Timeline: regular at 0, forced at 1, regular at 3, forced at 4.
	if forced != 2 {
		t.Errorf("%d forced announces in 4.6 s, want 2 (one before and one after the regular announce at 3 s)", forced)
	}
	if n := len(tr.snapshot()); n != 4 {
		t.Errorf("tracker saw %d announces, want 4", n)
	}
}

// The client's announce floor bounds the forced gap from below when the tracker's min interval
// is smaller: min interval 1 s, floor 2 s, interval 6 s.
func TestForcedAnnounceHonoursTheClientFloor(t *testing.T) {
	tr := newFakeTracker(t)
	tr.set(func(ft *fakeTracker) { ft.interval, ft.minInterval = 6*time.Second, 1*time.Second })
	_, tor, _ := newTorrentWith(t, tr, true, 2*time.Second)
	ts := scraperOf(t, tor)
	settled(t, tr, ts, 1)
	sleepSinceLastAnnounce(ts, 1200*time.Millisecond)
	if n := tor.AnnounceWhenDue(); n != 0 {
		t.Fatalf("sent %d announces 1.2 s after the last, inside the 2 s client floor", n)
	}
	sleepSinceLastAnnounce(ts, 2100*time.Millisecond)
	if n := tor.AnnounceWhenDue(); n != 1 {
		t.Fatalf("sent %d announces after the client floor, want 1", n)
	}
}

// Each successful response replaces the interval and min interval kept for the tracker, from
// Run's announce as well as from a forced one.
func TestForcedAnnounceTakesChangedTrackerValues(t *testing.T) {
	read := func(ts *trackerScraper) (interval, minInterval, wait time.Duration) {
		ts.ann.mu.Lock()
		defer ts.ann.mu.Unlock()
		return ts.ann.interval, ts.ann.minInterval, ts.ann.wait
	}
	t.Run("forced announce", func(t *testing.T) {
		tr := newFakeTracker(t)
		_, tor, _ := newPrivateTorrent(t, tr)
		ts := scraperOf(t, tor)
		settled(t, tr, ts, 1)
		tr.set(func(ft *fakeTracker) { ft.interval, ft.minInterval = 7*time.Second, 3*time.Second })
		sleepSinceLastAnnounce(ts, fakeMinInterval+50*time.Millisecond)
		if n := tor.AnnounceWhenDue(); n != 1 {
			t.Fatalf("sent %d, want 1", n)
		}
		if i, m, w := read(ts); i != 7*time.Second || m != 3*time.Second || w != 7*time.Second {
			t.Errorf("interval %v, min interval %v, wait %v; want 7 s, 3 s, 7 s", i, m, w)
		}
	})
	t.Run("regular announce", func(t *testing.T) {
		tr := newFakeTracker(t)
		tr.set(func(ft *fakeTracker) { ft.interval, ft.minInterval = 2*time.Second, 1*time.Second })
		_, tor, _ := newPrivateTorrent(t, tr)
		ts := scraperOf(t, tor)
		settled(t, tr, ts, 1)
		tr.set(func(ft *fakeTracker) { ft.interval, ft.minInterval = 5*time.Second, 3*time.Second })
		settled(t, tr, ts, 2)
		if i, m, w := read(ts); i != 5*time.Second || m != 3*time.Second || w != 5*time.Second {
			t.Errorf("interval %v, min interval %v, wait %v; want 5 s, 3 s, 5 s", i, m, w)
		}
	})
}

// A forced announce is the torrent's last announce: the status line and the failed-announce
// check read lastAnnounce, so it carries the forced announce's completion and next wait.
func TestForcedAnnounceUpdatesLastAnnounce(t *testing.T) {
	tr := newFakeTracker(t)
	_, tor, _ := newPrivateTorrent(t, tr)
	ts := scraperOf(t, tor)
	settled(t, tr, ts, 1)
	sleepSinceLastAnnounce(ts, fakeMinInterval+50*time.Millisecond)
	ts.ann.mu.Lock()
	before := ts.ann.lastDone
	ts.ann.mu.Unlock()
	if n := tor.AnnounceWhenDue(); n != 1 {
		t.Fatalf("sent %d, want 1", n)
	}
	ts.ann.mu.Lock()
	doneAt, wait := ts.ann.lastDone, ts.ann.wait
	ts.ann.mu.Unlock()
	tor.cl.rLock()
	last := ts.lastAnnounce
	tor.cl.rUnlock()
	if !doneAt.After(before) {
		t.Fatal("the forced announce was not recorded")
	}
	if !last.Completed.Equal(doneAt) || last.Interval != wait || last.Err != nil {
		t.Errorf("lastAnnounce is completed %v interval %v err %v; want the forced announce (%v, %v)",
			last.Completed, last.Interval, last.Err, doneAt, wait)
	}
}

// Without a ClientTrackerConfig floor, two announces to one tracker are a minute apart at least.
func TestForcedAnnounceDefaultFloorIsAMinute(t *testing.T) {
	if got := NewDefaultClientConfig().announceFloor(); got != time.Minute {
		t.Errorf("default announce floor = %v, want 1m", got)
	}
}

// Run's due check never waits on a timer of its own: when a forced announce has moved the last
// announce it returns at once, so Run recalculates (and sees wantPeers again); when one is in
// flight it returns the channel that ends it. Neither reserves.
func TestForcedAnnounceTryBeginReturnsInsteadOfWaiting(t *testing.T) {
	tr := newFakeTracker(t)
	_, tor, _ := newPrivateTorrent(t, tr)
	ts := scraperOf(t, tor)
	settled(t, tr, ts, 1)
	a := &ts.ann
	a.mu.Lock()
	a.lastDone = time.Now()
	a.mu.Unlock()
	if retry, ok := ts.tryBegin(time.Hour); ok || retry != nil {
		t.Errorf("not due: ok=%v retry=%v, want false and no channel", ok, retry)
	}
	a.mu.Lock()
	inFlight := a.inFlight
	a.inFlight, a.done = true, make(chan struct{})
	a.mu.Unlock()
	if retry, ok := ts.tryBegin(0); ok || retry == nil {
		t.Errorf("in flight: ok=%v retry=%v, want false and the in-flight channel", ok, retry)
	}
	a.mu.Lock()
	a.inFlight = inFlight
	a.mu.Unlock()
	if _, ok := ts.tryBegin(0); !ok {
		t.Error("control: a due tracker was not reserved")
	}
}

// A public torrent that wants peers announces at the client floor, as before forced announces
// existed.
func TestForcedAnnouncePublicTorrentStillShortensToTheFloor(t *testing.T) {
	tr := newFakeTracker(t)
	_, _, _ = newTorrentWith(t, tr, false, 100*time.Millisecond)
	time.Sleep(1500 * time.Millisecond)
	if n := len(tr.snapshot()); n < 5 {
		t.Errorf("%d announces in 1.5 s with a 4 s tracker interval and a 100 ms floor, want at least 5", n)
	}
}

// A public torrent that did not want peers when a forced announce moved the last announce wants
// them again later: Run, whose own timer has just fired, announces at the floor at once, not a
// whole interval after the forced announce.
func TestPublicRunHonoursWantPeersAfterAForcedAnnounce(t *testing.T) {
	tr := newFakeTracker(t)
	tr.set(func(ft *fakeTracker) { ft.delay = 300 * time.Millisecond })
	_, tor, _ := newTorrentWith(t, tr, false, 100*time.Millisecond)
	ts := scraperOf(t, tor)
	setWant := func(want bool) {
		max := 0
		if want {
			max = 50
		}
		tor.SetMaxEstablishedConns(max)
		tor.cl.lock()
		tor.updateWantPeersEvent()
		tor.cl.unlock()
	}
	setWant(false) // while the first announce is in flight, so Run reads it as not wanting
	settled(t, tr, ts, 1)
	tr.set(func(ft *fakeTracker) { ft.delay = 0 })
	ts.ann.mu.Lock()
	t0 := ts.ann.lastDone
	ts.ann.mu.Unlock()
	// Forced at 2 s; Run's own timer (interval 4 s) fires at 4 s and, with the forced announce
	// in between, would wait until 6 s.
	time.Sleep(time.Until(t0.Add(2 * time.Second)))
	if n := tor.AnnounceWhenDue(); n != 1 {
		t.Fatalf("forced announce sent %d, want 1", n)
	}
	time.Sleep(time.Until(t0.Add(4300 * time.Millisecond)))
	if n := len(tr.snapshot()); n != 2 {
		t.Fatalf("%d announces before wanting peers, want 2", n)
	}
	setWant(true)
	time.Sleep(700 * time.Millisecond) // t0+5.0 s: before the 6 s a stuck Run would wait for
	if n := len(tr.snapshot()); n < 3 {
		t.Errorf("%d announces 0.7 s after wanting peers, want Run's third at the floor", n)
	}
}
