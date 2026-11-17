package torrent

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// A forced announce is due only once the tracker has answered and its own interval has run
// out: a tracker still being asked for the first time, or one inside its interval, is not
// asked again.
func TestForcedAnnounceDue(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		last trackerAnnounceResult
		want bool
	}{
		{"never answered", trackerAnnounceResult{}, false},
		{"inside its interval", trackerAnnounceResult{Completed: now.Add(-time.Minute), Interval: 30 * time.Minute}, false},
		{"interval over", trackerAnnounceResult{Completed: now.Add(-31 * time.Minute), Interval: 30 * time.Minute}, true},
		{"failed, backoff running", trackerAnnounceResult{Completed: now.Add(-time.Minute), Interval: 5 * time.Minute, Err: http.ErrHandlerTimeout}, false},
		{"failed, backoff over", trackerAnnounceResult{Completed: now.Add(-6 * time.Minute), Interval: 5 * time.Minute, Err: http.ErrHandlerTimeout}, true},
	}
	for _, c := range cases {
		ts := &trackerScraper{lastAnnounce: c.last}
		if got := ts.forcedDue(now); got != c.want {
			t.Errorf("%s: forcedDue = %v, want %v", c.name, got, c.want)
		}
	}
}

// fakeTracker answers every announce with an empty peer list and a 30 minute interval, and
// counts them.
func fakeTracker(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := bencode.Marshal(map[string]any{"interval": 1800, "peers": ""})
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Hard rule 3: the forced announce of a private torrent goes to the trackers of its own
// metainfo and nowhere else (no DHT, no tracker we did not store), and never inside the
// tracker's interval, so it cannot be used to hammer a tracker.
func TestPrivateForcedAnnounceStaysOnItsOwnTrackers(t *testing.T) {
	own, ownHits := fakeTracker(t)
	_, otherHits := fakeTracker(t) // a tracker the torrent does not list

	cfg := NewDefaultClientConfig()
	cfg.DataDir = t.TempDir()
	cfg.ListenHost = func(string) string { return "127.0.0.1" }
	cfg.ListenPort = 0
	cfg.NoDefaultPortForwarding = true
	cfg.DisableIPv6 = true
	cfg.NoDHT = true
	cfg.PeriodicallyAnnounceTorrentsToDht = true
	cl, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	fd := &fakeDht{}
	cl.AddDhtServer(fd)

	yes := true
	b, err := bencode.Marshal(metainfo.Info{Name: "private", Length: 16384, PieceLength: 16384, Pieces: make([]byte, 20), Private: &yes})
	if err != nil {
		t.Fatal(err)
	}
	spec := &TorrentSpec{InfoHash: metainfo.HashBytes(b), InfoBytes: b, Trackers: [][]string{{own.URL + "/announce"}}}
	tor, _, err := cl.AddTorrentSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	// The torrent's own first announce ("started") must have answered before a forced one
	// is ever due.
	waitFor(t, "the first announce", func() bool {
		cl.rLock()
		defer cl.rUnlock()
		for _, ta := range tor.trackerAnnouncers {
			if ts, ok := ta.(*trackerScraper); ok && !ts.lastAnnounce.Completed.IsZero() {
				return true
			}
		}
		return false
	})
	base := ownHits.Load()

	if n := tor.AnnounceWhenDue(nil); n != 0 {
		t.Fatalf("a forced announce went out inside the tracker's interval (%d trackers)", n)
	}

	// Let the tracker's 30 minute interval run out.
	cl.lock()
	for _, ta := range tor.trackerAnnouncers {
		ta.(*trackerScraper).lastAnnounce.Completed = time.Now().Add(-31 * time.Minute)
	}
	cl.unlock()

	done := make(chan [3]int, 1)
	if n := tor.AnnounceWhenDue(func(peers, errs, total int, _ []string) { done <- [3]int{peers, errs, total} }); n != 1 {
		t.Fatalf("forced announce after the interval started %d announces, want 1", n)
	}
	select {
	case r := <-done:
		if r != [3]int{0, 0, 1} {
			t.Errorf("announce result = %v, want peers 0, errs 0, total 1", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the forced announce never finished")
	}
	if got := ownHits.Load() - base; got != 1 {
		t.Errorf("own tracker saw %d forced announces, want 1", got)
	}
	// It counts as an announce: a second one right after is inside the interval again.
	if n := tor.AnnounceWhenDue(nil); n != 0 {
		t.Errorf("a second forced announce went out right after the first (%d)", n)
	}
	time.Sleep(200 * time.Millisecond)
	if n := otherHits.Load(); n != 0 {
		t.Errorf("a tracker the torrent does not list saw %d announces", n)
	}
	if n := fd.announces.Load(); n != 0 {
		t.Errorf("the DHT saw %d announces of a private torrent", n)
	}
}
