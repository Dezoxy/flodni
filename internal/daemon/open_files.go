package daemon

import (
	"sync"
	"sync/atomic"

	"flodni/internal/warmup"
)

// The cache files FUSE handles hold open, for warmup.IsOpen: quota eviction skips them.
//
// The hook runs under the cache's quota lock and tailMu, so it cannot walk activeHandles
// (h.hash and h.fileID are set late, under h.mu) or take any handle lock. Instead each
// handle counts itself once per file, when it learns which file it reads, and gives
// the count back when it leaves activeHandles (releaseResources: the kernel's Release or the
// orphan sweep). Reading is one map load and one atomic load.

// openFileCount is the number of handles holding one file.
type openFileCount struct{ n atomic.Int32 }

// openFiles maps warmup.FileKey (the hash and index exactly as the handle passes them to
// the cache, which is how the cache names its files) to *openFileCount. An entry is
// never removed: it is a few bytes per file ever opened since the process started.
var openFiles sync.Map

// handleReleased marks a handle that gave its count back: a late resolution must not
// take a new one, which nothing would ever release.
var handleReleased = &openFileCount{}

// isFileOpen is warmup.IsOpen.
func isFileOpen(k warmup.FileKey) bool {
	v, ok := openFiles.Load(k)
	return ok && v.(*openFileCount).n.Load() > 0
}

// installOpenFileHook makes the cache skip the files a handle holds open.
func installOpenFileHook() { warmup.IsOpen = isFileOpen }

// holdFile counts h as holding the file (hash, fileID). It does nothing for a handle that
// already holds a file or was released, and takes no lock, so callers may hold h.mu.
func (h *MkvHandle) holdFile(hash string, fileID int) {
	k := warmup.FileKey{Hash: hash, FileIdx: fileID}
	v, ok := openFiles.Load(k)
	if !ok {
		v, _ = openFiles.LoadOrStore(k, &openFileCount{})
	}
	c := v.(*openFileCount)
	c.n.Add(1)
	if !h.openFile.CompareAndSwap(nil, c) {
		c.n.Add(-1) // already counted, or released
	}
}

// dropFile gives h's count back, once whatever calls it and however often. It is safe
// to call for a handle that never held a file.
func (h *MkvHandle) dropFile() {
	if c := h.openFile.Swap(handleReleased); c != nil && c != handleReleased {
		c.n.Add(-1)
	}
}
