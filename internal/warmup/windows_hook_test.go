package warmup

import (
	"bytes"
	"sync"
	"testing"
)

// Dezoxy fork: WindowsComplete and the OnWindowsCached hook (Readiness step 2, warm on
// add, the mount-path half): the cache tells the daemon when a file's two windows are
// complete, whoever wrote them.

// hookRecorder installs OnWindowsCached for the test and records its calls.
type hookRecorder struct {
	mu    sync.Mutex
	calls []FileKey
	sizes []int64
	check func() // runs inside every call
}

func recordHook(t *testing.T) *hookRecorder {
	t.Helper()
	r := &hookRecorder{}
	old := OnWindowsCached
	t.Cleanup(func() { OnWindowsCached = old })
	OnWindowsCached = func(k FileKey, size int64) {
		if r.check != nil {
			r.check()
		}
		r.mu.Lock()
		r.calls = append(r.calls, k)
		r.sizes = append(r.sizes, size)
		r.mu.Unlock()
	}
	return r
}

func (r *hookRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func TestWindowsCompleteShortFile(t *testing.T) {
	d := indexCache(t, t.TempDir())
	const fileSize = 300 << 10 // shorter than both windows
	k := FileKey{"s", 2}
	if ok, _ := d.WindowsComplete(k); ok {
		t.Fatal("an unknown file reported complete")
	}
	d.processWrite("s", 2, bytes.Repeat([]byte{1}, fileSize), 0)
	if ok, _ := d.WindowsComplete(k); ok {
		t.Fatal("a head with no tail reported complete")
	}
	d.writeTail("s", 2, bytes.Repeat([]byte{2}, fileSize), 0, fileSize)
	if ok, size := d.WindowsComplete(k); !ok || size != fileSize {
		t.Fatalf("short file: complete=%v size=%d", ok, size)
	}
}

func TestWindowsCompleteNeedsBothWindows(t *testing.T) {
	d := indexCache(t, t.TempDir())
	const fileSize = 10 << 20
	k := FileKey{"h", 1}
	d.writeTail("h", 1, bytes.Repeat([]byte{2}, 1<<20), fileSize-1<<20, fileSize)
	if ok, _ := d.WindowsComplete(k); ok {
		t.Fatal("a tail with no head reported complete")
	}
	d.processWrite("h", 1, bytes.Repeat([]byte{1}, 512<<10), 0)
	if ok, _ := d.WindowsComplete(k); ok {
		t.Fatal("half a head reported complete")
	}
	d.processWrite("h", 1, bytes.Repeat([]byte{1}, 512<<10), 512<<10)
	if ok, size := d.WindowsComplete(k); !ok || size != fileSize {
		t.Fatalf("complete=%v size=%d", ok, size)
	}
	var nilCache *DiskWarmupCache
	if ok, _ := nilCache.WindowsComplete(k); ok {
		t.Fatal("a nil cache reported complete")
	}
}

// The file size survives a restart through the cover file, so a restarted daemon still
// knows the head's target.
func TestWindowsCompleteAfterRestart(t *testing.T) {
	dir := t.TempDir()
	const fileSize = 10 << 20
	d := indexCache(t, dir)
	d.processWrite("h", 1, bytes.Repeat([]byte{1}, 1<<20), 0)
	d.writeTail("h", 1, bytes.Repeat([]byte{2}, 1<<20), fileSize-1<<20, fileSize)
	restarted := &DiskWarmupCache{dir: dir}
	if _, _, _, err := restarted.scanExisting(false); err != nil {
		t.Fatal(err)
	}
	if ok, size := restarted.WindowsComplete(FileKey{"h", 1}); !ok || size != fileSize {
		t.Fatalf("after restart: complete=%v size=%d", ok, size)
	}
}

// Head then tail: the hook fires once, at the tail write that completes the pair.
func TestHookFiresOnceFromWriteChunkAndWriteTail(t *testing.T) {
	d := indexCache(t, t.TempDir())
	rec := recordHook(t)
	const fileSize = 10 << 20
	d.WriteChunk("h", 1, bytes.Repeat([]byte{1}, 1<<20), 0)
	waitFor(t, "head write", func() bool { return d.GetAvailableRange("h", 1) >= 1<<20 })
	// The head is on disk before the worker runs its noteWrite for it. Queue a second
	// file's head and wait for it: the single worker has then finished the first write
	// entirely, so it cannot fire the hook concurrently with the tail write below (a
	// double fire, and a race on rec.calls and OnWindowsCached).
	d.WriteChunk("drain", 1, bytes.Repeat([]byte{1}, 1<<20), 0)
	waitFor(t, "worker drain", func() bool { return d.GetAvailableRange("drain", 1) >= 1<<20 })
	if rec.count() != 0 {
		t.Fatalf("hook fired with the tail missing: %d", rec.count())
	}
	d.WriteTail("h", 1, bytes.Repeat([]byte{2}, 1<<20), fileSize-1<<20, fileSize)
	if rec.count() != 1 || rec.calls[0] != (FileKey{"h", 1}) || rec.sizes[0] != fileSize {
		t.Fatalf("calls = %v sizes = %v", rec.calls, rec.sizes)
	}
}

// Tail then head: the write worker fires it, after the head write.
func TestHookFiresFromTheWriteWorkerWhenTheHeadCompletesLast(t *testing.T) {
	d := indexCache(t, t.TempDir())
	rec := recordHook(t)
	const fileSize = 10 << 20
	d.WriteTail("h", 1, bytes.Repeat([]byte{2}, 1<<20), fileSize-1<<20, fileSize)
	if rec.count() != 0 {
		t.Fatalf("hook fired with the head missing: %d", rec.count())
	}
	d.WriteChunk("h", 1, bytes.Repeat([]byte{1}, 1<<20), 0)
	waitFor(t, "hook", func() bool { return rec.count() == 1 })
}

// The hook runs with neither d.mu nor tailMu held, from each of the three write sites.
func TestHookRunsWithNoCacheLockHeld(t *testing.T) {
	d := indexCache(t, t.TempDir())
	rec := recordHook(t)
	rec.check = func() {
		for name, mu := range map[string]*sync.Mutex{"d.mu": &d.mu, "tailMu": &d.tailMu} {
			if !mu.TryLock() {
				t.Errorf("hook ran with %s held", name)
				continue
			}
			mu.Unlock()
		}
	}
	const fileSize = 10 << 20
	tail := bytes.Repeat([]byte{2}, 1<<20)

	// processWrite site: tail first, then the head from the worker.
	d.writeTail("a", 1, tail, fileSize-1<<20, fileSize)
	d.processWrite("a", 1, bytes.Repeat([]byte{1}, 1<<20), 0)
	// WriteTail site.
	d.processWrite("b", 1, bytes.Repeat([]byte{1}, 1<<20), 0)
	d.WriteTail("b", 1, tail, fileSize-1<<20, fileSize)
	// fillIndexTail site.
	d.processWrite("c", 1, bytes.Repeat([]byte{1}, 1<<20), 0)
	old := TailFetch
	t.Cleanup(func() { TailFetch = old })
	TailFetch = func(_ string, _ int, _ int64, buf []byte) (int, error) { return copy(buf, tail), nil }
	d.fillIndexTail(tailJob{path: d.tailPath("c", 1), hash: "c", fileID: 1, fileSize: fileSize})

	if rec.count() != 3 {
		t.Fatalf("hook calls = %d, want 3 (one per site): %v", rec.count(), rec.calls)
	}
}

// FillWindows writes through the primitives and fires nothing: the warmer records its
// own outcome.
func TestFillWindowsDoesNotFireTheHook(t *testing.T) {
	s := newSwarm(fillFile).allArrive()
	d := fillEnv(t, s)
	old := OnWindowsCached
	t.Cleanup(func() { OnWindowsCached = old })
	OnWindowsCached = func(FileKey, int64) { t.Error("FillWindows fired OnWindowsCached") }
	if _, err := d.FillWindows(fillReq()); err != nil {
		t.Fatal(err)
	}
	if ok, size := d.WindowsComplete(fillKey); !ok || size != fillFile {
		t.Fatalf("after a fill: complete=%v size=%d", ok, size)
	}
}
