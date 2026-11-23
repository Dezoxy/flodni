package warmup

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// A warm read that races an eviction, and eviction that meets an open file.

// setIsOpen installs the open-file hook for the test and restores it.
func setIsOpen(t *testing.T, f func(FileKey) bool) {
	t.Helper()
	old := IsOpen
	t.Cleanup(func() { IsOpen = old })
	IsOpen = f
}

// headWithData writes a dense head file for ("h", id) holding a recognisable block.
func headWithData(t *testing.T, d *DiskWarmupCache, id int) []byte {
	t.Helper()
	want := bytes.Repeat([]byte{0xAB}, 8192)
	if err := os.WriteFile(d.filePath("h", id), want, 0o644); err != nil {
		t.Fatal(err)
	}
	return want
}

// tailWithData writes a complete tail window for ("h", id) and returns its first page.
func tailWithData(t *testing.T, d *DiskWarmupCache, id int, fileSize int64) (start int64, want []byte) {
	t.Helper()
	start = TailStart(fileSize)
	window := bytes.Repeat([]byte{0xCD}, int(fileSize-start))
	d.WriteTail("h", id, window, start, fileSize)
	if !d.TailReady("h", id) {
		t.Fatal("tail window not ready")
	}
	return start, window[:4096]
}

// A reader that took the cached handle before an eviction closed it reads a closed file:
// that is a cache miss (the caller goes to the engine), not an error and not bytes.
func TestReadOnHandleClosedByEvictionIsAMiss(t *testing.T) {
	const fileSize = 10<<20 + 1000
	cases := []struct {
		name  string
		close func(ch *cachedHandle)
	}{
		{"file closed, flag set (closeHandle)", func(ch *cachedHandle) { ch.closed.Store(true); _ = ch.f.Close() }},
		{"file closed, flag not yet set", func(ch *cachedHandle) { _ = ch.f.Close() }},
		{"flag set, read would still succeed", func(ch *cachedHandle) { ch.closed.Store(true) }},
	}
	for _, c := range cases {
		t.Run("head "+c.name, func(t *testing.T) {
			d := indexCache(t, t.TempDir())
			headWithData(t, d, 1)
			ch, err := d.getHandle(d.filePath("h", 1))
			if err != nil {
				t.Fatal(err)
			}
			c.close(ch) // the reader holds ch; the eviction closed it
			buf := make([]byte, 4096)
			n, err := d.ReadAt("h", 1, buf, 0)
			if n != 0 || err != nil {
				t.Fatalf("n=%d err=%v, want a miss (0, nil)", n, err)
			}
		})
		t.Run("tail "+c.name, func(t *testing.T) {
			d := indexCache(t, t.TempDir())
			start, _ := tailWithData(t, d, 1, fileSize)
			ch, err := d.getHandle(d.tailPath("h", 1))
			if err != nil {
				t.Fatal(err)
			}
			c.close(ch)
			buf := make([]byte, 4096)
			n, err := d.ReadTail("h", 1, buf, start, fileSize)
			if n != 0 || err != nil {
				t.Fatalf("n=%d err=%v, want a miss (0, nil)", n, err)
			}
		})
	}
}

// Every other read error keeps reaching the caller: only a closed handle is a miss.
func TestOtherReadErrorsAreNotAMiss(t *testing.T) {
	const fileSize = 10<<20 + 1000
	// A write-only descriptor makes pread fail with EBADF.
	writeOnly := func(t *testing.T, path string) *cachedHandle {
		t.Helper()
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return &cachedHandle{f: f}
	}
	t.Run("head", func(t *testing.T) {
		d := indexCache(t, t.TempDir())
		headWithData(t, d, 1)
		d.handles.Store(d.filePath("h", 1), writeOnly(t, d.filePath("h", 1)))
		n, err := d.ReadAt("h", 1, make([]byte, 4096), 0)
		if n != 0 || !errors.Is(err, syscall.EBADF) {
			t.Fatalf("n=%d err=%v, want the EBADF to pass through", n, err)
		}
	})
	t.Run("tail", func(t *testing.T) {
		d := indexCache(t, t.TempDir())
		start, _ := tailWithData(t, d, 1, fileSize)
		d.handles.Store(d.tailPath("h", 1), writeOnly(t, d.tailPath("h", 1)))
		n, err := d.ReadTail("h", 1, make([]byte, 4096), start, fileSize)
		if n != 0 || !errors.Is(err, syscall.EBADF) {
			t.Fatalf("n=%d err=%v, want the EBADF to pass through", n, err)
		}
	})
	t.Run("a short read at the end of the window stays n and EOF", func(t *testing.T) {
		d := indexCache(t, t.TempDir())
		start, _ := tailWithData(t, d, 1, fileSize)
		window := fileSize - start
		buf := make([]byte, 4096)
		n, err := d.ReadTail("h", 1, buf, start+window-100, fileSize)
		if n != 100 || err != io.EOF {
			t.Fatalf("n=%d err=%v, want 100 bytes and io.EOF as before", n, err)
		}
	})
}

// The real thing, under -race: readers spin while the quota check evicts the file they
// read. Every answer is the file's bytes or a miss, never an error and never other bytes.
func TestReadRacingEvictionNeverFailsOrCorrupts(t *testing.T) {
	const fileSize = 10<<20 + 1000
	rounds := 500
	if testing.Short() {
		rounds = 20
	}
	d := evictEnv(t)
	// The files stay small, so the read path's own size checks leave them alone; the
	// quota walk counts the victim as 400 MiB and the filler as 800 MiB.
	old := allocatedBytes
	t.Cleanup(func() { allocatedBytes = old })
	allocatedBytes = func(fi os.FileInfo) int64 {
		switch fi.Name()[0] {
		case 'v':
			return 200 * mb
		case 'k':
			return 800 * mb
		}
		return fi.Size()
	}
	block := bytes.Repeat([]byte{0xAB}, 4096)
	start := TailStart(fileSize)
	for r := 0; r < rounds; r++ {
		// "v" is the victim (oldest), "k" a newer file the quota check keeps.
		head, vtail := d.filePath("v", 1), d.tailPath("v", 1)
		for _, p := range []string{head, vtail} {
			if err := os.WriteFile(p, block, 0o644); err != nil {
				t.Fatal(err)
			}
			past := time.Now().Add(-3 * time.Hour)
			if err := os.Chtimes(p, past, past); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(d.filePath("k", 1), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		tr := d.tailRangeFor(vtail)
		tr.setWindow(start, fileSize-start, fileSize)
		tr.add(0, 4096)
		setTotal(d, 1200*mb) // over the 1 GiB quota; the walk resyncs it

		var stop atomic.Bool
		var wg sync.WaitGroup
		bad := make(chan string, 8)
		report := func(s string) {
			select {
			case bad <- s:
			default:
			}
		}
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func(tail bool) {
				defer wg.Done()
				buf := make([]byte, 4096)
				for !stop.Load() {
					var n int
					var err error
					if tail {
						n, err = d.ReadTail("v", 1, buf, start, fileSize)
					} else {
						n, err = d.ReadAt("v", 1, buf, 0)
					}
					if err != nil {
						report("read error: " + err.Error())
						return
					}
					if n != 0 && (n != len(buf) || !bytes.Equal(buf, block)) {
						report("wrong bytes served")
						return
					}
				}
			}(i%2 == 1)
		}
		time.Sleep(time.Millisecond)
		d.mu.Lock()
		ev, _ := d.enforceQuotaLocked(0, evictOpts{trigger: "test"})
		d.mu.Unlock()
		stop.Store(true)
		wg.Wait()
		select {
		case s := <-bad:
			t.Fatalf("round %d: %s", r, s)
		default:
		}
		if len(ev) != 1 || ev[0].Key.Hash != "v" {
			t.Fatalf("round %d: evicted %+v, want the victim v", r, ev)
		}
		_ = os.Remove(d.filePath("k", 1))
		d.tailCoverage.Delete(vtail)
	}
}

// openEnv is the three files of TestEvictionOrder: a, b, c written 3h, 2h and 1h ago,
// 400 MiB each, one file over the quota, so the oldest, a, goes first.
func openEnv(t *testing.T) *DiskWarmupCache {
	t.Helper()
	return readOrderEnv(t, EvictOrderWritten)
}

func TestEvictionSkipsAnOpenFile(t *testing.T) {
	d := openEnv(t)
	setIsOpen(t, func(k FileKey) bool { return k.Hash == "a" })
	if got := evictOne(t, d); got != "b-1" {
		t.Fatalf("evicted %s, want b-1: a is the oldest but open", got)
	}
	if !exists(d.dir+"/a-1"+warmupSuffix) || !exists(d.dir+"/a-1"+tailSuffix) {
		t.Fatal("the open file lost a window")
	}
}

func TestEvictionWithNoHookIsUnchanged(t *testing.T) {
	d := openEnv(t)
	setIsOpen(t, nil)
	if got := evictOne(t, d); got != "a-1" {
		t.Fatalf("evicted %s, want a-1: the oldest, with no hook", got)
	}
}

func TestEvictionTakesTheFileAgainOnceItIsClosed(t *testing.T) {
	d := openEnv(t)
	var open atomic.Bool
	open.Store(true)
	setIsOpen(t, func(k FileKey) bool { return k.Hash == "a" && open.Load() })
	if got := evictOne(t, d); got != "b-1" {
		t.Fatalf("evicted %s, want b-1 while a is open", got)
	}
	// Back over the quota (b's windows are written again); a is closed now and is the
	// oldest left.
	open.Store(false)
	sparseFile(t, d.dir+"/b-1"+warmupSuffix, 300*mb, 2*time.Hour)
	sparseFile(t, d.dir+"/b-1"+tailSuffix, 100*mb, 2*time.Hour)
	setTotal(d, 1200*mb)
	if got := evictOne(t, d); got != "a-1" {
		t.Fatalf("evicted %s, want a-1 after the last handle closed", got)
	}
}

// When every candidate is open nothing is removed and the check answers "no room": it
// neither loops nor waits.
func TestEvictionWithEveryCandidateOpenFindsNoRoom(t *testing.T) {
	for _, policy := range []EvictPolicy{EvictAny, EvictNeverPlayed} {
		d := openEnv(t)
		setIsOpen(t, func(FileKey) bool { return true })
		done := make(chan struct{})
		var ev []Eviction
		var room bool
		go func() {
			defer close(done)
			d.mu.Lock()
			ev, room = d.enforceQuotaLocked(0, evictOpts{policy: policy, trigger: "test"})
			d.mu.Unlock()
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("policy %d: the quota check did not return", policy)
		}
		if room || len(ev) != 0 {
			t.Fatalf("policy %d: room=%v evicted=%+v, want no room and nothing evicted", policy, room, ev)
		}
		for _, stem := range []string{"a-1", "b-1", "c-1"} {
			if !exists(d.dir+"/"+stem+warmupSuffix) || !exists(d.dir+"/"+stem+tailSuffix) {
				t.Fatalf("policy %d: %s lost a window", policy, stem)
			}
		}
	}
}

// An operator's EvictTo is a request for space now: it does not take the open-file skip.
func TestEvictToIgnoresTheOpenHook(t *testing.T) {
	d := openEnv(t)
	setIsOpen(t, func(k FileKey) bool { return k.Hash == "a" })
	res := d.EvictTo(900 * mb)
	if res.Files == 0 || exists(d.dir+"/a-1"+warmupSuffix) {
		t.Fatalf("EvictTo = %+v: the oldest file should go whether open or not", res)
	}
}
