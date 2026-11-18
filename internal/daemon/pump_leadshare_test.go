package daemon

import (
	"context"
	"flodni/internal/promtext"
	"fmt"
	"strings"
	"testing"
	"time"
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

// registerPump registers a pump for path; playing marks the path as a confirmed playback.
func registerPump(t *testing.T, path string, playing bool) {
	t.Helper()
	app.activePumps.Store(path, &NativePumpState{path: path})
	t.Cleanup(func() { app.activePumps.Delete(path) })
	if playing {
		app.playbackRegistry.Store(path, &PlaybackState{Path: path, IsHealthy: true})
		t.Cleanup(func() { app.playbackRegistry.Delete(path) })
	}
}

func TestPlayingPumpCount(t *testing.T) {
	newTestApp(t)
	if n := playingPumpCount(); n != 0 {
		t.Fatalf("empty registry: %d", n)
	}
	registerPump(t, "/a", true)
	registerPump(t, "/b", true)
	for i := 0; i < 5; i++ {
		registerPump(t, fmt.Sprintf("/scan%d", i), false) // scan, paused or grace pumps
	}
	if n := playingPumpCount(); n != 2 {
		t.Fatalf("two playing pumps and five others: %d", n)
	}
	app.activePumps.Delete("/a")
	if n := playingPumpCount(); n != 1 {
		t.Fatalf("one playing pump: %d", n)
	}
}

// One viewer plus scan pumps: the viewer's lead is the single-stream lead; two viewers halve it.
func TestScanPumpsDoNotShrinkAViewersLead(t *testing.T) {
	useEngineReadPath(t) // budget 64 MiB, 4 MiB chunks
	const chunk = 4 * mb
	viewer := engineHandle("/test/viewer-1.mkv", 4*gb)
	registerPump(t, viewer.path, true)
	scans := make([]*MkvHandle, 5)
	for i := range scans {
		scans[i] = engineHandle(fmt.Sprintf("/test/scan-%d.mkv", i), 4*gb)
		registerPump(t, scans[i].path, false)
	}
	if n := viewer.leadStreams(); n != 1 {
		t.Fatalf("viewer divisor with five scan pumps = %d, want 1", n)
	}
	if n := scans[0].leadStreams(); n != 2 {
		t.Fatalf("a scan pump's own divisor = %d, want playing+1 = 2", n)
	}
	// The viewer may lead by the single-stream limit: a chunk that far ahead is read.
	single := pumpLeadLimit(gc().ReadAheadBudget, chunk)
	rd := &fakeReader{}
	player := 100 * mb
	if _, next := viewer.nativePumpChunk(rd, player+single/chunk*chunk, chunk, player); len(rd.readCalls()) != 1 || next == player+single/chunk*chunk {
		t.Fatalf("the viewer's pump was held back at its single-stream lead (reads %d)", len(rd.readCalls()))
	}
	registerPump(t, "/test/viewer-2.mkv", true)
	if n := viewer.leadStreams(); n != 2 {
		t.Fatalf("two viewers: divisor %d, want 2", n)
	}
	if got, want := streamLeadLimit(gc().ReadAheadBudget, viewer.leadStreams(), chunk), gc().ReadAheadBudget/2-3*chunk; got != want {
		t.Fatalf("two viewers: lead %d, want %d", got, want)
	}
	if n := viewer.leadStreams(); n != 2 {
		t.Fatalf("divisor %d", n)
	}
}

// A pump parked a budget ahead of its player while it ran alone must rejoin the player once a
// second stream shrinks its share: stranded means more than twice the share ahead.
func TestStrandedPumpReanchorsWhenTheShareShrinks(t *testing.T) {
	useEngineReadPath(t) // budget 64 MiB, 4 MiB chunks
	const chunk, size = 4 * mb, 4 * gb
	const path = "/test/share-strand.mkv"
	player := 100 * mb
	h, ps := pumpHandle(path, size)
	h.lastActivityTime = time.Now()
	registerPump(t, path, true)
	registerPump(t, "/test/share-strand-other.mkv", true)
	primary := engineHandle(path, size)
	primary.isPrimaryHandle.Store(true)
	primary.lastOff = player
	registerHandle(t, primary)
	limit := gc().ReadAheadBudget/2 - 3*chunk // two playing streams
	// The chunks the pump will walk over are cached already: it must not fetch them.
	for o := player; o <= player+limit; o += chunk {
		raCache.Put(path, o, o+chunk-1, wantPattern(int(chunk), o))
	}
	rd := &fakeReader{}
	takeSlot(h)
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	// Parked just past the budget ahead: within 2x the budget, beyond 2x the two-stream share.
	h.nativePump(ctx, rd, player+gc().ReadAheadBudget+chunk, ps)
	if off := ps.pumpOff.Load(); off < player || off > player+limit+chunk {
		t.Fatalf("pump offset %d MiB is not back at the player (%d MiB) within its share", off/mb, player/mb)
	}
	if n := len(rd.readCalls()); n != 0 {
		t.Fatalf("the re-anchored pump fetched %d cached chunks: %+v", n, rd.readCalls())
	}
}

// The transition: A ran alone and filled the cache to the single-stream lead; B starts.
// B's pump writes evict A's oldest chunks, the ones just ahead of A's player. The pumps'
// leads fit the budget together now, but the eviction order across streams is the cache's.
func TestSecondStreamDoesNotEvictTheFirstStreamsLead(t *testing.T) {
	t.Skip("cross-stream eviction: fixed by the raCache multi-stream PR")
	useEngineReadPath(t) // budget 64 MiB, 4 MiB chunks
	const chunk, size = 4 * mb, 4 * gb
	a, b := engineHandle("/test/trans-a.mkv", size), engineHandle("/test/trans-b.mkv", size)
	registerPump(t, a.path, true)
	rd := &fakeReader{}
	playerA, playerB := 100*mb, 100*mb
	offA, offB := playerA, playerB
	for { // A alone, up to the single-stream lead
		_, next := a.nativePumpChunk(rd, offA, chunk, playerA)
		if next == offA {
			break
		}
		offA = next
	}
	registerPump(t, b.path, true)
	for {
		_, next := b.nativePumpChunk(rd, offB, chunk, playerB)
		if next == offB {
			break
		}
		offB = next
	}
	window := gc().ReadAheadBudget/2 - 4*chunk
	if !cachedAcross(a.path, playerA, playerA+window-1) {
		t.Fatalf("stream A lost chunks within %d bytes ahead of its player", window)
	}
}

func TestNativePumpWithNoReaderLeavesNoZombieEntry(t *testing.T) {
	useEngineReadPath(t)
	const path = "/test/nil-reader-zombie.mkv"
	h, ps := pumpHandle(path, 10*mb)
	takeSlot(h)
	app.activePumps.Store(path, ps)
	t.Cleanup(func() { app.activePumps.Delete(path) })
	h.nativePump(context.Background(), nil, 0, ps)
	if _, ok := app.activePumps.Load(path); ok {
		t.Error("a pump that never started must not stay registered")
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
		registerPump(t, p, true)
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
	gauge := func() string {
		var w promtext.Writer
		collectReadAheadProm(&w)
		return string(w.Bytes())
	}
	if body := gauge(); !strings.Contains(body, "flodni_playing_pumps 0\n") ||
		!strings.Contains(body, fmt.Sprintf("flodni_pump_lead_limit_bytes %d\n", 64*mb-3*4*mb)) {
		t.Fatalf("no pump: single-stream limit expected:\n%s", body)
	}
	registerPump(t, "/g1", true)
	registerPump(t, "/g2", true)
	registerPump(t, "/g3", false) // not playing: not counted
	if body := gauge(); !strings.Contains(body, "flodni_playing_pumps 2\n") ||
		!strings.Contains(body, fmt.Sprintf("flodni_pump_lead_limit_bytes %d\n", 32*mb-3*4*mb)) {
		t.Fatalf("two pumps: half the budget minus slack expected:\n%s", body)
	}
}
