package daemon

import (
	"context"
	"sync"
	"testing"

	"flodni/internal/gostorm/native"
	"flodni/internal/warmup"
)

// The set of cache files a handle holds open, which quota eviction skips. A count per
// file, kept at open and given back at release, read without any lock.

func fileKey(hash string, idx int) warmup.FileKey { return warmup.FileKey{Hash: hash, FileIdx: idx} }

func TestOpenFileIsOpenUntilTheLastHandleReleases(t *testing.T) {
	k := fileKey("openfiles-two", 3)
	a, b := &MkvHandle{}, &MkvHandle{}
	if isFileOpen(k) {
		t.Fatal("open before any handle")
	}
	a.holdFile(k.Hash, k.FileIdx)
	b.holdFile(k.Hash, k.FileIdx)
	if !isFileOpen(k) {
		t.Fatal("not open with two handles")
	}
	if isFileOpen(fileKey(k.Hash, 4)) || isFileOpen(fileKey("other", 3)) {
		t.Fatal("another file counted as open")
	}
	a.dropFile()
	if !isFileOpen(k) {
		t.Fatal("closed while one handle still holds it")
	}
	b.dropFile()
	if isFileOpen(k) {
		t.Fatal("still open after the last handle released")
	}
}

// A handle gives its count back once, whatever releases it and however often.
func TestOpenFileCountIsGivenBackOnce(t *testing.T) {
	k := fileKey("openfiles-once", 1)
	a, b := &MkvHandle{}, &MkvHandle{}
	a.holdFile(k.Hash, k.FileIdx)
	b.holdFile(k.Hash, k.FileIdx)
	a.holdFile(k.Hash, k.FileIdx) // a second resolution of the same handle counts nothing
	a.dropFile()
	a.dropFile() // the sweep, then the kernel's Release
	if !isFileOpen(k) {
		t.Fatal("a's second release took b's count")
	}
	b.dropFile()
	if isFileOpen(k) {
		t.Fatal("still open: a's repeated hold counted twice")
	}
}

// A handle that resolves its file after it was released (a read after the orphan sweep)
// must not take a count nothing would ever give back.
func TestOpenFileNotCountedAfterRelease(t *testing.T) {
	k := fileKey("openfiles-late", 2)
	h := &MkvHandle{}
	h.dropFile()
	h.holdFile(k.Hash, k.FileIdx)
	if isFileOpen(k) {
		t.Fatal("a released handle took a count")
	}
}

// Release and the orphan sweep both reach dropFile through releaseResources.
func TestReleaseGivesTheOpenFileBack(t *testing.T) {
	newTestApp(t)
	useTestGlobals(t)
	k := fileKey("openfiles-release", 5)
	const path = "/test/open-files-release.mkv"
	t.Cleanup(func() { stopTimer(&app.priorityTimers, path) })
	h := readHandle(path, 10*gb)
	h.hasSlot.Store(false)
	h.nativeReader = &native.NativeReader{}
	registerHandle(t, h)
	h.holdFile(k.Hash, k.FileIdx)

	h.sweepOrphan()
	if isFileOpen(k) {
		t.Fatal("still open after the sweep dropped the handle from activeHandles")
	}
	if errno := h.Release(context.Background()); errno != 0 {
		t.Fatalf("errno = %v", errno)
	}
	if isFileOpen(k) {
		t.Fatal("open after the kernel's Release")
	}
}

// The hook runs under the cache's quota lock: concurrent holds and drops are safe and
// every one balances.
func TestOpenFileCountsBalanceUnderConcurrency(t *testing.T) {
	k := fileKey("openfiles-race", 7)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				h := &MkvHandle{}
				h.holdFile(k.Hash, k.FileIdx)
				_ = isFileOpen(k)
				h.dropFile()
			}
		}()
	}
	wg.Wait()
	if isFileOpen(k) {
		t.Fatal("open after every handle released")
	}
}

// The daemon installs the hook at start-up: a nil hook would silently protect nothing.
func TestOpenFilesHookIsTheDaemons(t *testing.T) {
	old := warmup.IsOpen
	t.Cleanup(func() { warmup.IsOpen = old })
	installOpenFileHook()
	k := fileKey("openfiles-hook", 9)
	h := &MkvHandle{}
	h.holdFile(k.Hash, k.FileIdx)
	t.Cleanup(h.dropFile)
	if warmup.IsOpen == nil || !warmup.IsOpen(k) {
		t.Fatal("warmup.IsOpen does not see the open handle")
	}
}
