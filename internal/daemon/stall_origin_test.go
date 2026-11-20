package daemon

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// originCounts is the unconfirmed row of the counter for one cause, by origin.
func originCounts(cause int) (out [numStallOrigins]int64) {
	for o := range out {
		out[o] = keepaliveCounters.stalls[0][cause][o].Load()
	}
	return out
}

// stubRefetched replaces the engine lookup and records what it was asked.
type refetchAsk struct {
	hash string
	idx  int
	off  int64
	n    int
	at   time.Time
}

func stubRefetched(t *testing.T, answer bool) *[]refetchAsk {
	t.Helper()
	var asks []refetchAsk
	prev := stallRefetched
	stallRefetched = func(hash string, idx int, off int64, n int, since time.Time) bool {
		asks = append(asks, refetchAsk{hash, idx, off, n, since})
		return answer
	}
	t.Cleanup(func() { stallRefetched = prev })
	return &asks
}

func setPump(t *testing.T, path string, pumpOff int64) {
	t.Helper()
	ps := &NativePumpState{path: path}
	ps.pumpOff.Store(pumpOff)
	app.activePumps.Store(path, ps)
	t.Cleanup(func() { app.activePumps.Delete(path) })
}

// Each stall lands under exactly one origin: evicted wins over the pump, then a pump at or
// behind the read is pump_behind, no pump is no_pump and a pump ahead of the read is swarm.
func TestStallOriginClassification(t *testing.T) {
	const off = 500 * stallMiB
	for _, tc := range []struct {
		name      string
		refetched bool
		pump      bool
		pumpOff   int64
		want      int
	}{
		{"evicted beats a lagging pump", true, true, off - stallMiB, stallOriginEvicted},
		{"evicted without a pump", true, false, 0, stallOriginEvicted},
		{"pump behind the read", false, true, off - stallMiB, stallOriginPumpBehind},
		{"pump exactly at the read", false, true, off, stallOriginPumpBehind},
		{"pump ahead of the read", false, true, off + 1, stallOriginSwarm},
		{"no pump", false, false, 0, stallOriginNoPump},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess, clock := stallEnv(t)
			sess.fileIndex = 3
			asks := stubRefetched(t, tc.refetched)
			if tc.pump {
				setPump(t, "/m/s.mkv", tc.pumpOff)
			}
			stallRead(srcFetchBlock, slow, off)
			got := originCounts(stallCausePlayback)
			var total int64
			for o, n := range got {
				total += n
				if (o == tc.want) != (n == 1) {
					t.Errorf("origin %s = %d, want it %v", stallOriginNames[o], n, o == tc.want)
				}
			}
			if total != 1 {
				t.Errorf("origins sum to %d, want 1", total)
			}
			// The engine is asked about the read's own span, from the second it began.
			if len(*asks) != 1 {
				t.Fatalf("engine asked %d times, want 1", len(*asks))
			}
			a := (*asks)[0]
			if a.hash != kaHashA || a.idx != 3 || a.off != off || a.n != int(stallMiB) || !a.at.Equal(clock.Add(-slow)) {
				t.Errorf("asked %+v, want hash %s file 3 off %d n %d at %v", a, kaHashA, off, stallMiB, clock.Add(-slow))
			}
		})
	}
}

// A read at or under the threshold asks nothing and counts nothing.
func TestStallOriginNotAskedForFastReads(t *testing.T) {
	stallEnv(t)
	asks := stubRefetched(t, true)
	stallRead(srcFetchBlock, ttffStallDuration, 500*stallMiB)
	if len(*asks) != 0 {
		t.Errorf("engine asked %d times for a read that did not stall", len(*asks))
	}
	if p, k := stallCounts(); p+k != 0 {
		t.Errorf("a read at the threshold counted %d stalls", p+k)
	}
}

// A stall the engine cannot be asked about (no session, so no hash or file index; a session
// whose stub carries no file index; a read that returned nothing) is never evicted.
func TestStallOriginNeverEvictedWithoutAKey(t *testing.T) {
	t.Run("no session", func(t *testing.T) {
		resetKeepaliveState(t)
		asks := stubRefetched(t, true)
		ttffRead("/m/none.mkv", srcFetchBlock, slow, int(stallMiB), 0)
		if len(*asks) != 0 {
			t.Errorf("engine asked %d times", len(*asks))
		}
		if got := keepaliveCounters.stalls[0][stallCausePlayback][stallOriginNoPump].Load(); got != 1 {
			t.Errorf("no_pump = %d, want 1", got)
		}
	})
	t.Run("no file index", func(t *testing.T) {
		stallEnv(t) // the stub of this session carries index 0
		asks := stubRefetched(t, true)
		stallRead(srcFetchBlock, slow, 500*stallMiB)
		if len(*asks) != 0 || originCounts(stallCausePlayback)[stallOriginEvicted] != 0 {
			t.Errorf("engine asked %d times, evicted=%d", len(*asks), originCounts(stallCausePlayback)[stallOriginEvicted])
		}
	})
	t.Run("empty read", func(t *testing.T) {
		sess, _ := stallEnv(t)
		sess.fileIndex = 3
		asks := stubRefetched(t, true)
		ttffRead("/m/s.mkv", srcFetchBlock, slow, 0, 500*stallMiB)
		if len(*asks) != 0 {
			t.Errorf("engine asked %d times for an empty read", len(*asks))
		}
	})
}

// The real lookup answers false for a torrent the engine does not hold.
func TestStallRefetchedDefaultUnknownTorrent(t *testing.T) {
	if stallRefetched(kaHashA, 1, 0, 10, time.Now()) {
		t.Error("the default lookup reported a refetch for an unknown torrent")
	}
}

// Sixteen series from the start, the origin text in the HELP, and the sum over origin is
// what the pair counted before.
func TestStallOriginMetricSeries(t *testing.T) {
	resetKeepaliveState(t)
	body := scrape(t).Body.String()
	for _, conf := range []string{"true", "false"} {
		for _, cause := range stallCauseNames {
			for _, origin := range stallOriginNames {
				want := fmt.Sprintf(`flodni_read_stalls_total{confirmed=%q,cause=%q,origin=%q} 0`, conf, cause, origin)
				if !strings.Contains(body, want+"\n") {
					t.Errorf("series missing: %s", want)
				}
			}
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "# HELP flodni_read_stalls_total ") {
			for _, w := range append([]string{"15 s", "origin"}, stallOriginNames[:]...) {
				if !strings.Contains(line, w) {
					t.Errorf("HELP lacks %q: %s", w, line)
				}
			}
		}
	}
	keepaliveCounters.stalls[0][stallCausePlayback][stallOriginEvicted].Add(2)
	keepaliveCounters.stalls[0][stallCausePlayback][stallOriginNoPump].Add(5)
	if got := stallSum(0, stallCausePlayback); got != 7 {
		t.Errorf("sum over origin = %d, want 7", got)
	}
}
