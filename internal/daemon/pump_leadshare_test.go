package daemon

import (
	"flodni/internal/promtext"
	"fmt"
	"strings"
	"testing"
)

func TestLeadBudget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name            string
		budget, streams int64
		want            int64
	}{
		{"no stream keeps the whole budget", 1024 * mb, 0, 1024 * mb},
		{"one stream keeps the whole budget", 1024 * mb, 1, 1024 * mb},
		{"two streams split it", 1024 * mb, 2, 512 * mb},
		{"six streams split it", 1024 * mb, 6, 1024 * mb / 6},
		{"the floor holds when the split goes below it", 64 * mb, 6, minLeadShare},
		{"a budget under the floor stays whole", 20 * mb, 3, 20 * mb},
		{"a zero budget stays zero", 0, 4, 0},
	}
	for _, c := range cases {
		if got := leadBudget(c.budget, c.streams); got != c.want {
			t.Errorf("%s: leadBudget(%d, %d) = %d, want %d", c.name, c.budget, c.streams, got, c.want)
		}
	}
}

func TestStreamLeadLimit(t *testing.T) {
	t.Parallel()
	const chunk = 16 * mb
	cases := []struct {
		name            string
		budget, streams int64
		chunk, want     int64
	}{
		{"one stream is today's limit", 1024 * mb, 1, chunk, pumpLeadLimit(1024*mb, chunk)},
		{"one stream, budget minus three chunks", 1024 * mb, 1, chunk, 1024*mb - 3*chunk},
		{"two streams", 1024 * mb, 2, chunk, 512*mb - 3*chunk},
		{"six streams", 1024 * mb, 6, chunk, 1024*mb/6 - 3*chunk},
		{"the floor", 64 * mb, 6, 4 * mb, minLeadShare - 3*4*mb},
		{"two chunks at least for a crowded big chunk", 64 * mb, 8, 16 * mb, 2*16*mb - 16*mb},
		{"a budget under one chunk", 8 * mb, 3, 16 * mb, 8 * mb},
		{"no chunk size", 100 * mb, 2, 0, 50 * mb},
	}
	for _, c := range cases {
		got := streamLeadLimit(c.budget, c.streams, c.chunk)
		if got != c.want {
			t.Errorf("%s: streamLeadLimit(%d, %d, %d) = %d, want %d", c.name, c.budget, c.streams, c.chunk, got, c.want)
		}
		if c.budget > 0 && c.chunk > 0 && got < min(c.chunk, c.budget) {
			t.Errorf("%s: limit %d is under one chunk", c.name, got)
		}
	}
}

func TestLivePumpCount(t *testing.T) {
	newTestApp(t)
	if n := livePumpCount(); n != 0 {
		t.Fatalf("empty registry: %d", n)
	}
	t.Cleanup(func() { app.activePumps.Delete("/a"); app.activePumps.Delete("/b") })
	app.activePumps.Store("/a", &NativePumpState{})
	app.activePumps.Store("/b", &NativePumpState{})
	if n := livePumpCount(); n != 2 {
		t.Fatalf("two pumps: %d", n)
	}
	app.activePumps.Delete("/a")
	if n := livePumpCount(); n != 1 {
		t.Fatalf("one pump: %d", n)
	}
}

// Two pumps share a budget that fits one stream's lead twice over only if each holds its
// share. Neither may lead past its share, and the chunks just ahead of each player must
// stay cached: the evictions (oldest written first) fall behind the players.
func TestConcurrentPumpsKeepTheirShareAhead(t *testing.T) {
	useEngineReadPath(t) // budget 64 MiB, 4 MiB chunks
	const chunk, size = 4 * mb, 4 * gb
	budget := gc().ReadAheadBudget
	paths := []string{"/test/share-a.mkv", "/test/share-b.mkv"}
	handles := make([]*MkvHandle, len(paths))
	pumpOff := make([]int64, len(paths))
	player := make([]int64, len(paths))
	rd := &fakeReader{}
	for i, p := range paths {
		handles[i] = engineHandle(p, size)
		app.activePumps.Store(p, &NativePumpState{path: p})
		t.Cleanup(func() { app.activePumps.Delete(p) })
		player[i] = 100 * mb
		pumpOff[i] = player[i]
	}
	limit := budget/2 - 3*chunk // two streams: half the budget minus the slack, spelled out
	ev0 := raCache.Evictions()

	for round := 0; round < 25; round++ {
		for i := range paths {
			for { // pump until the hard limit holds it
				stop, next := handles[i].nativePumpChunk(rd, pumpOff[i], chunk, player[i])
				if stop {
					t.Fatalf("pump %d stopped at %d", i, pumpOff[i])
				}
				if next == pumpOff[i] {
					break
				}
				pumpOff[i] = next
			}
			if lead := pumpOff[i] - player[i]; lead > limit+chunk {
				t.Fatalf("round %d: pump %d leads %d, over its share %d (+ one chunk)", round, i, lead, limit)
			}
		}
		for i, p := range paths {
			ahead := limit - chunk // inside the window the player's next reads fall in
			if !cachedAcross(p, player[i], player[i]+ahead-1) {
				t.Fatalf("round %d: stream %d lost chunks within %d bytes ahead of its player (evictions %d)", round, i, ahead, raCache.Evictions()-ev0)
			}
			player[i] += chunk // the players advance one chunk per round
		}
	}
}

func TestLeadShareGauges(t *testing.T) {
	useEngineReadPath(t) // budget 64 MiB, base 4 MiB
	t.Cleanup(func() { app.activePumps.Delete("/g1"); app.activePumps.Delete("/g2") })
	gauge := func() string {
		var w promtext.Writer
		collectReadAheadProm(&w)
		return string(w.Bytes())
	}
	if body := gauge(); !strings.Contains(body, "flodni_active_pumps 0\n") ||
		!strings.Contains(body, fmt.Sprintf("flodni_pump_lead_limit_bytes %d\n", 64*mb-3*4*mb)) {
		t.Fatalf("no pump: single-stream limit expected:\n%s", body)
	}
	app.activePumps.Store("/g1", &NativePumpState{})
	app.activePumps.Store("/g2", &NativePumpState{})
	if body := gauge(); !strings.Contains(body, "flodni_active_pumps 2\n") ||
		!strings.Contains(body, fmt.Sprintf("flodni_pump_lead_limit_bytes %d\n", 32*mb-3*4*mb)) {
		t.Fatalf("two pumps: half the budget minus slack expected:\n%s", body)
	}
}
