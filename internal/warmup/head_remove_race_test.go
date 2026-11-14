package warmup

import (
	"bytes"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// A removal that lands between a head write and the next chunk leaves the size cache
// ahead of the disk. The next chunk must not recreate the file at its own offset: the
// bytes below it would read back as zeros while GetAvailableRange reports them covered.
func TestHeadChunkAfterRemovalDoesNotCreateHole(t *testing.T) {
	d := indexCache(t, t.TempDir())
	const chunk = 64 << 10
	path := d.filePath("h", 1)

	d.processWrite("h", 1, bytes.Repeat([]byte{1}, chunk), 0)
	if got := d.GetAvailableRange("h", 1); got != chunk {
		t.Fatalf("setup: range=%d want %d", got, chunk)
	}

	// Reproduce what a removal interleaved with processWrite used to leave behind (a
	// removal landing after WriteAt but before the bookkeeping): the file is gone and
	// its handle closed, but a size entry for it survives.
	d.closeHandle(path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	d.sizeCache.Store(path, sizeEntry{size: chunk, updatedAt: time.Now()})
	d.missing.Delete(path)

	d.processWrite("h", 1, bytes.Repeat([]byte{2}, chunk), chunk)

	if fi, err := os.Stat(path); err == nil {
		t.Fatalf("file recreated at off>0 with size %d: bytes [0,%d) are a hole", fi.Size(), chunk)
	}
	if got := d.GetAvailableRange("h", 1); got != 0 {
		t.Fatalf("GetAvailableRange=%d, want 0 for a removed head", got)
	}
}

// A chunk past offset 0 for a missing head is dropped before it can create anything,
// so it must not evict other files to make room for a file it will never create.
func TestMissingHeadChunkPastZeroDoesNotEvict(t *testing.T) {
	oldGB := diskQuotaGB
	t.Cleanup(func() { diskQuotaGB = oldGB })
	diskQuotaGB = 1
	countApparent(t) // the filler is a sparse file that only has to count as full
	const quota = int64(1) << 30

	d := indexCache(t, t.TempDir())
	oldPath := d.filePath("old", 1)
	f, err := os.Create(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(quota - 256<<10); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldPath, past, past); err != nil {
		t.Fatal(err)
	}
	atomic.StoreInt64(&d.totalSize, quota-256<<10)

	d.processWrite("new", 1, bytes.Repeat([]byte{1}, 64<<10), 64<<10)

	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("a dropped chunk evicted the oldest file: %v", err)
	}
	if _, err := os.Stat(d.filePath("new", 1)); !os.IsNotExist(err) {
		t.Fatalf("chunk at off>0 created a file: %v", err)
	}
}

// OnWarmupStateChange may call back into the cache, so it must run with tailMu free.
func TestStateCallbacksRunOutsideTailMu(t *testing.T) {
	d := indexCache(t, t.TempDir())
	var calls, states []bool
	old := OnWarmupStateChange
	t.Cleanup(func() { OnWarmupStateChange = old })
	OnWarmupStateChange = func(_ string, _ int, active bool) {
		free := d.tailMu.TryLock()
		if free {
			d.tailMu.Unlock()
		}
		calls = append(calls, free)
		states = append(states, active)
	}

	d.processWrite("h", 1, bytes.Repeat([]byte{1}, int(FileSize)), 0)

	// One chunk of FileSize starts and completes the warmup: two callbacks.
	if len(calls) != 2 || !calls[0] || !calls[1] {
		t.Fatalf("tailMu free during callbacks (start, complete) = %v, want [true true]", calls)
	}
	if !states[0] || states[1] {
		t.Fatalf("callback states = %v, want [true false]", states)
	}
}

// A stale size entry for a vanished head must not survive the file's re-creation at
// offset 0. The create stores the stale size (and skips the quota accounting), so a
// later out-of-order chunk at an offset the new file never reached passes the density
// check and leaves a hole that GetAvailableRange reports as covered.
func TestHeadCreateAtZeroOverStaleSizeLeavesNoHole(t *testing.T) {
	d := indexCache(t, t.TempDir())
	const chunk = 64 << 10
	const stale = 4 * chunk
	path := d.filePath("h", 1)

	d.sizeCache.Store(path, sizeEntry{size: stale, updatedAt: time.Now()})

	d.processWrite("h", 1, bytes.Repeat([]byte{1}, chunk), 0)
	if got := d.GetAvailableRange("h", 1); got != chunk {
		t.Fatalf("range after create = %d, want %d (stale entry leaked)", got, chunk)
	}
	if got := d.TrackedBytes(); got != chunk {
		t.Fatalf("tracked = %d, want %d", got, chunk)
	}

	// Out of order: starts past the end of what the file really holds.
	d.processWrite("h", 1, bytes.Repeat([]byte{2}, chunk), 2*chunk)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != chunk {
		t.Fatalf("file size %d, want %d: out-of-order chunk left a hole", fi.Size(), chunk)
	}
	if got := d.GetAvailableRange("h", 1); got != chunk {
		t.Fatalf("range = %d, want %d", got, chunk)
	}
}
