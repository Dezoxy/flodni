package torrent

import (
	"testing"
	"time"
)

func TestEarlyEligibleLocked(t *testing.T) {
	for _, c := range []struct {
		name string
		a    announceState
		want bool
	}{
		{"never announced", announceState{}, false},
		{"failed", announceState{interval: 4 * time.Second, minInterval: time.Second}, false},
		{"no min interval", announceState{ok: true, interval: 4 * time.Second}, false},
		{"min equals interval", announceState{ok: true, interval: 4 * time.Second, minInterval: 4 * time.Second}, false},
		{"min above interval", announceState{ok: true, interval: 4 * time.Second, minInterval: 8 * time.Second}, false},
		{"min below interval", announceState{ok: true, interval: 4 * time.Second, minInterval: time.Second}, true},
	} {
		if got := c.a.earlyEligibleLocked(); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// setAnn overwrites a scraper's record the way a finished announce would leave it.
func setAnn(ts *trackerScraper, ok bool, interval, min time.Duration) {
	ts.ann.mu.Lock()
	ts.ann.ok, ts.ann.interval, ts.ann.minInterval = ok, interval, min
	ts.ann.mu.Unlock()
}

func TestEarlyAnnounceState(t *testing.T) {
	tr := newFakeTracker(t)
	tr.set(func(ft *fakeTracker) { ft.delay = 3 * time.Second }) // the first announce stays open
	_, tor, _ := newPrivateTorrent(t, tr)
	ts := scraperOf(t, tor)
	if known, eligible := tor.EarlyAnnounceState(); known || eligible {
		t.Errorf("before any announce: known %v eligible %v, want false/false", known, eligible)
	}
	setAnn(ts, false, 4*time.Second, time.Second) // failed only
	if known, eligible := tor.EarlyAnnounceState(); known || eligible {
		t.Errorf("failed only: known %v eligible %v, want false/false", known, eligible)
	}
	for name, c := range map[string]struct {
		min      time.Duration
		wantElig bool
	}{
		"min 0":       {0, false},
		"min >= int.": {4 * time.Second, false},
		"min < int.":  {time.Second, true},
	} {
		setAnn(ts, true, 4*time.Second, c.min)
		if known, eligible := tor.EarlyAnnounceState(); !known || eligible != c.wantElig {
			t.Errorf("%s: known %v eligible %v, want true/%v", name, known, eligible, c.wantElig)
		}
	}
}

// One eligible tracker among several makes the torrent eligible.
func TestEarlyAnnounceStateAnyTracker(t *testing.T) {
	tr := newFakeTracker(t)
	tr.set(func(ft *fakeTracker) { ft.delay = 3 * time.Second })
	_, tor, _ := newPrivateTorrent(t, tr)
	first := scraperOf(t, tor)
	second := &trackerScraper{}
	tor.cl.lock()
	tor.trackerAnnouncers = map[string]torrentTrackerAnnouncer{"a": first, "b": second}
	tor.cl.unlock()
	setAnn(first, true, 4*time.Second, 0)
	setAnn(second, true, 4*time.Second, time.Second)
	if known, eligible := tor.EarlyAnnounceState(); !known || !eligible {
		t.Errorf("known %v eligible %v, want true/true", known, eligible)
	}
	setAnn(second, true, 4*time.Second, 0)
	if known, eligible := tor.EarlyAnnounceState(); !known || eligible {
		t.Errorf("known %v eligible %v, want true/false", known, eligible)
	}
}
