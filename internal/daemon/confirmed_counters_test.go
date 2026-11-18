package daemon

import (
	"sync/atomic"
	"testing"
	"time"
)

const (
	kaHashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	kaHashB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// resetKeepaliveState isolates the registry, the label cache and the counters.
func resetKeepaliveState(t *testing.T) {
	newTestApp(t)
	t.Helper()
	reset := func() {
		app.playbackRegistry.Range(func(k, _ any) bool { app.playbackRegistry.Delete(k); return true })
		sessions.Range(func(k, _ any) bool { sessions.Delete(k); return true })
		confirmedHashes.mu.Lock()
		confirmedHashes.at = time.Time{}
		confirmedHashes.set = nil
		confirmedHashes.mu.Unlock()
		for i := range keepaliveCounters.idleCloses {
			keepaliveCounters.idleCloses[i].Store(0)
			keepaliveCounters.reopens[i].Store(0)
			for c := range keepaliveCounters.stalls[i] {
				keepaliveCounters.stalls[i][c].Store(0)
			}
		}
	}
	reset()
	prevNow := confirmedHashesNow
	t.Cleanup(func() { confirmedHashesNow = prevNow; reset() })
}

func kaCounts(c *[2]atomic.Int64) (unconfirmed, confirmed int64) {
	return c[0].Load(), c[1].Load()
}

// The label is true when any file of the torrent has a webhook-confirmed playback.
func TestConfirmedHashLabel(t *testing.T) {
	resetKeepaliveState(t)
	app.playbackRegistry.Store("/m/a.mkv", &PlaybackState{Path: "/m/a.mkv", Hash: kaHashA})
	app.playbackRegistry.Store("/m/b1.mkv", &PlaybackState{Path: "/m/b1.mkv", Hash: kaHashB})
	app.playbackRegistry.Store("/m/b2.mkv", &PlaybackState{Path: "/m/b2.mkv", Hash: kaHashB, IsHealthy: true})
	app.playbackRegistry.Store("/m/c.mkv", &PlaybackState{Path: "/m/c.mkv", Hash: "", IsHealthy: true})

	if hashConfirmed(kaHashA) {
		t.Error("unconfirmed torrent labelled confirmed")
	}
	if !hashConfirmed(kaHashB) {
		t.Error("torrent with one confirmed file labelled unconfirmed")
	}
	if hashConfirmed("") || hashConfirmed("cccccccccccccccccccccccccccccccccccccccc") {
		t.Error("empty or unknown hash labelled confirmed")
	}
}

// The confirmed set is rebuilt at most once a second.
func TestConfirmedHashesCachedForOneSecond(t *testing.T) {
	resetKeepaliveState(t)
	now := time.Unix(1000, 0)
	confirmedHashesNow = func() time.Time { return now }
	ps := &PlaybackState{Path: "/m/a.mkv", Hash: kaHashA}
	app.playbackRegistry.Store(ps.Path, ps)

	if hashConfirmed(kaHashA) {
		t.Fatal("confirmed before the webhook")
	}
	ps.IsHealthy = true
	now = now.Add(900 * time.Millisecond)
	if hashConfirmed(kaHashA) {
		t.Error("set rebuilt inside the second")
	}
	now = now.Add(200 * time.Millisecond)
	if !hashConfirmed(kaHashA) {
		t.Error("set not rebuilt after the second")
	}
}

func TestIdleCloseCountedByConfirmed(t *testing.T) {
	resetKeepaliveState(t)
	app.playbackRegistry.Store("/m/b.mkv", &PlaybackState{Path: "/m/b.mkv", Hash: kaHashB, IsHealthy: true})

	countIdleClose(kaHashA)
	countIdleClose(kaHashB)
	countIdleClose(kaHashB)
	if u, c := kaCounts(&keepaliveCounters.idleCloses); u != 1 || c != 2 {
		t.Fatalf("idle closes unconfirmed=%d confirmed=%d, want 1 and 2", u, c)
	}
}

func TestReopenCountedByConfirmedAndOnTheSession(t *testing.T) {
	resetKeepaliveState(t)
	app.playbackRegistry.Store("/m/b.mkv", &PlaybackState{Path: "/m/b.mkv", Hash: kaHashB, IsHealthy: true})
	s := &TTFFSession{path: "/m/b.mkv", hash: kaHashB}
	sessions.Store(s.path, s)

	countReopen("/m/b.mkv", kaHashB)
	countReopen("/m/a.mkv", kaHashA)
	if u, c := kaCounts(&keepaliveCounters.reopens); u != 1 || c != 1 {
		t.Fatalf("reopens unconfirmed=%d confirmed=%d, want 1 and 1", u, c)
	}
	if got := s.reopens.Load(); got != 1 {
		t.Fatalf("session reopens = %d, want 1", got)
	}
}

// A served read over the stall threshold counts; one at the threshold does not.
func TestStallCountedByConfirmedAtTtffRead(t *testing.T) {
	resetKeepaliveState(t)
	app.playbackRegistry.Store("/m/b.mkv", &PlaybackState{Path: "/m/b.mkv", Hash: kaHashB, IsHealthy: true})
	app.playbackRegistry.Store("/m/a.mkv", &PlaybackState{Path: "/m/a.mkv", Hash: kaHashA})

	ttffRead("/m/b.mkv", srcFetchBlock, ttffStallDuration+time.Millisecond, 4096, 0)
	ttffRead("/m/a.mkv", srcFetchBlock, ttffStallDuration+time.Millisecond, 4096, 0)
	ttffRead("/m/a.mkv", srcFetchBlock, ttffStallDuration, 4096, 0)
	ttffRead("/m/nosuch.mkv", srcFetchBlock, 5*time.Second, 4096, 0)
	st := &keepaliveCounters.stalls
	if u, c := st[0][0].Load()+st[0][1].Load(), st[1][0].Load()+st[1][1].Load(); u != 2 || c != 1 {
		t.Fatalf("stalls unconfirmed=%d confirmed=%d, want 2 and 1", u, c)
	}
}
