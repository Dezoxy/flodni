package torrent

import (
	"fmt"
	"testing"
	"time"
)

func refillV2On(cfg *ClientConfig) { cfg.RefillV2 = true }

func TestRefillV2DefaultsOff(t *testing.T) {
	if NewDefaultClientConfig().RefillV2 {
		t.Fatal("RefillV2 is on in the default client config")
	}
}

// The candidate cap: a refill of a peer with a 16-chunk queue considers max(2 pieces, 4x queue)
// chunks, not the whole readahead window.
func TestRefillV2CapsCandidates(t *testing.T) {
	g := newRefillRig(t, 40, 8, refillV2On) // 320 chunks wanted
	p, impl := g.addPeer("a")
	cands := g.refill(p, "test")
	if want := 64; cands != want { // max(2*8, 4*16)
		t.Errorf("candidates = %d, want %d", cands, want)
	}
	if len(impl.requested) != 16 {
		t.Errorf("requested %d chunks, want the 16 of the queue", len(impl.requested))
	}
}

// The cap leaves the steal budget alone: a window of free chunks must not hide the chunk
// another peer holds, wherever its piece falls in the scan.
func TestRefillV2StealCandidatesKeepTheirOwnBudget(t *testing.T) {
	g := newRefillRig(t, 40, 8, refillV2On)
	a, _ := g.addPeer("a")
	b, _ := g.addPeer("b")
	g.forceHold(b, 300)
	g.cl.lock()
	defer g.cl.unlock()
	next := a.getDesiredRequestState()
	idx := next.Requests.requestIndexes
	if len(idx) != 65 { // 64 free + the one steal candidate
		t.Errorf("candidates = %d, want 65", len(idx))
	}
	found := false
	for _, r := range idx {
		found = found || r == 300
	}
	if !found {
		t.Error("the chunk B holds is not among the candidates")
	}
}

// Steal from a stalled holder: the same scenario as TestRefillFlagOffNoStealFromStalledHolder.
func TestRefillV2StealsFromStalledHolder(t *testing.T) {
	g := newRefillRig(t, 2, 4, refillV2On)
	a, _ := g.addPeer("a")
	b, _ := g.addPeer("b")
	g.forceHold(a, 0, 1, 2, 3, 4)
	g.forceHold(b, 5, 6)
	g.cl.lock()
	b.lastStartedExpectingToReceiveChunks = time.Now().Add(-time.Minute)
	g.cl.unlock()
	g.refill(a, "test")
	if got, want := fmt.Sprint(g.held(a)), "[0 1 2 3 4 5 6 7]"; got != want {
		t.Errorf("A holds %s, want %s", got, want)
	}
	if got := fmt.Sprint(g.held(b)); got != "[]" {
		t.Errorf("B still holds %s", got)
	}
}

// A holder that is delivering is not robbed just because the stealer has a deeper queue.
func TestRefillV2DoesNotStealFromALiveHolder(t *testing.T) {
	g := newRefillRig(t, 2, 4, refillV2On)
	a, _ := g.addPeer("a")
	b, _ := g.addPeer("b")
	g.forceHold(a, 0, 1, 2, 3, 4)
	g.forceHold(b, 5, 6)
	g.cl.lock()
	b.lastUsefulChunkReceived = time.Now()
	g.cl.unlock()
	g.refill(a, "test")
	if got, want := fmt.Sprint(g.held(b)), "[5 6]"; got != want {
		t.Errorf("B holds %s, want %s", got, want)
	}
}

// Half-queue refill: a peer at or under half of its queue asks for more, unless it dropped a
// request in the last few seconds.
