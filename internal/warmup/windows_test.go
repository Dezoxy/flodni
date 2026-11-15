package warmup

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWindowsNilCache(t *testing.T) {
	var d *DiskWarmupCache
	if got := d.Windows(); got != nil {
		t.Fatalf("nil cache listed %v", got)
	}
}

func TestWindowsPartialAndCompleteWindows(t *testing.T) {
	d := indexCache(t, t.TempDir())
	const fileSize = 8 << 20 // windows are 1 MiB
	k := FileKey{"h", 1}

	d.processWrite("h", 1, bytes.Repeat([]byte{1}, 256<<10), 0)
	d.writeTail("h", 1, bytes.Repeat([]byte{2}, 256<<10), fileSize-256<<10, fileSize)
	if got := d.Windows()[k]; got != (WindowState{HeadBytes: 256 << 10, TailBytes: 256 << 10}) {
		t.Fatalf("partial windows = %+v", got)
	}

	d.processWrite("h", 1, bytes.Repeat([]byte{1}, 768<<10), 256<<10)
	d.writeTail("h", 1, bytes.Repeat([]byte{2}, 768<<10), fileSize-1<<20, fileSize)
	if got := d.Windows()[k]; got != (WindowState{HeadBytes: 1 << 20, TailBytes: 1 << 20, TailComplete: true}) {
		t.Fatalf("complete windows = %+v", got)
	}
	if !d.TailReady("h", 1) {
		t.Fatal("TailReady disagrees with the listing")
	}
}

// A tail window shorter than TailWarmupSize is the whole file's end; its tail completes.
func TestWindowsShortFileTailCompletes(t *testing.T) {
	d := indexCache(t, t.TempDir())
	const fileSize = 300 << 10
	d.writeTail("s", 2, bytes.Repeat([]byte{1}, fileSize), 0, fileSize)
	want := WindowState{TailBytes: fileSize, TailComplete: true}
	if got := d.Windows()[FileKey{"s", 2}]; got != want {
		t.Fatalf("short tail = %+v, want %+v", got, want)
	}
	if !d.TailReady("s", 2) {
		t.Fatal("a fully covered short tail must be ready")
	}

	// And after a restart: the cover file carries the short window.
	restarted := &DiskWarmupCache{dir: d.dir}
	restarted.scanExisting(false)
	if got := restarted.Windows()[FileKey{"s", 2}]; got != want {
		t.Fatalf("short tail after restart = %+v, want %+v", got, want)
	}
}

func TestWindowsSkipsEmptyTrackers(t *testing.T) {
	d := indexCache(t, t.TempDir())
	d.tailRangeFor(d.tailPath("e", 1)) // what a probe of an unwritten tail leaves
	if got := d.Windows(); len(got) != 0 {
		t.Fatalf("empty tracker listed: %v", got)
	}
}

func TestWindowsEntriesVanish(t *testing.T) {
	const fileSize = 8 << 20
	fill := func(d *DiskWarmupCache, hash string) {
		d.processWrite(hash, 1, bytes.Repeat([]byte{1}, 1<<20), 0)
		d.writeTail(hash, 1, bytes.Repeat([]byte{2}, 1<<20), fileSize-1<<20, fileSize)
		if _, ok := d.Windows()[FileKey{hash, 1}]; !ok {
			t.Fatalf("%s not listed after writes", hash)
		}
	}

	t.Run("RemoveHash", func(t *testing.T) {
		d := indexCache(t, t.TempDir())
		fill(d, "rm")
		fill(d, "keep")
		d.RemoveHash("rm")
		w := d.Windows()
		if _, ok := w[FileKey{"rm", 1}]; ok {
			t.Fatal("removed hash still listed")
		}
		if _, ok := w[FileKey{"keep", 1}]; !ok {
			t.Fatal("RemoveHash dropped another hash")
		}
	})

	t.Run("eviction", func(t *testing.T) {
		oldGB := diskQuotaGB
		t.Cleanup(func() { diskQuotaGB = oldGB })
		diskQuotaGB = 1
		const quota = int64(1) << 30

		d := indexCache(t, t.TempDir())
		fill(d, "old")
		past := time.Now().Add(-time.Hour)
		for _, p := range []string{d.filePath("old", 1), d.tailPath("old", 1)} {
			if err := os.Chtimes(p, past, past); err != nil {
				t.Fatal(err)
			}
		}
		sparseFile(t, d.filePath("big", 1), quota-512<<10, time.Minute)
		d.sizeCache.Store(d.filePath("big", 1), sizeEntry{size: quota - 512<<10, updatedAt: time.Now()})
		atomic.AddInt64(&d.totalSize, quota-512<<10)
		d.mu.Lock()
		// "big" is the survivor by exclusion: the low-water mark would otherwise take it too.
		evicted, _ := d.enforceQuotaLocked(0, evictOpts{exclude: FileKey{"big", 1}})
		d.mu.Unlock()
		if len(evicted) != 1 || evicted[0].Key != (FileKey{"old", 1}) {
			t.Fatalf("evicted %+v, want the old file", evicted)
		}
		w := d.Windows()
		if _, ok := w[FileKey{"old", 1}]; ok {
			t.Fatalf("evicted file still listed: %+v", w[FileKey{"old", 1}])
		}
		if _, ok := w[FileKey{"big", 1}]; !ok {
			t.Fatal("eviction dropped the survivor")
		}
	})

	t.Run("dropResidue", func(t *testing.T) {
		d := indexCache(t, t.TempDir())
		d.processWrite("res", 1, bytes.Repeat([]byte{1}, 64<<10), 0)
		if got := d.Windows()[FileKey{"res", 1}].HeadBytes; got != 64<<10 {
			t.Fatalf("residue not listed before the drop: %d", got)
		}
		d.closeHandle(d.filePath("res", 1))
		d.dropResidue(d.filePath("res", 1))
		if _, ok := d.Windows()[FileKey{"res", 1}]; ok {
			t.Fatal("dropped residue still listed")
		}
	})

	t.Run("stat miss", func(t *testing.T) {
		d := indexCache(t, t.TempDir())
		path := d.filePath("gone", 1)
		// A size entry whose file vanished and whose TTL ran out: the next lookup
		// stats, misses, and must take the phantom out of the listing.
		d.sizeCache.Store(path, sizeEntry{size: 2 << 20, updatedAt: time.Now().Add(-time.Minute)})
		if _, ok := d.Windows()[FileKey{"gone", 1}]; !ok {
			t.Fatal("setup: phantom not listed")
		}
		if got := d.GetAvailableRange("gone", 1); got != 0 {
			t.Fatalf("range = %d for a missing file", got)
		}
		if _, ok := d.Windows()[FileKey{"gone", 1}]; ok {
			t.Fatal("phantom survived a stat miss")
		}
	})
}

func TestWindowsRestartRestoresBothWindows(t *testing.T) {
	dir := t.TempDir()
	const fileSize = 8 << 20
	d := indexCache(t, dir)
	d.processWrite("r", 3, bytes.Repeat([]byte{1}, 1<<20), 0)
	d.writeTail("r", 3, bytes.Repeat([]byte{2}, 1<<20), fileSize-1<<20, fileSize)
	d.processWrite("p", 4, bytes.Repeat([]byte{1}, 64<<10), 0) // residue: removed by the walk
	want := WindowState{HeadBytes: 1 << 20, TailBytes: 1 << 20, TailComplete: true}
	if got := d.Windows()[FileKey{"r", 3}]; got != want {
		t.Fatalf("before restart = %+v", got)
	}

	restarted := &DiskWarmupCache{dir: dir}
	restarted.scanExisting(false)
	w := restarted.Windows()
	if got := w[FileKey{"r", 3}]; got != want {
		t.Fatalf("after restart = %+v, want %+v", got, want)
	}
	if _, ok := w[FileKey{"p", 4}]; ok {
		t.Fatal("a residue below the ready floor was listed after restart")
	}
}

// After concurrent writers, removals and listings settle, the listing is what a walk of
// the cache directory finds.
func TestWindowsAgreeWithDiskWalk(t *testing.T) {
	d := indexCache(t, t.TempDir())
	const fileSize = 8 << 20
	const hashes = 12

	stop := make(chan struct{})
	var listers sync.WaitGroup
	listers.Add(1)
	go func() { // listing while writes race
		defer listers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				d.Windows()
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < hashes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			hash := fmt.Sprintf("h%02d", i)
			for c := 0; c < 1+i%4; c++ { // heads of 1..4 chunks of 256 KiB
				d.processWrite(hash, 1, bytes.Repeat([]byte{1}, 256<<10), int64(c)*256<<10)
			}
			if i%3 != 0 { // some files have no tail
				n := int64(128<<10) * int64(1+i%8) // 128 KiB..1 MiB: the last ones complete it
				d.writeTail(hash, 1, bytes.Repeat([]byte{2}, int(n)), fileSize-n, fileSize)
			}
			if i%5 == 0 {
				d.RemoveHash(hash)
			}
		}(i)
	}
	wg.Wait()
	close(stop)
	listers.Wait()

	got := d.Windows()
	want := map[FileKey]WindowState{}
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		var stem string
		switch {
		case strings.HasSuffix(name, warmupSuffix):
			stem = strings.TrimSuffix(name, warmupSuffix)
		case strings.HasSuffix(name, tailSuffix):
			stem = strings.TrimSuffix(name, tailSuffix)
		default:
			continue
		}
		k := splitCacheName(stem)
		ws := want[k]
		if strings.HasSuffix(name, warmupSuffix) {
			ws.HeadBytes = info.Size()
		} else {
			// The tail file is sparse and written from its first covered offset: its
			// apparent size is the window's end, so coverage is checked via TailReady.
			ws.TailBytes = got[k].TailBytes
			ws.TailComplete = d.TailReady(k.Hash, k.FileIdx)
			if got[k].TailBytes <= 0 || got[k].TailBytes > info.Size() {
				t.Errorf("%v: tail bytes %d vs file size %d", k, got[k].TailBytes, info.Size())
			}
		}
		want[k] = ws
	}
	if len(got) != len(want) {
		t.Fatalf("listing has %d files, disk walk %d\nlisting: %v\nwalk:    %v", len(got), len(want), got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%v: listing %+v, disk %+v", k, got[k], w)
		}
	}
	for i := 0; i < hashes; i += 5 {
		if _, ok := got[FileKey{fmt.Sprintf("h%02d", i), 1}]; ok {
			t.Errorf("removed hash h%02d listed", i)
		}
	}
}

func TestWarmCountsUseTheReadyFloorAndCompleteTails(t *testing.T) {
	const fileSize = 8 << 20
	var nilCache *DiskWarmupCache
	if h, tl := nilCache.WarmCounts(); h != 0 || tl != 0 {
		t.Fatalf("nil cache counts %d/%d", h, tl)
	}

	d := indexCache(t, t.TempDir())
	// full head and full tail
	d.processWrite("a", 1, bytes.Repeat([]byte{1}, 1<<20), 0)
	d.writeTail("a", 1, bytes.Repeat([]byte{2}, 1<<20), fileSize-1<<20, fileSize)
	// head below the floor, partial tail
	d.processWrite("b", 1, bytes.Repeat([]byte{1}, 64<<10), 0)
	d.writeTail("b", 1, bytes.Repeat([]byte{2}, 64<<10), fileSize-64<<10, fileSize)
	// tail only
	d.writeTail("c", 1, bytes.Repeat([]byte{2}, 1<<20), fileSize-1<<20, fileSize)

	heads, tails := d.WarmCounts()
	if heads != 1 || tails != 2 {
		t.Fatalf("warm heads/tails = %d/%d, want 1/2", heads, tails)
	}
}

func TestShortTailPartialIsNotComplete(t *testing.T) {
	d := indexCache(t, t.TempDir())
	const fileSize = 300 << 10
	d.writeTail("s", 2, bytes.Repeat([]byte{1}, 100<<10), 0, fileSize)
	if d.TailReady("s", 2) {
		t.Fatal("a third of a short tail counted as complete")
	}
	if got := d.Windows()[FileKey{"s", 2}]; got != (WindowState{TailBytes: 100 << 10}) {
		t.Fatalf("partial short tail = %+v", got)
	}
}

func TestShortTailCoverDroppedWhenTheWindowShrinksBelowIt(t *testing.T) {
	dir := t.TempDir()
	d := indexCache(t, dir) // its cleanup restores TailWarmupSize
	const fileSize = 300 << 10
	d.writeTail("s", 2, bytes.Repeat([]byte{1}, fileSize), 0, fileSize)
	TailWarmupSize = 256 << 10 // the file is no longer shorter than the window
	restarted := &DiskWarmupCache{dir: dir}
	if _, dropped, _, _ := restarted.scanExisting(false); dropped != 1 || restarted.TailReady("s", 2) {
		t.Fatalf("short cover kept for a smaller window (dropped=%d)", dropped)
	}
}
