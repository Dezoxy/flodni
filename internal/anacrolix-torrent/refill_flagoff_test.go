package torrent

import (
	"fmt"
	"testing"
	"time"
)

// Characterization of the request refill as it was before engine_refill_v2: with the flag off
// (the default) these decisions must stay what they are. The goldens were recorded on the code
// before the flag existed.

// Table of stealPermitted decisions for a holder that is not stalled.
func TestRefillFlagOffStealPermittedTable(t *testing.T) {
	now := time.Now()
	older, newer := now.Add(-time.Minute), now
	yes := func() bool { return true }
	no := func() bool { return false }
	cases := []struct {
		name         string
		reason       string
		diff         int64
		stealer, hld time.Time
		grace        func() bool
		want         bool
	}{
		{"cancel never steals back", "Peer.cancel", -5, newer, older, yes, false},
		{"far ahead of the holder", "x", 2, newer, older, yes, false},
		{"one ahead, stealer older", "x", 1, older, newer, yes, false},
		{"one ahead, tie", "x", 1, now, now, yes, false},
		{"one ahead, stealer fresher", "x", 1, newer, older, yes, true},
		{"behind the holder", "x", -3, older, newer, yes, true},
		{"grace not elapsed", "x", 0, newer, older, no, false},
	}
	for _, c := range cases {
		if got := stealPermitted(c.reason, c.diff, c.stealer, c.hld, false, c.grace); got != c.want {
			t.Errorf("%s: stealPermitted = %v, want %v", c.name, got, c.want)
		}
	}
}

// Scripted scenario 1: 10 pieces of 8 chunks, one peer with an empty queue. Everything the
// strategy offers is a candidate (no cap), and the refill requests the first nominalMax chunks in
// order.
func TestRefillFlagOffFreeRefill(t *testing.T) {
	g := newRefillRig(t, 10, 8, nil)
	p, impl := g.addPeer("a")
	cands := g.refill(p, "test")
	if cands != 80 {
		t.Errorf("candidates = %d, want 80 (uncapped)", cands)
	}
	// Ties between pieces are broken at random, so the pin is the shape: 16 requests, two
	// whole pieces, each in chunk order.
	if len(impl.requested) != 16 {
		t.Fatalf("requested %d chunks, want 16: %v", len(impl.requested), impl.requested)
	}
	for half := 0; half < 2; half++ {
		run := impl.requested[half*8 : half*8+8]
		for i, r := range run {
			if r != run[0]+RequestIndex(i) || run[0]%8 != 0 {
				t.Errorf("request run %d is not one piece in chunk order: %v", half, run)
				break
			}
		}
	}
}

// Scripted scenario 2: A holds 5 chunks, B holds 2 chunks and has delivered nothing for a
// minute. Nothing is unrequested, so A's refill reaches for B's chunks. Queue sizes forbid the
// steal and today a stalled holder does not change that.
func TestRefillFlagOffNoStealFromStalledHolder(t *testing.T) {
	g := newRefillRig(t, 2, 4, nil)
	a, _ := g.addPeer("a")
	b, _ := g.addPeer("b")
	g.forceHold(a, 0, 1, 2, 3, 4)
	g.forceHold(b, 5, 6)
	g.cl.lock()
	b.lastStartedExpectingToReceiveChunks = time.Now().Add(-time.Minute)
	g.cl.unlock()
	g.refill(a, "test")
	if got, want := fmt.Sprint(g.held(a)), "[0 1 2 3 4 7]"; got != want {
		t.Errorf("A holds %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(g.held(b)), "[5 6]"; got != want {
		t.Errorf("B holds %s, want %s", got, want)
	}
}
