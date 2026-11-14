package daemon

import (
	"context"
	"errors"
	"flodni/internal/gostorm/native"
	"flodni/internal/gostorm/web"
	"flodni/internal/library"
	"flodni/internal/registry"
	"flodni/internal/vfs"
	"flodni/internal/warmup"
	"fmt"
	"github.com/cespare/xxhash/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// resolveTargetFile finds the torrent hash and file index for a given URL and size.
func resolveTargetFile(url string, targetSize int64, physicalPath string) (string, int, error) {
	target, err := resolveOpenTarget(url, targetSize, physicalPath, nil, false)
	return target.Hash, target.FileIndex, err
}

// resolveOpenTarget is resolveTargetFile with the engine's file list given when the
// caller already has it (haveFiles), so an open reads it once.
func resolveOpenTarget(url string, targetSize int64, physicalPath string, files []library.TorrentFile, haveFiles bool) (library.OpenTarget, error) {
	if nativeBridge == nil {
		return library.OpenTarget{}, fmt.Errorf("nativeBridge is nil")
	}
	if !strings.Contains(url, "link=") {
		return library.OpenTarget{}, fmt.Errorf("file not found in torrent")
	}

	hashStr := strings.ToLower(urlQueryValue(url, "link="))
	urlIndex := 0
	if raw := urlQueryValue(url, "index="); raw != "" {
		if idx, err := strconv.Atoi(raw); err == nil {
			urlIndex = idx
		}
	}

	// The ids are the engine's own, in the order it assigned them.
	if !haveFiles {
		files = engineFileList(hashStr)
	}
	return library.ResolveOpenTarget(hashStr, urlIndex, targetSize, physicalPath, files)
}

// urlQueryValue reads a bare query value without parsing the whole URL, which
// this FUSE path does on every Open.
func urlQueryValue(url, key string) string {
	start := strings.Index(url, key)
	if start < 0 {
		return ""
	}
	start += len(key)
	if end := strings.Index(url[start:], "&"); end >= 0 {
		return url[start : start+end]
	}
	return url[start:]
}

// Fast deterministic inode from FNV-1a hash to avoid syscalls in Readdir.
// Uses POSIX bits (syscall.S_IFDIR/S_IFREG) for FUSE/Samba/kernel compatibility.
func hashFilenameToInode(name string) uint64 {
	return xxhash.Sum64String(name)
}

// sanitizeTime ensures a time is never "zero" (1970), falling back to current time
func sanitizeTime(t time.Time) uint64 {
	if t.IsZero() || t.Unix() <= 0 {
		return uint64(time.Now().Unix())
	}
	return uint64(t.Unix())
}

// fillAttrFromStat populates FUSE attributes from a standard syscall.Stat_t.
func fillAttrFromStat(st *syscall.Stat_t, out *fuse.Attr) {
	out.Ino = st.Ino
	out.Nlink = uint32(st.Nlink)
	out.Mode = uint32(st.Mode)
	out.Uid = st.Uid
	out.Gid = st.Gid
	out.Rdev = uint32(st.Rdev)
	// out.Blksize = uint32(st.Blksize) // CRITICAL: Samba uses for buffer sizing
	out.Blksize = uint32(gc().FuseBlockSize) // Configurable block size (default 1MB)
	out.Blocks = uint64(st.Blocks)           // CRITICAL: Samba uses for throughput calc
	out.Size = uint64(st.Size)

	// Use time.Now() as cross-platform baseline for virtualized FUSE attributes.
	now := time.Now()
	out.Mtime = sanitizeTime(now)
	out.Atime = sanitizeTime(now)
	out.Ctime = sanitizeTime(now)
}

// fillAttrFromMetadata populates FUSE attributes from our internal Metadata.
func fillAttrFromMetadata(m *vfs.Metadata, out *fuse.Attr) {
	out.Size = uint64(m.Size)
	out.Mode = syscall.S_IFREG | 0644
	out.Uid, out.Gid = gc().UID, gc().GID
	out.Nlink = 1
	// out.Blksize = 4096                                 // Standard block size
	out.Blksize = uint32(gc().FuseBlockSize)  // Configurable block size (default 1MB)
	out.Blocks = (uint64(m.Size) + 511) / 512 // Estimate blocks based on size

	ts := sanitizeTime(m.Mtime)
	out.Mtime = ts
	out.Atime = ts
	out.Ctime = ts
}

// VirtualMkvRoot - nodo radice per file virtuali .mkv
type VirtualMkvRoot struct {
	fs.Inode
	sourcePath string
}

// Compile-time interface checks - verificano che implementiamo correttamente le interfacce
var _ = (fs.InodeEmbedder)((*VirtualMkvRoot)(nil))
var _ = (fs.NodeLookuper)((*VirtualMkvRoot)(nil))
var _ = (fs.NodeReaddirer)((*VirtualMkvRoot)(nil))
var _ = (fs.NodeGetattrer)((*VirtualMkvRoot)(nil))
var _ = (fs.NodeStatfser)((*VirtualMkvRoot)(nil))

func (r *VirtualMkvRoot) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	st := syscall.Stat_t{}
	if err := syscall.Stat(r.sourcePath, &st); err != nil {
		logger.Printf("ROOT GETATTR ERROR: stat failed for %s: %v", r.sourcePath, err)
		return vfs.ToErrno(err)
	}

	fillAttrFromStat(&st, &out.Attr)

	// Root inode must be constant (required by Plex).
	out.Ino = vfs.InodeRoot
	out.Mode = syscall.S_IFDIR | 0755
	out.Size = 4096

	return 0
}

func (r *VirtualMkvRoot) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	fullPath := filepath.Join(r.sourcePath, name)

	if isStubPath(fullPath) {
		meta, err := getOrReadMeta(fullPath)
		if err == nil {
			addFileToInodeMap(fullPath, meta.URL)
			ino := getFileInodeFromMap(fullPath)
			node := &VirtualMkvNode{vMeta: meta}
			stable := fs.StableAttr{
				Mode: syscall.S_IFREG,
				Ino:  ino,
				Gen:  1,
			}
			child := r.NewInode(ctx, node, stable)
			fillAttrFromMetadata(meta, &out.Attr)
			out.Ino = ino
			return child, 0
		}
	}

	// Fallback for directories or other files
	st := syscall.Stat_t{}
	if err := syscall.Lstat(fullPath, &st); err != nil {
		return nil, syscall.ENOENT
	}

	if (st.Mode & syscall.S_IFMT) == syscall.S_IFDIR {
		node := &VirtualDirNode{physicalPath: fullPath}
		dirIno := getDirInodeFromMap(fullPath)
		stable := fs.StableAttr{
			Mode: syscall.S_IFDIR,
			Ino:  dirIno,
			Gen:  1,
		}
		child := r.NewInode(ctx, node, stable)
		fillAttrFromStat(&st, &out.Attr)
		out.Ino = dirIno
		return child, 0
	}

	node := &fs.LoopbackNode{RootData: &fs.LoopbackRoot{Path: r.sourcePath}}
	stable := fs.StableAttr{Mode: uint32(st.Mode & syscall.S_IFMT)}
	child := r.NewInode(ctx, node, stable)
	return child, 0
}

type nfsDirStream struct {
	entries []fuse.DirEntry
	index   int
}

func (s *nfsDirStream) HasNext() bool {
	return s.index < len(s.entries)
}

func (s *nfsDirStream) Next() (fuse.DirEntry, syscall.Errno) {
	e := s.entries[s.index]
	s.index++
	return e, 0
}

func (s *nfsDirStream) Seekdir(ctx context.Context, off uint64) syscall.Errno {
	if off == 0 {
		s.index = 0
		return 0
	}
	for i, e := range s.entries {
		if e.Off == off {
			s.index = i + 1
			return 0
		}
	}
	return 0
}

func (s *nfsDirStream) Close() {}

func (r *VirtualMkvRoot) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	if entries, found := globalDirCache.Get(r.sourcePath); found {
		return &nfsDirStream{entries: entries}, 0
	}
	// Snapshot the generation before the walk: a concurrent invalidate must not be
	// overwritten by this listing landing after it.
	generation := globalDirCache.Generation(r.sourcePath)

	entries, err := os.ReadDir(r.sourcePath)
	if err != nil {
		logger.Printf("READDIR ERROR: %v", err)
		return nil, vfs.ToErrno(err)
	}

	result := make([]fuse.DirEntry, 0, len(entries))
	for i, e := range entries {
		mode := uint32(syscall.S_IFREG)
		var ino uint64
		fullPath := filepath.Join(r.sourcePath, e.Name())
		if e.IsDir() {
			mode = syscall.S_IFDIR
			ino = getDirInodeFromMap(fullPath)
		} else {
			ino = getFileInodeFromMap(fullPath)
		}

		result = append(result, fuse.DirEntry{
			Name: e.Name(),
			Mode: mode,
			Ino:  ino,
			Off:  uint64(i + 1),
		})
	}

	globalDirCache.PutIfGeneration(r.sourcePath, result, generation)

	return &nfsDirStream{entries: result}, 0
}

// Statfs implements fs.NodeStatfser to provide filesystem statistics for Samba compatibility
func (r *VirtualMkvRoot) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	logger.Printf("=== STATFS === path=%s", r.sourcePath)

	// Calculate realistic values based on virtual files
	// Block size: standard 4KB
	out.Bsize = 4096

	// Total blocks: ~1TB virtual filesystem (arbitrary but realistic)
	out.Blocks = 250 * 1024 * 1024 // 1TB / 4KB blocks

	// Free blocks: ~500GB available (half of total, arbitrary)
	out.Bfree = 125 * 1024 * 1024
	out.Bavail = 125 * 1024 * 1024 // Available to non-root users

	// File counts: based on actual cache size
	totalFiles := uint64(metaCache.Len())
	if totalFiles == 0 {
		totalFiles = 1000 // Fallback estimate if cache not populated
	}
	out.Files = totalFiles
	out.Ffree = 500 // Arbitrary free inodes

	// Namemax: maximum filename length
	out.NameLen = 255

	logger.Printf("STATFS: blocks=%d/%d files=%d/%d bsize=%d",
		out.Blocks, out.Bfree, out.Files, out.Ffree, out.Bsize)

	return 0
}

// VirtualDirNode - nodo per directory (movies, tv) con file .mkv virtuali
type VirtualDirNode struct {
	fs.Inode
	physicalPath string // Path fisico della directory (es. /mnt/torrserver/movies)
}

// Compile-time interface checks for VirtualDirNode
var _ fs.NodeReaddirer = (*VirtualDirNode)(nil)
var _ fs.NodeLookuper = (*VirtualDirNode)(nil)
var _ fs.NodeGetattrer = (*VirtualDirNode)(nil)
var _ fs.NodeUnlinker = (*VirtualDirNode)(nil)
var _ fs.NodeMkdirer = (*VirtualDirNode)(nil)
var _ fs.NodeCreater = (*VirtualDirNode)(nil)
var _ fs.NodeRenamer = (*VirtualDirNode)(nil)
var _ fs.NodeSetattrer = (*VirtualDirNode)(nil)

// Every mutation of a virtual directory answers ENOSYS.
func (d *VirtualDirNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, syscall.ENOSYS
}

func (d *VirtualDirNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	return nil, nil, 0, syscall.ENOSYS
}

func (d *VirtualDirNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	return syscall.ENOSYS
}

func (d *VirtualDirNode) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	return syscall.ENOSYS
}

func (d *VirtualDirNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	if entries, found := globalDirCache.Get(d.physicalPath); found {
		return &nfsDirStream{entries: entries}, 0
	}
	// Snapshot the generation before the walk: a concurrent invalidate must not be
	// overwritten by this listing landing after it.
	generation := globalDirCache.Generation(d.physicalPath)

	entries, err := os.ReadDir(d.physicalPath)
	if err != nil {
		logger.Printf("READDIR DIR ERROR: %v", err)
		return nil, vfs.ToErrno(err)
	}

	result := make([]fuse.DirEntry, 0, len(entries))
	for i, e := range entries {
		fullPath := filepath.Join(d.physicalPath, e.Name())
		if e.IsDir() {
			ino := getDirInodeFromMap(fullPath)
			result = append(result, fuse.DirEntry{
				Name: e.Name(),
				Mode: syscall.S_IFDIR,
				Ino:  ino,
				Off:  uint64(i + 1),
			})
		} else if isStubPath(fullPath) {
			ino := getFileInodeFromMap(fullPath)
			result = append(result, fuse.DirEntry{
				Name: e.Name(),
				Mode: syscall.S_IFREG,
				Ino:  ino,
				Off:  uint64(i + 1),
			})
		}
	}

	globalDirCache.PutIfGeneration(d.physicalPath, result, generation)

	return &nfsDirStream{entries: result}, 0
}

func (d *VirtualDirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	fullPath := filepath.Join(d.physicalPath, name)

	if isStubPath(fullPath) {
		meta, err := getOrReadMeta(fullPath)
		if err == nil {
			addFileToInodeMap(fullPath, meta.URL)
			ino := getFileInodeFromMap(fullPath)
			node := &VirtualMkvNode{vMeta: meta}
			stable := fs.StableAttr{
				Mode: syscall.S_IFREG,
				Ino:  ino,
				Gen:  1,
			}
			child := d.NewInode(ctx, node, stable)
			fillAttrFromMetadata(meta, &out.Attr)
			out.Ino = ino
			return child, 0
		}
	}

	// Fallback for directories
	st := syscall.Stat_t{}
	if err := syscall.Stat(fullPath, &st); err != nil {
		return nil, syscall.ENOENT
	}

	if (st.Mode & syscall.S_IFMT) == syscall.S_IFDIR {
		node := &VirtualDirNode{physicalPath: fullPath}
		dirIno := getDirInodeFromMap(fullPath)
		stable := fs.StableAttr{
			Mode: syscall.S_IFDIR,
			Ino:  dirIno,
			Gen:  1,
		}
		child := d.NewInode(ctx, node, stable)
		fillAttrFromStat(&st, &out.Attr)
		out.Ino = dirIno
		return child, 0
	}

	return nil, syscall.ENOENT
}

// Getattr returns directory attributes
func (d *VirtualDirNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	st := syscall.Stat_t{}
	if err := syscall.Stat(d.physicalPath, &st); err != nil {
		logger.Printf("GETATTR DIR ERROR: %v", err)
		return vfs.ToErrno(err)
	}

	fillAttrFromStat(&st, &out.Attr)

	// Use full-path hash to avoid inode collisions (e.g. Season.01 dirs).
	out.Ino = getDirInodeFromMap(d.physicalPath)

	// Override ONLY Mode and Size to ensure directory permissions and Samba compliance
	out.Mode = syscall.S_IFDIR | 0755
	out.Size = 4096

	return 0
}

// forceCloseVirtualFile terminates the active pump and closes all open handles for a
// virtual .mkv path. Without this, smbd D-states on a file with an active blocking read.
// Shared by the FUSE Unlink handler and invalidateSyncRemovedPath.
func forceCloseVirtualFile(fullPath string) {
	// V-OpenTracker: attendi che i Read() in volo completino prima di cancellare
	// la pump, per evitare nil-deref su nativeReader durante cancel concorrente.
	if globalOpenTracker.IsPathOpen(fullPath) {
		for i := 0; i < 3; i++ {
			if !globalOpenTracker.IsPathOpen(fullPath) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if val, ok := activePumps.Load(fullPath); ok {
		ps := val.(*NativePumpState)
		if ps.cancel != nil {
			ps.cancel()
		}
		activePumps.Delete(fullPath)
		logger.Printf("UNLINK: force-terminated active pump for %s", filepath.Base(fullPath))
	}
	// Close all handles referencing this file
	activeHandles.Range(func(key, value interface{}) bool {
		h := key.(*MkvHandle)
		if h.path == fullPath {
			if h.nativeReader != nil {
				_ = h.nativeReader.Close() // force-closing a read handle; nothing to recover
			}
			activeHandles.Delete(h)
			logger.Printf("UNLINK: force-closed handle for %s", filepath.Base(fullPath))
		}
		return true
	})
}

// invalidateSyncRemovedPath drops FUSE-layer state for a stub removed out-of-band by the
// library API: its os.Remove on the physical path bypasses the Unlink handler, so
// without this the virtual file keeps being listed and served for hours (dircache), and
// a Plex scan in that window registers the replaced file as a ghost duplicate version
// that later fails to play once the cache finally expires.
func invalidateSyncRemovedPath(path string) {
	if strings.HasSuffix(path, ".mkv") {
		forceCloseVirtualFile(path)
		// Close the session now, before the library clears the readability row: a session
		// left open writes its row back when it closes. closeSession keeps its usual
		// rules, so a young one gets no dead-swarm verdict.
		if v, ok := sessions.Load(path); ok {
			v.(*TTFFSession).closeSession()
		}
		registry.RemoveFromRegistry(path)
	}
	// The directory listing is not the only way back to a removed stub: a lookup by
	// exact path is served from the metadata cache, which holds entries for 24h.
	if metaCache != nil {
		metaCache.Delete(path)
	}
	// The file's own directory and every ancestor up to the source root: adding the
	// first track of an album changes the listing of each directory on the way up, and
	// a Readdir of any of them inside the TTL would otherwise miss it. Deleting the
	// path itself covers removed directories too (empty season/show cleanup).
	if physicalSourcePath == "" {
		globalDirCache.Delete(filepath.Dir(path))
	} else {
		dir := filepath.Dir(path)
		for strings.HasPrefix(dir, physicalSourcePath) {
			globalDirCache.Delete(dir)
			if dir == physicalSourcePath {
				break
			}
			next := filepath.Dir(dir)
			if next == dir {
				break
			}
			dir = next
		}
	}
	globalDirCache.Delete(path)
}

// unlinkRemove removes the stub at path through the library's reference-checked
// removal; main wires it once the library exists.
var unlinkRemove func(ctx context.Context, path string) error

// Unlink handles file deletion and triggers torrent auto-remove (FASE 4.2)
func (d *VirtualDirNode) Unlink(ctx context.Context, name string) syscall.Errno {
	logger.Printf("=== UNLINK === dir=%s file=%s", d.physicalPath, name)

	// Only handle .mkv files
	if !strings.HasSuffix(name, ".mkv") {
		logger.Printf("UNLINK: not an mkv file, skipping auto-remove")
		return syscall.EPERM // Not permitted for non-mkv files
	}

	fullPath := filepath.Join(d.physicalPath, name)

	forceCloseVirtualFile(fullPath)
	tailFillTargets.Delete(fullPath)

	// The library deletes the stub, invalidates the caches and forgets the
	// episode, then drops the torrent only when no other stub uses it. A torrent
	// that is not dropped is not an error here: the stub is gone.
	if unlinkRemove == nil {
		logger.Printf("UNLINK ERROR: no library removal is wired")
		return syscall.EIO
	}
	if err := unlinkRemove(ctx, fullPath); err != nil {
		var le *library.Error
		if errors.As(err, &le) {
			switch le.Status {
			case http.StatusBadRequest:
				logger.Printf("UNLINK: refused, %s is not a stub inside the media directories: %v", fullPath, err)
				return syscall.EPERM
			case http.StatusNotFound:
				logger.Printf("UNLINK: no stub at %s", fullPath)
				return syscall.ENOENT
			}
		}
		logger.Printf("UNLINK ERROR: failed to remove %s: %v", fullPath, err)
		return syscall.EIO
	}

	logger.Printf("UNLINK COMPLETE: stub removed")
	return 0
}

// VirtualMkvNode - nodo per singolo file .mkv virtuale
type VirtualMkvNode struct {
	fs.Inode
	vMeta *vfs.Metadata
	wake  func(context.Context, string, int) error // per-node activation dependency; wired at Open after RED
}

// Compile-time interface checks
var _ fs.NodeGetattrer = (*VirtualMkvNode)(nil)
var _ fs.NodeOpener = (*VirtualMkvNode)(nil)

func (n *VirtualMkvNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	fillAttrFromMetadata(n.vMeta, &out.Attr)
	out.Ino = getFileInodeFromMap(n.vMeta.Path)
	return 0
}

var _ fs.NodeSetattrer = (*VirtualMkvNode)(nil)

func (n *VirtualMkvNode) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	return syscall.ENOSYS
}

func (n *VirtualMkvNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if gc().LogLevel == "DEBUG" {
		logger.Printf("=== OPEN VIRTUAL === path=%s", n.vMeta.Path)
	}
	// PROACTIVE CLEANUP TRIGGER (V246): must be sync before any Read() can arrive.
	raCache.SwitchContext(n.vMeta.Path)

	// Cancel any pending release timers (Debounce)
	if oldTimer, ok := pumpTimers.LoadAndDelete(n.vMeta.Path); ok {
		oldTimer.(*time.Timer).Stop()
	}
	if oldTimer, ok := priorityTimers.LoadAndDelete(n.vMeta.Path); ok {
		oldTimer.(*time.Timer).Stop()
	}

	hashStr, urlFileIdx := vfs.ExtractHashAndIndex(n.vMeta.URL)

	// headReady: the warm path (async Wake, direct ID injection, instant start) needs the
	// head warmup file only. tailReady is not a gate; it feeds the [TTFF] line.
	headReady := false
	tailReady := false
	if warmup.DiskWarmup != nil && hashStr != "" {
		headReady = warmup.DiskWarmup.HeadReady(hashStr, urlFileIdx)
		tailReady = warmup.DiskWarmup.TailReady(hashStr, urlFileIdx)
		if headReady && suspectHead(hashStr, urlFileIdx, n.vMeta.Size) {
			// The head under the stub's index may be another file's: open cold, so the
			// wake is synchronous and openTarget resolves against the engine's list.
			headReady, tailReady = false, false
		}
	}
	magnetCandidate := n.vMeta.URL
	if hashStr != "" && (strings.HasPrefix(n.vMeta.URL, "http://") || strings.HasPrefix(n.vMeta.URL, "https://")) {
		magnetCandidate = "magnet:?xt=urn:btih:" + hashStr
	}

	// Async Wake when head warmup is ready (Open returns instantly); sync Wake otherwise.
	wake := n.wake
	if wake == nil && nativeBridge != nil {
		wake = nativeBridge.Wake
	}
	if wake != nil && magnetCandidate != "" {
		if headReady {
			// Open returns instantly here, so the activation must not inherit the
			// request context: the kernel tears it down as soon as Open returns, and
			// a background context lets the wake finish for the reader that is about
			// to open the same path.
			safeGo(func() {
				if err := wake(context.Background(), magnetCandidate, urlFileIdx); err != nil {
					if errors.Is(err, native.ErrNotStored) {
						noteWakeRefused(n.vMeta.Path, hashStr, "open (background)")
						return
					}
					logger.Printf("[VFS] background activation failed for %s: %v", n.vMeta.Path, err)
				}
			})
		} else {
			if ctx.Err() != nil {
				return nil, 0, syscall.EINTR
			}
			// A synchronous activation that cannot start is terminal: returning a
			// handle here gives a scanner a file whose reads can never be served.
			// The request context goes with it, so a cancelled Open makes the wake
			// return (and release its semaphore token) instead of running to the
			// metadata timeout detached.
			activated := make(chan error, 1)
			go func() {
				// A panicking activation must fail the Open, not take the process down.
				defer func() {
					if r := recover(); r != nil {
						logger.Printf("[PANIC] wake goroutine recovered for %s: %v\n%s", magnetCandidate, r, debug.Stack())
						activated <- fmt.Errorf("activation panicked: %v", r)
					}
				}()
				activated <- wake(ctx, magnetCandidate, urlFileIdx)
			}()
			select {
			case err := <-activated:
				if err != nil {
					if errors.Is(err, native.ErrNotStored) {
						noteWakeRefused(n.vMeta.Path, hashStr, "open")
						// A stub already deleted on disk is gone, not unreadable.
						if _, statErr := os.Stat(n.vMeta.Path); errors.Is(statErr, os.ErrNotExist) {
							return nil, 0, syscall.ENOENT
						}
						return nil, 0, syscall.EIO
					}
					logger.Printf("[VFS] activation failed for %s: %v", n.vMeta.Path, err)
					return nil, 0, syscall.EIO
				}
			case <-ctx.Done():
				return nil, 0, syscall.EINTR
			}
		}
	}

	// Registered only once activation succeeded: a session for an Open that is
	// about to fail outlives the call and is never closed.
	ttffRegister(n.vMeta.Path, n.vMeta.Size, hashStr, urlFileIdx, headReady, tailReady)
	if caller, ok := fuse.FromContext(ctx); ok {
		noteReaderCaller(n.vMeta.Path, caller.Pid, caller.Uid)
	}

	if val, exists := playbackRegistry.Load(n.vMeta.Path); !exists {
		playbackRegistry.Store(n.vMeta.Path, &PlaybackState{
			Path:     n.vMeta.Path,
			Hash:     hashStr,
			ImdbID:   n.vMeta.ImdbID,
			OpenedAt: time.Now(),
		})
	} else {
		if val != nil {
			state := val.(*PlaybackState)
			state.mu.Lock()
			state.OpenedAt = time.Now()
			state.IsStopped = false

			// Restore priority only if webhook confirmed recently (<30m) to avoid zombie torrents.
			recentlyConfirmed := !state.ConfirmedAt.IsZero() && time.Since(state.ConfirmedAt) < 30*time.Minute
			isHealthy := state.IsHealthy
			state.mu.Unlock()

			if isHealthy && recentlyConfirmed && state.Hash != "" {
				hHash := metainfo.NewHashFromHex(state.Hash)
				if t := web.BTS.GetTorrent(hHash); t != nil {
					if !t.IsPriority.Load() {
						t.IsPriority.Store(true)
						logger.Printf("[NativeBridge] Priority RESTORED for Silent Re-Open: %s", state.Hash)
					}
				}
			}
		}
	}

	now := time.Now()

	// Use warmup IDs directly when available, skipping the resolveTargetFile retry loop.
	finalHash, fileIdx, headReady, err := openTarget(n.vMeta.URL, n.vMeta.Size, n.vMeta.Path, hashStr, urlFileIdx, headReady)
	if errors.Is(err, library.ErrStubMismatch) {
		return nil, 0, syscall.EIO // a stale stub no file of the torrent matches: never guess
	}
	isNative := (err == nil)
	if !isNative && gc().LogLevel == "DEBUG" {
		logger.Printf("[NativeBridge] Resolution failed for %s: %v. Access will rely on cache/retry.", filepath.Base(n.vMeta.Path), err)
	}

	h := &MkvHandle{
		url:              n.vMeta.URL,
		magnet:           magnetCandidate, // Store for potential re-wake
		wake:             wake,
		size:             n.vMeta.Size,
		path:             n.vMeta.Path,
		lastTime:         now,
		lastOff:          -1,
		lastActivityTime: now,       // Initialize activity tracking
		hasWarmup:        headReady, // Eligibility for fast SSD probes
		usesWarmup:       true,
	}
	h.state.Store(stateWarmup) // Initial state; transitions to stateStreaming on seek/resume.

	if isNative {
		h.hash = finalHash
		h.fileID = fileIdx
		// media.stop only knows the path; keep what EnsureTail needs to reach the file.
		tailFillTargets.Store(n.vMeta.Path, tailFillTarget{hash: finalHash, fileID: fileIdx, size: n.vMeta.Size})
		coldHeadReady := headReady
		if !coldHeadReady && warmup.DiskWarmup != nil && (finalHash != hashStr || fileIdx != urlFileIdx) {
			coldHeadReady = warmup.DiskWarmup.HeadReady(finalHash, fileIdx) // the resolved pair, not the stub's
		}
		markColdOpen(n.vMeta.Path, warmup.FileKey{Hash: finalHash, FileIdx: fileIdx}, coldHeadReady)
		// Gillian: proactive pump start at Open() — pump ready before first Read().
		// pumpOnce ensures single start; late rescue path in Read() handles hash=='' case.
		// Dezoxy fork: with pump_on_open off, only confirmed playback starts it here; a
		// scanner's Open gets demand fetches and Read()'s pump upgrade handles playback.
		if startPumpAtOpen(gc().PumpOnOpen, playbackConfirmed(n.vMeta.Path)) {
			h.pumpOnce.Do(func() {
				h.startNativePump(finalHash, fileIdx)
			})
		}
		if !headReady {
			// Real cold start: no warmup data present yet. Signal warmupActive here, at Open(),
			// rather than waiting for the first WriteChunk - that first-connection burst is
			// exactly the window aggressive PEX churn (Task 3) needs to catch, and by the time
			// WriteChunk fires once, the torrent's initial peer set has often already connected.
			// warmup.OnWarmupStateChange (wired at startup) still owns turning this back off once
			// the writeWorker synchronously observes real STARTING/COMPLETED transitions.
			forceTorrentWarmupActive(finalHash, fileIdx)
		}
	}

	return h, storeOpenHandle(h), 0
}

// openFlags decides the FUSE open flags (Dezoxy fork). Without FOPEN_KEEP_CACHE the
// kernel invalidates the file's page cache at every open and waits for each page a
// read in flight keeps locked: one slow read (a budget hold, a slot wait, a fetch)
// stalls every other open of the file until it answers. A file with a live handle has
// not changed since that handle's open, so its cache is kept; the first open still
// invalidates, which is what drops the pages of a replaced release. Never
// unconditional: with ExplicitDataCacheControl nothing else drops the pages.
func openFlags(otherHandleLive bool) uint32 {
	if otherHandleLive {
		return fuse.FOPEN_KEEP_CACHE
	}
	return 0
}

// storeOpenHandle adds h to activeHandles and returns its open flags. The question
// "is another handle live" is asked before h is stored: asked after, it is always
// true and the flag becomes unconditional.
func storeOpenHandle(h *MkvHandle) uint32 {
	flags := openFlags(anyLiveHandleFor(h.path))
	activeHandles.Store(h, true)
	return flags
}

// approximateMetadataSize estimates the memory footprint of a Metadata entry
// Used for LRU cache size tracking
func approximateMetadataSize(m *vfs.Metadata) int64 {
	// Approximate size:
	// - URL string: len(URL)
	// - Path string: len(Path)
	// - Size: 8 bytes (int64)
	// - Mtime: 24 bytes (time.Time structure)
	// - Overhead: 64 bytes (pointers, struct alignment)
	return int64(len(m.URL) + len(m.Path) + 8 + 24 + 64)
}

func getOrReadMeta(path string) (*vfs.Metadata, error) {
	var m *vfs.Metadata

	// Check cache first (fast path without lock)
	if val, ok := metaCache.Get(path); ok {
		m = val
	} else {
		// Acquire lock to prevent stampede
		unlock := globalLockManager.Lock(path)
		defer unlock()

		// Double-check cache after acquiring lock
		if val, ok := metaCache.Get(path); ok {
			m = val
		} else {
			fileMeta, err := vfs.ReadMetadataFromFileWithLimits(path, vfs.VideoSizeLimits)
			if err != nil {
				return nil, err
			}

			m = &vfs.Metadata{
				URL:    fileMeta.URL,
				Size:   fileMeta.Size,
				Mtime:  fileMeta.Mtime,
				Path:   fileMeta.Path,
				ImdbID: fileMeta.ImdbID,
			}

			metaCache.Put(path, m, approximateMetadataSize(m))
		}
	}

	return m, nil
}

// isStubPath reports whether a physical path is a virtual file. Every FUSE entry
// point shares it: Readdir, Lookup, Getattr and Open must agree about what is a
// virtual file, or a scanner sees names it cannot open.
func isStubPath(fullPath string) bool {
	base := filepath.Base(fullPath)
	return len(base) > len(".mkv") && strings.HasSuffix(base, ".mkv")
}
