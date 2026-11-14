package warmup

import (
	"bytes"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// countApparent makes the quota count a file's length, so a test can fake a large cache
// with sparse files. The tests of allocated-block accounting leave it off.
func countApparent(t *testing.T) {
	t.Helper()
	old := allocatedBytes
	allocatedBytes = func(fi os.FileInfo) int64 { return fi.Size() }
	t.Cleanup(func() { allocatedBytes = old })
}

// sparseFile makes a file of the given apparent size and age, and has the quota count
// apparent sizes for the rest of the test.
func sparseFile(t *testing.T, path string, size int64, age time.Duration) {
	t.Helper()
	countApparent(t)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	past := time.Now().Add(-age)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
}

// Quota eviction moves the counters by window; RemoveHash does not.
func TestEvictionCountersByWindow(t *testing.T) {
	oldGB := diskQuotaGB
	t.Cleanup(func() { diskQuotaGB = oldGB })
	diskQuotaGB = 1
	const quota = int64(1) << 30

	d := indexCache(t, t.TempDir())
	const oldHead, oldTail = int64(40 << 20), int64(40 << 20)
	const bigSize = quota - 10<<20
	sparseFile(t, d.filePath("oldh", 1), oldHead, 2*time.Hour)
	sparseFile(t, d.tailPath("oldt", 1), oldTail, time.Hour)
	// Evicting the oldest file alone leaves the total over the quota; the second ends it.
	sparseFile(t, d.filePath("big", 1), bigSize, time.Minute)
	atomic.StoreInt64(&d.totalSize, bigSize+oldHead+oldTail)

	before := CounterStats()
	d.mu.Lock()
	d.enforceQuotaLocked(0, evictOpts{})
	d.mu.Unlock()
	after := CounterStats()

	if got := after.EvictedHeads - before.EvictedHeads; got != 1 {
		t.Fatalf("evicted heads +%d, want 1", got)
	}
	if got := after.EvictedHeadBytes - before.EvictedHeadBytes; got != oldHead {
		t.Fatalf("evicted head bytes +%d, want %d", got, oldHead)
	}
	if got := after.EvictedTails - before.EvictedTails; got != 1 {
		t.Fatalf("evicted tails +%d, want 1", got)
	}
	if got := after.EvictedTailBytes - before.EvictedTailBytes; got != oldTail {
		t.Fatalf("evicted tail bytes +%d, want %d", got, oldTail)
	}
	if after.QuotaBytes != quota {
		t.Fatalf("quota = %d, want %d", after.QuotaBytes, quota)
	}
	if got := d.TrackedBytes(); got != bigSize {
		t.Fatalf("tracked = %d after eviction", got)
	}

	d.writeTail("rm", 1, bytes.Repeat([]byte{1}, 128<<10), 7<<20, 8<<20)
	mid := CounterStats()
	d.RemoveHash("rm")
	if end := CounterStats(); end != mid {
		t.Fatalf("RemoveHash moved the eviction counters: %+v -> %+v", mid, end)
	}
}

// A chunk that finds writeCh full is dropped and counted.
func TestWriteQueueDropIsCounted(t *testing.T) {
	d := &DiskWarmupCache{dir: t.TempDir(), writeCh: make(chan warmupWrite)} // unbuffered, no worker: always full
	before := CounterStats().WriteQueueDrops
	d.writeChunk("h", 1, []byte("x"), 0)
	if got := CounterStats().WriteQueueDrops - before; got != 1 {
		t.Fatalf("drops +%d, want 1", got)
	}
}

func TestTrackedBytesNilReceiver(t *testing.T) {
	var d *DiskWarmupCache
	if d.TrackedBytes() != 0 {
		t.Fatal("nil cache must report 0")
	}
}
