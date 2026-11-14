package warmup

import (
	"bytes"
	"errors"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The quota counts allocated blocks, and every change to totalSize happens under tailMu.

// requireSparse skips a test on a filesystem that stores holes as data.
func requireSparse(t *testing.T, path string, apparent int64) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	got := allocatedBytes(fi)
	if got >= apparent/2 {
		t.Skipf("filesystem does not keep holes: %d allocated of %d", got, apparent)
	}
	return got
}

// blocked reports whether fn is still waiting after a grace period while tailMu is held,
// and finishes it by releasing the lock.
func blocked(t *testing.T, d *DiskWarmupCache, fn func()) bool {
	t.Helper()
	d.tailMu.Lock()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	var finishedEarly bool
	select {
	case <-done:
		finishedEarly = true
	case <-time.After(200 * time.Millisecond):
	}
	d.tailMu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("did not finish after tailMu was released")
	}
	return !finishedEarly
}

func TestSparseTailCountsItsAllocatedBlocks(t *testing.T) {
	d := indexCache(t, t.TempDir())
	TailWarmupSize = 32 << 20 // restored by indexCache's cleanup
	const fileSize = 256 << 20
	tailStart := TailStart(fileSize)
	// One block written at the far end of the window: the file is as long as the window
	// and holds one block.
	d.writeTail("h", 1, bytes.Repeat([]byte{1}, 64<<10), fileSize-64<<10, fileSize)
	path := d.tailPath("h", 1)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() < fileSize-tailStart-1<<10 {
		t.Fatalf("test setup: tail file is %d bytes, want the window length", fi.Size())
	}
	requireSparse(t, path, fi.Size())
	// Counted at least at the bytes written, and nowhere near the apparent size: the
	// allocated blocks are never compared with a second stat, which may lag the first.
	if got := tailTotal(d); got < 64<<10 || got >= fi.Size()/2 {
		t.Fatalf("total = %d, want at least the 64 KiB written and well below the apparent %d", got, fi.Size())
	}
	d.RemoveHash("h")
	if got := tailTotal(d); got != 0 {
		t.Fatalf("total after removal = %d, want 0", got)
	}
}

func TestSparseHeadCountsItsAllocatedBlocksOnScanAndWalk(t *testing.T) {
	dir := t.TempDir()
	d := indexCache(t, dir)
	const apparent = 2 << 30
	path := d.filePath("sp", 1)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{1}, 64<<10), apparent-64<<10); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	alloc := requireSparse(t, path, apparent)

	total, _, _, err := d.scanExisting(false)
	if err != nil {
		t.Fatal(err)
	}
	if total != alloc {
		t.Fatalf("start-up walk = %d, want the allocated size %d", total, alloc)
	}

	// The eviction walk counts the same: a 2 GiB sparse file is not over a 1 GiB quota.
	oldGB := diskQuotaGB
	t.Cleanup(func() { diskQuotaGB = oldGB })
	diskQuotaGB = 1
	atomic.StoreInt64(&d.totalSize, apparent) // what the apparent-size accounting believed
	d.mu.Lock()
	evicted, room := d.enforceQuotaLocked(1<<20, evictOpts{})
	d.mu.Unlock()
	if len(evicted) != 0 || !room {
		t.Fatalf("evicted %d, room %v: the file holds %d bytes", len(evicted), room, alloc)
	}
	if got := tailTotal(d); got != alloc {
		t.Fatalf("walk total = %d, want %d", got, alloc)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("sparse head was evicted: %v", err)
	}
}

func TestQuotaWalkHoldsTailMu(t *testing.T) {
	d := indexCache(t, t.TempDir())
	if !blocked(t, d, func() { d.EvictTo(1 << 40) }) {
		t.Fatal("the quota walk stored the total without tailMu")
	}
}

func TestDropResidueHoldsTailMu(t *testing.T) {
	d := indexCache(t, t.TempDir())
	path := d.filePath("r", 1)
	if err := os.WriteFile(path, bytes.Repeat([]byte{1}, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	d.tailMu.Lock()
	d.credit(path, allocatedBytes(fi))
	d.tailMu.Unlock()
	if !blocked(t, d, func() { d.dropResidue(path) }) {
		t.Fatal("dropResidue unlinked and subtracted without tailMu")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("residue still there: %v", err)
	}
	if got := tailTotal(d); got != 0 {
		t.Fatalf("total after the drop = %d, want 0", got)
	}
}

func corruptHead(t *testing.T, d *DiskWarmupCache) string {
	t.Helper()
	path := d.filePath("cccccccc", 1)
	if err := os.WriteFile(path, make([]byte, FileSize+(16<<20)+(1<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	d.tailMu.Lock()
	d.credit(path, allocatedBytes(fi))
	d.tailMu.Unlock()
	return path
}

func TestCorruptRemovalSubtractsTheFile(t *testing.T) {
	d := indexCache(t, t.TempDir())
	path := corruptHead(t, d)
	if got := d.GetAvailableRange("cccccccc", 1); got != 0 {
		t.Fatalf("range of a corrupt head = %d, want 0", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("corrupt head still there: %v", err)
	}
	if got := tailTotal(d); got != 0 {
		t.Fatalf("total after the corrupt removal = %d, want 0", got)
	}
}

func TestCorruptRemovalHoldsTailMu(t *testing.T) {
	d := indexCache(t, t.TempDir())
	corruptHead(t, d)
	if !blocked(t, d, func() { d.GetAvailableRange("cccccccc", 1) }) {
		t.Fatal("the corrupt removal changed the total without tailMu")
	}
}

func TestHeadCreateClosesTheHandleItReplaces(t *testing.T) {
	d := indexCache(t, t.TempDir())
	path := d.filePath("h", 1)
	stale, err := os.Create(d.dir + "/stale")
	if err != nil {
		t.Fatal(err)
	}
	old := &cachedHandle{f: stale}
	d.handles.Store(path, old) // a handle for a file that no longer exists

	d.writeHeadLocked(path, 0, bytes.Repeat([]byte{1}, 8192))

	if !old.closed.Load() {
		t.Fatal("the replaced handle was not marked closed")
	}
	if _, err := stale.Write([]byte{1}); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("the replaced descriptor is still open: %v", err)
	}
	if v, ok := d.handles.Load(path); !ok || v.(*cachedHandle) == old {
		t.Fatal("the new handle is not the cached one")
	}
}

// A filesystem whose st_blocks lags the write (ZFS within a txg) must not make the quota
// count a fresh write as free: every credit is floored at the bytes known written.
func TestLaggingBlocksDoNotUnderCount(t *testing.T) {
	old := allocatedBytes
	allocatedBytes = func(os.FileInfo) int64 { return 0 }
	t.Cleanup(func() { allocatedBytes = old })

	d := indexCache(t, t.TempDir())
	const fileSize = 8 << 20
	d.writeTail("h", 1, bytes.Repeat([]byte{1}, 256<<10), fileSize-1<<20, fileSize)
	if got := tailTotal(d); got != 256<<10 {
		t.Fatalf("tail total = %d, want the 256 KiB written", got)
	}
	d.processWrite("h", 2, bytes.Repeat([]byte{1}, 64<<10), 0)
	if got := tailTotal(d); got != 256<<10+64<<10 {
		t.Fatalf("total with a head = %d, want %d", got, 256<<10+64<<10)
	}
	d.RemoveHash("h")
	if got := tailTotal(d); got != 0 {
		t.Fatalf("total after removal = %d, want 0", got)
	}
}

// RemoveHash is reachable from the engine API while the start-up walk runs: the walk
// holds tailMu, or -race reports the ledger written by both.
func TestStartupScanHoldsTailMuAgainstRemoveHash(t *testing.T) {
	dir := t.TempDir()
	d := indexCache(t, dir)
	for i := 0; i < 200; i++ {
		p := d.filePath("h"+string(rune('a'+i%26)), i)
		if err := os.WriteFile(p, bytes.Repeat([]byte{1}, 8192), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				for c := 0; c < 26; c++ {
					d.RemoveHash("h" + string(rune('a'+c)))
				}
			}
		}
	}()
	if _, _, _, err := d.scanAtStart(false); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()

	if !blocked(t, d, func() { _, _, _, _ = d.scanAtStart(false) }) {
		t.Fatal("the start-up scan ran without tailMu")
	}
}

// An unlink that fails leaves the file and its bytes counted.
func TestRemoveHashKeepsBytesOfAFileItCouldNotUnlink(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory the process cannot write to")
	}
	dir := t.TempDir()
	d := indexCache(t, dir)
	path := d.filePath("h", 1)
	if err := os.WriteFile(path, bytes.Repeat([]byte{1}, 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	d.tailMu.Lock()
	d.credit(path, allocatedBytes(fi))
	d.tailMu.Unlock()
	before := tailTotal(d)

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	d.RemoveHash("h")
	if got := tailTotal(d); got != before {
		t.Fatalf("total = %d after a failed unlink, want %d", got, before)
	}
	_ = os.Chmod(dir, 0o755)
	d.RemoveHash("h")
	if got := tailTotal(d); got != 0 {
		t.Fatalf("total after the real removal = %d, want 0", got)
	}
}
