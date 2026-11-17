package torrent

import (
	"testing"
	"time"
)

func TestRefillV2CandidateLimitFor(t *testing.T) {
	for _, c := range []struct{ chunksPerPiece, nominal, want int }{
		{8, 16, 64}, {128, 16, 256}, {8, 2, 16}, {1, 150, 600},
	} {
		if got := requestCandidateLimitFor(c.chunksPerPiece, c.nominal); got != c.want {
			t.Errorf("requestCandidateLimitFor(%d, %d) = %d, want %d", c.chunksPerPiece, c.nominal, got, c.want)
		}
	}
}

func TestRefillV2PeerStalled(t *testing.T) {
	now := time.Now()
	long, short := now.Add(-10*time.Second), now.Add(-time.Second)
	for _, c := range []struct {
		name                  string
		lastUseful, expecting time.Time
		want                  bool
	}{
		{"not expecting", time.Time{}, time.Time{}, false},
		{"expecting long, nothing delivered", time.Time{}, long, true},
		{"expecting briefly", time.Time{}, short, false},
		{"delivered recently", short, long, false},
		{"delivered long ago", long, long.Add(-time.Minute), true},
	} {
		if got := peerStalled(c.lastUseful, c.expecting, now); got != c.want {
			t.Errorf("%s: peerStalled = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRefillV2WantsEarlyRefill(t *testing.T) {
	g := newRefillRig(t, 4, 8, refillV2On)
	p, _ := g.addPeer("a")
	g.refill(p, "test") // a fresh peer's queue is 16
	g.cl.lock()
	defer g.cl.unlock()
	p._chunksReceivedWhileExpecting = 100000 // warmed: the queue target is its 150 cap, under PeerMaxRequests
	p.PeerMaxRequests = 16
	now := time.Now()
	if p.wantsEarlyRefill(now) {
		t.Error("a full queue wants a refill")
	}
	var held []RequestIndex
	p.requestState.Requests.Iterate(func(r RequestIndex) bool { held = append(held, r); return true })
	for _, r := range held[:8] { // 16 -> 8, half
		p.requestState.Requests.CheckedRemove(r)
	}
	if !p.wantsEarlyRefill(now) {
		t.Error("a queue at half does not want a refill")
	}
	p.lastDropReject = now.Add(-time.Second)
	if p.wantsEarlyRefill(now) {
		t.Error("a peer that just dropped a request is refilled early")
	}
	if !p.wantsEarlyRefill(now.Add(refillPauseAfterDrop)) {
		t.Error("the pause after a drop never ends")
	}
}

func TestRefillV2ReleasedTimer(t *testing.T) {
	fired := make(chan struct{}, 4)
	rt := newReleasedTimer(time.Hour, func() { fired <- struct{}{} })
	rt.Stop()
	if rt.f.Load() != nil {
		t.Error("Stop keeps the function alive")
	}
	rt.Reset(time.Millisecond)
	select {
	case <-fired:
		t.Error("a stopped timer fired after Reset")
	case <-time.After(50 * time.Millisecond):
	}
	rt = newReleasedTimer(time.Millisecond, func() { fired <- struct{}{} })
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Error("a live timer never fired")
	}
	rt.Stop()
}

// The flag is read per client: off in the default config, on when set.
func TestRefillV2PeerFlag(t *testing.T) {
	off := newRefillRig(t, 1, 1, nil)
	pOff, _ := off.addPeer("a")
	on := newRefillRig(t, 1, 1, refillV2On)
	pOn, _ := on.addPeer("a")
	if pOff.refillV2() || !pOn.refillV2() {
		t.Errorf("refillV2() off=%v on=%v, want false and true", pOff.refillV2(), pOn.refillV2())
	}
	if (&Peer{}).refillV2() {
		t.Error("a peer with no torrent has the flag on")
	}
}
