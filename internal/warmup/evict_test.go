package warmup

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// evictEnv is a 1 GiB quota and a cache dir; sizes below are chosen so the quota check
// needs a known number of victims.
func evictEnv(t *testing.T) *DiskWarmupCache {
	t.Helper()
	oldGB, oldPlayed, oldOn := diskQuotaGB, IsPlayed, OnEvicted
	t.Cleanup(func() { diskQuotaGB, IsPlayed, OnEvicted = oldGB, oldPlayed, oldOn })
	diskQuotaGB = 1
	IsPlayed, OnEvicted = nil, nil
	countApparent(t) // its tests fake a large cache with sparse files
	return indexCache(t, t.TempDir())
}

const (
	mb    = int64(1) << 20
	quota = int64(1) << 30
)

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// setTotal syncs the running total with the files, as the quota check expects.
func setTotal(d *DiskWarmupCache, n int64) { atomic.StoreInt64(&d.totalSize, n) }

func TestEvictionOrder(t *testing.T) {
	tests := []struct {
		name   string
		played map[FileKey]bool
		gone   []string // stems that must be evicted
		stays  []string
		needed int64
	}{
		{
			name: "no marks: oldest first, as before",
			gone: []string{"a-1"}, stays: []string{"b-1", "c-1"},
		},
		{
			name:   "a newer never-played file goes before an older played one",
			played: map[FileKey]bool{{"a", 1}: true},
			gone:   []string{"b-1"}, stays: []string{"a-1", "c-1"},
		},
		{
			name:   "only played files left: the oldest goes",
			played: map[FileKey]bool{{"a", 1}: true, {"b", 1}: true, {"c", 1}: true},
			gone:   []string{"a-1"}, stays: []string{"b-1", "c-1"},
		},
		{
			name:   "never-played class is exhausted before a played file is touched",
			played: map[FileKey]bool{{"a", 1}: true},
			needed: 300 * mb, // 476 MiB over the quota: two victims
			gone:   []string{"b-1", "c-1"}, stays: []string{"a-1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := evictEnv(t)
			// Three files of 400 MiB: head 300 + tail 100 each, ages 3h, 2h, 1h.
			for i, stem := range []string{"a-1", "b-1", "c-1"} {
				age := time.Duration(3-i) * time.Hour
				sparseFile(t, d.dir+"/"+stem+warmupSuffix, 300*mb, age)
				sparseFile(t, d.dir+"/"+stem+tailSuffix, 100*mb, age)
			}
			setTotal(d, 1200*mb)
			IsPlayed = func(k FileKey) bool { return tt.played[k] }
			d.mu.Lock()
			ev, room := d.enforceQuotaLocked(tt.needed, evictOpts{trigger: "test"})
			d.mu.Unlock()
			if !room {
				t.Fatal("no room after eviction")
			}
			if len(ev) != len(tt.gone) {
				t.Fatalf("evicted %d files, want %d: %+v", len(ev), len(tt.gone), ev)
			}
			for _, s := range tt.gone {
				if exists(d.dir+"/"+s+warmupSuffix) || exists(d.dir+"/"+s+tailSuffix) {
					t.Errorf("%s survived; head and tail must go together", s)
				}
			}
			for _, s := range tt.stays {
				if !exists(d.dir+"/"+s+warmupSuffix) || !exists(d.dir+"/"+s+tailSuffix) {
					t.Errorf("%s was evicted", s)
				}
			}
		})
	}
}

func TestEvictionRecordIsPerFileAndCountersPerWindow(t *testing.T) {
	d := evictEnv(t)
	sparseFile(t, d.filePath("old", 3), 300*mb, 2*time.Hour)
	sparseFile(t, d.tailPath("old", 3), 100*mb, 3*time.Hour) // older tail: the unit is as new as its head
	sparseFile(t, d.filePath("new", 1), 700*mb, time.Minute)
	setTotal(d, 1100*mb)
	IsPlayed = func(FileKey) bool { return true } // both played: the older goes

	before := CounterStats()
	d.mu.Lock()
	ev, room := d.enforceQuotaLocked(0, evictOpts{trigger: TriggerHeadCreate})
	d.mu.Unlock()
	after := CounterStats()

	if !room || len(ev) != 1 {
		t.Fatalf("room=%v evicted=%+v", room, ev)
	}
	e := ev[0]
	if e.Key != (FileKey{"old", 3}) || e.HeadBytes != 300*mb || e.TailBytes != 100*mb ||
		!e.Played || e.Trigger != TriggerHeadCreate || e.Age < 119*time.Minute || e.Age > 121*time.Minute {
		t.Fatalf("record = %+v", e)
	}
	if after.EvictedHeads-before.EvictedHeads != 1 || after.EvictedTails-before.EvictedTails != 1 {
		t.Fatal("each window counter must rise by one")
	}
	if after.EvictedHeadBytes-before.EvictedHeadBytes != 300*mb || after.EvictedTailBytes-before.EvictedTailBytes != 100*mb {
		t.Fatal("window bytes wrong")
	}
}

func TestTheFileBeingCreatedIsNeverAVictim(t *testing.T) {
	d := evictEnv(t)
	// Its tail is the oldest never-played unit, and the quota is over.
	sparseFile(t, d.tailPath("mine", 1), 600*mb, 5*time.Hour)
	sparseFile(t, d.filePath("other", 1), 500*mb, time.Hour)
	setTotal(d, 1100*mb)

	d.mu.Lock()
	ev, _ := d.enforceQuotaLocked(0, evictOpts{exclude: FileKey{"mine", 1}})
	d.mu.Unlock()
	if !exists(d.tailPath("mine", 1)) {
		t.Fatal("the file being created lost its tail to its own head")
	}
	if len(ev) != 1 || ev[0].Key.Hash != "other" {
		t.Fatalf("evicted %+v", ev)
	}
}

func TestEvictNeverPlayedPolicy(t *testing.T) {
	t.Run("not enough never-played bytes: nothing is evicted", func(t *testing.T) {
		d := evictEnv(t)
		sparseFile(t, d.filePath("np", 1), 100*mb, 2*time.Hour)
		sparseFile(t, d.filePath("pl", 1), 1100*mb, time.Hour)
		setTotal(d, 1200*mb) // 176 MiB over; only 100 MiB are never played
		IsPlayed = func(k FileKey) bool { return k.Hash == "pl" }

		d.mu.Lock()
		ev, room := d.enforceQuotaLocked(0, evictOpts{policy: EvictNeverPlayed})
		d.mu.Unlock()
		if room || len(ev) != 0 || !exists(d.filePath("np", 1)) || !exists(d.filePath("pl", 1)) {
			t.Fatalf("room=%v evicted=%+v", room, ev)
		}
	})
	t.Run("enough: only never-played files go", func(t *testing.T) {
		d := evictEnv(t)
		sparseFile(t, d.filePath("np1", 1), 400*mb, 2*time.Hour)
		sparseFile(t, d.filePath("np2", 1), 300*mb, 3*time.Hour)
		sparseFile(t, d.filePath("pl", 1), 500*mb, 4*time.Hour) // oldest of all, but played
		setTotal(d, 1200*mb)
		IsPlayed = func(k FileKey) bool { return k.Hash == "pl" }

		d.mu.Lock()
		ev, room := d.enforceQuotaLocked(0, evictOpts{policy: EvictNeverPlayed})
		d.mu.Unlock()
		if !room || len(ev) != 1 || ev[0].Key.Hash != "np2" || !exists(d.filePath("pl", 1)) {
			t.Fatalf("room=%v evicted=%+v", room, ev)
		}
	})
}

func TestEvictNoneRemovesNothingOverQuota(t *testing.T) {
	d := evictEnv(t)
	sparseFile(t, d.filePath("a", 1), 1100*mb, time.Hour)
	setTotal(d, 1100*mb)
	d.mu.Lock()
	ev, room := d.enforceQuotaLocked(0, evictOpts{policy: EvictNone})
	d.mu.Unlock()
	if room || len(ev) != 0 || !exists(d.filePath("a", 1)) {
		t.Fatalf("room=%v evicted=%+v", room, ev)
	}
	if got := d.TrackedBytes(); got != 1100*mb {
		t.Fatalf("the walk must still resync the total, got %d", got)
	}
}

func TestOnEvictedRunsOnceOutsideBothLocks(t *testing.T) {
	d := evictEnv(t)
	sparseFile(t, d.filePath("a", 1), 600*mb, 2*time.Hour)
	sparseFile(t, d.filePath("b", 1), 600*mb, time.Hour)
	setTotal(d, 1200*mb)
	var calls int
	var muFree, tailFree bool
	OnEvicted = func(evs []Eviction) {
		calls++
		if muFree = d.mu.TryLock(); muFree {
			d.mu.Unlock()
		}
		if tailFree = d.tailMu.TryLock(); tailFree {
			d.tailMu.Unlock()
		}
		if len(evs) != 1 {
			t.Errorf("batch of %d, want 1", len(evs))
		}
	}
	// A chunk at offset 0 for a new file triggers the eviction, as in production.
	d.processWrite("new", 1, make([]byte, 64<<10), 0)
	if calls != 1 || !muFree || !tailFree {
		t.Fatalf("calls=%d d.mu free=%v tailMu free=%v", calls, muFree, tailFree)
	}
}

func TestRemoveHashCallsOnHashRemovedAfterTheLock(t *testing.T) {
	d := evictEnv(t)
	old := OnHashRemoved
	t.Cleanup(func() { OnHashRemoved = old })
	var got string
	var free bool
	OnHashRemoved = func(h string) {
		got = h
		if free = d.tailMu.TryLock(); free {
			d.tailMu.Unlock()
		}
	}
	d.RemoveHash("zz")
	if got != "zz" || !free {
		t.Fatalf("hook got %q, tailMu free=%v", got, free)
	}
}

func TestSplitCacheName(t *testing.T) {
	for stem, want := range map[string]FileKey{
		"abc-12":     {"abc", 12},
		"a-b-3":      {"a-b", 3},
		"nodash":     {"nodash", -1},
		"abc-notnum": {"abc-notnum", -1},
	} {
		if got := splitCacheName(stem); got != want {
			t.Errorf("%q = %+v, want %+v", stem, got, want)
		}
	}
}

// requestEnv is a cache of three never-played 300 MiB files, a oldest, c newest.
func requestEnv(t *testing.T) *DiskWarmupCache {
	t.Helper()
	d := evictEnv(t)
	sparseFile(t, d.filePath("a", 1), 300*mb, 3*time.Hour)
	sparseFile(t, d.filePath("b", 1), 300*mb, 2*time.Hour)
	sparseFile(t, d.filePath("c", 1), 300*mb, time.Hour)
	setTotal(d, 900*mb)
	return d
}

func TestEvictToGoesDownToTheTargetOldestFirst(t *testing.T) {
	d := requestEnv(t)
	res := d.EvictTo(400 * mb)
	if res.Files != 2 || res.Bytes != 600*mb || res.TotalAfter != 300*mb {
		t.Fatalf("result = %+v", res)
	}
	if exists(d.filePath("a", 1)) || exists(d.filePath("b", 1)) || !exists(d.filePath("c", 1)) {
		t.Fatal("the two oldest go, the newest stays")
	}
}

func TestEvictToTakesPlayedFilesOnlyAfterNeverPlayed(t *testing.T) {
	d := requestEnv(t)
	IsPlayed = func(k FileKey) bool { return k.Hash == "a" } // the oldest is played
	res := d.EvictTo(300 * mb)
	if res.Files != 2 || !exists(d.filePath("a", 1)) || exists(d.filePath("b", 1)) || exists(d.filePath("c", 1)) {
		t.Fatalf("never-played b and c go before the older played a: %+v", res)
	}
	res = d.EvictTo(0)
	if res.Files != 1 || res.TotalAfter != 0 || exists(d.filePath("a", 1)) {
		t.Fatalf("a target of 0 takes the played file last: %+v", res)
	}
}

func TestEvictToAboveTheTotalEvictsNothing(t *testing.T) {
	d := requestEnv(t)
	res := d.EvictTo(900 * mb)
	if res.Files != 0 || res.Bytes != 0 || res.TotalAfter != 900*mb {
		t.Fatalf("result = %+v", res)
	}
	if !exists(d.filePath("a", 1)) || !exists(d.filePath("b", 1)) || !exists(d.filePath("c", 1)) {
		t.Fatal("nothing may be removed")
	}
}

func TestEvictToWalksTheDiskWhenTheRunningTotalIsLow(t *testing.T) {
	d := requestEnv(t)
	setTotal(d, 0) // drifted far under the disk
	res := d.EvictTo(600 * mb)
	if res.Files != 1 || res.TotalAfter != 600*mb || exists(d.filePath("a", 1)) {
		t.Fatalf("a request trusts the disk, not the total: %+v", res)
	}
}

func TestEvictToRecordsLikeAQuotaEviction(t *testing.T) {
	d := requestEnv(t)
	sparseFile(t, d.tailPath("a", 1), 100*mb, 3*time.Hour)
	setTotal(d, 1000*mb)
	var got []Eviction
	OnEvicted = func(evs []Eviction) { got = append(got, evs...) }
	var logged strings.Builder
	oldOut := evictLog.Writer()
	evictLog.SetOutput(&logged)
	t.Cleanup(func() { evictLog.SetOutput(oldOut) })

	before := CounterStats()
	d.EvictTo(600 * mb)
	after := CounterStats()

	if len(got) != 1 || got[0].Key != (FileKey{"a", 1}) || got[0].Trigger != TriggerRequest ||
		got[0].HeadBytes != 300*mb || got[0].TailBytes != 100*mb || got[0].Played {
		t.Fatalf("OnEvicted got %+v", got)
	}
	if !strings.Contains(logged.String(), `"trigger":"request"`) || !strings.Contains(logged.String(), `"hash":"a"`) {
		t.Fatalf("log line = %q", logged.String())
	}
	if after.EvictedHeads-before.EvictedHeads != 1 || after.EvictedTails-before.EvictedTails != 1 ||
		after.EvictedHeadBytes-before.EvictedHeadBytes != 300*mb {
		t.Fatal("the eviction counters must move as for a quota eviction")
	}
}

func TestEvictToOnANilCacheDoesNothing(t *testing.T) {
	var d *DiskWarmupCache
	if res := d.EvictTo(0); res != (EvictResult{}) {
		t.Fatalf("result = %+v", res)
	}
}
