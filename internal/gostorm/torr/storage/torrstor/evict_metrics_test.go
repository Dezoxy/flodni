package torrstor

import (
	"math"
	"sync/atomic"
	"testing"
	"time"
)

// zone classification is a pure function of the reader snapshot and the piece id.
func TestEvictViewZone(t *testing.T) {
	none := math.MaxInt
	for _, tc := range []struct {
		name string
		v    evictView
		id   int
		want int
	}{
		{"no readers", evictView{readers: 0, minPiece: none, minOpen: none}, 5, zoneNoReader},
		{"behind the only reader", evictView{readers: 1, minPiece: 20, minOpen: 20}, 19, zoneBehind},
		{"the reader's own piece is not behind", evictView{readers: 1, minPiece: 20, minOpen: 20}, 20, zoneAheadUnread},
		{"after an open-ended reader", evictView{readers: 1, minPiece: 20, minOpen: 20}, 30, zoneAheadUnread},
		{"after a bounded reader only", evictView{readers: 1, minPiece: 20, minOpen: none}, 30, zoneNoReader},
		{"behind a bounded reader", evictView{readers: 1, minPiece: 20, minOpen: none}, 3, zoneBehind},
		// Readers at 10 (open-ended) and 40 (bounded): 25 is after the first, so it is unread
		// there even though it is behind the second.
		{"after one open-ended, behind another", evictView{readers: 2, minPiece: 10, minOpen: 10}, 25, zoneAheadUnread},
		{"behind every reader", evictView{readers: 2, minPiece: 10, minOpen: 10}, 9, zoneBehind},
		// A bounded reader at 10 and an open-ended one at 40: 25 is behind the open-ended one
		// but not behind the bounded one, so it is in neither zone.
		{"between a bounded and an open-ended reader", evictView{readers: 2, minPiece: 10, minOpen: 40}, 25, zoneNoReader},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.v.zone(tc.id); got != tc.want {
				t.Errorf("zone(%d) = %s, want %s", tc.id, EvictionZoneNames[got], EvictionZoneNames[tc.want])
			}
		})
	}
}

func TestEvictionReaderBucket(t *testing.T) {
	for n, want := range map[int]int{0: 0, 1: 1, 2: 2, 7: 2} {
		if got := readerBucket(n); got != want {
			t.Errorf("readerBucket(%d) = %d, want %d", n, got, want)
		}
	}
}

// evictionDelta returns what EvictionCounts gained since the snapshot.
func evictionDelta(before [numEvictionZones][numReaderBuckets]int64) (d [numEvictionZones][numReaderBuckets]int64) {
	after := EvictionCounts()
	for z := range d {
		for b := range d[z] {
			d[z][b] = after[z][b] - before[z][b]
		}
	}
	return d
}

// scriptEviction completes pieces 10..12 and 25..28 of the 6 MiB cache; the last completion
// evicts the two oldest, pieces 10 and 11.
func scriptEviction(t *testing.T, c *Cache) {
	t.Helper()
	const mib = 1 << 20
	for i, id := range []int{10, 11, 12} {
		fetch(t, c, id, mib, int64(i+1))
		if err := c.pieces[id].MarkComplete(); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []int{25, 26, 27, 28} {
		fetch(t, c, id, mib, 100+int64(id))
		if err := c.pieces[id].MarkComplete(); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []int{10, 11} {
		if atomic.LoadInt64(&c.pieces[id].Size) != 0 {
			t.Fatalf("setup: piece %d not evicted", id)
		}
	}
}

// Two pieces behind the only (open-ended) reader are evicted: both count under behind, with
// one reader.
func TestEvictionCountedBehind(t *testing.T) {
	const mib = int64(1 << 20)
	tor, c := newMetricsTestCache(t)
	r := c.NewReader(tor.Files()[0])
	t.Cleanup(func() { c.CloseReader(r) })
	if _, err := r.Seek(20*mib, 0); err != nil {
		t.Fatal(err)
	}
	before := EvictionCounts()
	scriptEviction(t, c)
	d := evictionDelta(before)
	if d[zoneBehind][1] != 2 {
		t.Errorf("behind/1 reader = %d, want 2 (delta %v)", d[zoneBehind][1], d)
	}
	var total int64
	for z := range d {
		for b := range d[z] {
			total += d[z][b]
		}
	}
	if total != 2 {
		t.Errorf("evictions counted = %d, want 2 (delta %v)", total, d)
	}
}

// With the open-ended reader at piece 1 the evicted pieces 10 and 11 lie at or after it and
// outside its window: ahead_unread. Marked bounded, the same reader makes them no_reader.
func TestEvictionCountedAheadUnreadAndBounded(t *testing.T) {
	const mib = int64(1 << 20)
	for _, tc := range []struct {
		name    string
		bounded bool
		zone    int
	}{{"open-ended", false, zoneAheadUnread}, {"bounded", true, zoneNoReader}} {
		t.Run(tc.name, func(t *testing.T) {
			tor, c := newMetricsTestCache(t)
			r := c.NewReader(tor.Files()[0])
			t.Cleanup(func() { c.CloseReader(r) })
			if tc.bounded {
				r.SetBounded(0)
			}
			if _, err := r.Seek(1*mib, 0); err != nil {
				t.Fatal(err)
			}
			before := EvictionCounts()
			scriptEviction(t, c)
			if d := evictionDelta(before); d[tc.zone][1] != 2 {
				t.Errorf("%s/1 reader = %d, want 2 (delta %v)", EvictionZoneNames[tc.zone], d[tc.zone][1], d)
			}
		})
	}
}

// No reader at all: no_reader, readers bucket 0. Two readers: the 2+ bucket.
func TestEvictionReaderBuckets(t *testing.T) {
	const mib = int64(1 << 20)
	t.Run("none", func(t *testing.T) {
		_, c := newMetricsTestCache(t)
		before := EvictionCounts()
		scriptEviction(t, c)
		if d := evictionDelta(before); d[zoneNoReader][0] != 2 {
			t.Errorf("no_reader/0 readers = %d, want 2 (delta %v)", d[zoneNoReader][0], d)
		}
	})
	t.Run("two", func(t *testing.T) {
		tor, c := newMetricsTestCache(t)
		for _, off := range []int64{20 * mib, 30 * mib} {
			r := c.NewReader(tor.Files()[0])
			t.Cleanup(func() { c.CloseReader(r) })
			if _, err := r.Seek(off, 0); err != nil {
				t.Fatal(err)
			}
		}
		before := EvictionCounts()
		scriptEviction(t, c)
		if d := evictionDelta(before); d[zoneBehind][2] != 2 {
			t.Errorf("behind/2+ readers = %d, want 2 (delta %v)", d[zoneBehind][2], d)
		}
	})
}

// A refetch is attributed to the open-ended reader when one exists, else to a bounded one,
// else to none; the unlabelled total keeps counting every refetch.
func TestRefetchedBytesByReader(t *testing.T) {
	const mib = int64(1 << 20)
	for _, tc := range []struct {
		name    string
		readers string // "", "bounded" or "open"
		slot    int
	}{{"none", "", refetchNone}, {"bounded", "bounded", refetchBounded}, {"open_ended", "open", refetchOpenEnded}} {
		t.Run(tc.name, func(t *testing.T) {
			tor, c := newMetricsTestCache(t)
			if tc.readers != "" {
				r := c.NewReader(tor.Files()[0])
				t.Cleanup(func() { c.CloseReader(r) })
				if tc.readers == "bounded" {
					r.SetBounded(0)
				}
				if _, err := r.Seek(20*mib, 0); err != nil {
					t.Fatal(err)
				}
			}
			scriptEviction(t, c)
			total0, by0 := RefetchedBytes(), RefetchedBytesByReader()
			fetch(t, c, 10, int(mib), 200) // piece 10 was evicted: a refetch
			if got := RefetchedBytes() - total0; got != mib {
				t.Errorf("total refetched = %d, want %d", got, mib)
			}
			by := RefetchedBytesByReader()
			for i := range by {
				want := int64(0)
				if i == tc.slot {
					want = mib
				}
				if got := by[i] - by0[i]; got != want {
					t.Errorf("%s refetched = %d, want %d", RefetchReaderNames[i], got, want)
				}
			}
		})
	}
}

// Reader accounting follows a reader's whole life: open-ended at attach, bounded after
// SetBounded (once), gone at CloseReader.
func TestReaderKindCounts(t *testing.T) {
	tor, c := newMetricsTestCache(t)
	r1 := c.NewReader(tor.Files()[0])
	r2 := c.NewReader(tor.Files()[0])
	if o, b := c.nOpenEnded.Load(), c.nBounded.Load(); o != 2 || b != 0 {
		t.Fatalf("after attach open=%d bounded=%d, want 2 and 0", o, b)
	}
	r2.SetBounded(0)
	r2.SetBounded(0) // idempotent
	if o, b := c.nOpenEnded.Load(), c.nBounded.Load(); o != 1 || b != 1 {
		t.Fatalf("after SetBounded open=%d bounded=%d, want 1 and 1", o, b)
	}
	c.CloseReader(r2)
	c.CloseReader(r1)
	if o, b := c.nOpenEnded.Load(), c.nBounded.Load(); o != 0 || b != 0 {
		t.Fatalf("after close open=%d bounded=%d, want 0 and 0", o, b)
	}
}

// RefetchedSince is true only for a piece that completed a second time, at or after the
// given second: the first completion is delivery, not a refetch.
func TestRefetchedSince(t *testing.T) {
	_, c := newMetricsTestCache(t)
	complete := func(id int) {
		t.Helper()
		fetch(t, c, id, 1<<20, 1)
		if err := c.pieces[id].MarkComplete(); err != nil {
			t.Fatal(err)
		}
	}
	complete(12)
	if c.RefetchedSince(0, 39, 0) {
		t.Error("a first completion counts as a refetch")
	}
	c.pieces[12].Release() // evicted: bytes gone, the mark stays
	if c.RefetchedSince(0, 39, 0) {
		t.Error("an eviction alone counts as a refetch")
	}
	before := time.Now().Unix()
	complete(12) // the second completion
	if !c.RefetchedSince(10, 14, before) || !c.RefetchedSince(12, 12, 0) {
		t.Error("a piece completed twice is not reported")
	}
	if c.RefetchedSince(10, 14, time.Now().Unix()+5) {
		t.Error("a refetch before the read began is reported")
	}
	if c.RefetchedSince(13, 20, 0) || c.RefetchedSince(0, 11, 0) {
		t.Error("a span without piece 12 is reported")
	}
	var nilCache *Cache
	if nilCache.RefetchedSince(0, 1, 0) {
		t.Error("nil cache reports a refetch")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if c.RefetchedSince(12, 12, 0) {
		t.Error("closed cache reports a refetch")
	}
}
