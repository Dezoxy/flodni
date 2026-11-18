package daemon

import (
	"strings"
	"sync/atomic"
	"testing"
)

// The pump reads the last chunk with the player at the end; before it decides, the player
// moves back (a seek). The pump re-anchors to the player instead of ending behind it.
func TestAPumpAtTheEndReanchorsToAPlayerThatMovedAway(t *testing.T) {
	const path = "/test/eof-reanchor-branch.mkv"
	var once atomic.Bool
	var player *simHandle
	t.Cleanup(func() { pumpEOFHook = nil }) // registered first: runs after the sim's cleanup has ended the pump
	sim := newPumpSim(t, path, eofSize, eofSize-100*mb, 0)
	buf := captureLog()
	pumpEOFHook = func() {
		if once.CompareAndSwap(false, true) {
			atomic.StoreInt64(&player.h.lastOff, eofAt+100)
		}
	}
	player = sim.open("A")
	sim.settle()
	playTo(player, eofSize-100*mb, eofSize-54*mb) // 946 MiB: the pump may read the last chunk, and the player then stops

	waitUntil(t, "the pump to re-anchor", func() bool {
		return strings.Contains(buf.String(), "[V306] Pump reached EOF ahead of the player; re-anchoring to 108MB")
	})
	if !pumpRegistered(path) {
		t.Fatalf("the pump ended behind the player:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), v306EOF) {
		t.Error("a pump that re-anchored was reported as having reached the end")
	}
	pumpFollows(t, sim, eofAt)
}
