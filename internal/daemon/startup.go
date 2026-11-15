package daemon

import (
	"errors"
	"flodni/internal/flog"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"flodni/internal/cache"
	"flodni/internal/gostorm/settings"
	"flodni/internal/vfs"
)

// ghostInodesFmt is the startup GC line operators grep for.
const ghostInodesFmt = "Startup GC: Pruned %d ghost inodes"

// dirNoStubsFmt names a source directory that held no stubs: the tail of the
// "Startup cache: ... holds no stubs" line and the reason the report gives for the
// prunes it stopped.
const dirNoStubsFmt = "%s holds no stubs"

// unstoredLogHashes is how many hashes the startup warning names.
const unstoredLogHashes = 5

// libraryUnstored is the number of library torrents the engine holds no record with
// metainfo for, set once after the startup walk. Nil until then: a zero before the
// count exists would read as "all stored".
var libraryUnstored atomic.Pointer[int]

// engineHasMetainfo reports whether the engine's database holds a record for the hash
// with the torrent's metainfo. A record without it is refused a wake. One database
// read; a record that does not decode counts as absent.
func engineHasMetainfo(hash string) bool {
	var h metainfo.Hash
	if err := h.FromHexString(hash); err != nil {
		return false
	}
	rec := settings.GetTorrent(h)
	return rec != nil && rec.TorrentSpec != nil && len(rec.InfoBytes) > 0
}

// StartupCacheBuilder pre-populates metadata cache at startup
// This dramatically improves Plex scan performance (30s -> 10s for 1000+ files)
// Runs in background to avoid delaying mount operation
type StartupCacheBuilder struct {
	sourcePath string
	metaCache  *cache.LRUCache
	logger     *slog.Logger

	// Statistics
	mu             sync.Mutex
	filesProcessed int
	filesSkipped   int
	errors         int
	invalidStubs   int // stubs that parse but declare a size outside the valid range
	startTime      time.Time
	foundFiles     map[string]bool // V142: Track files for GC
	foundHashes    map[string]bool // lower-case info hashes of every stub seen, for pruning
	stubsSeen      int             // stubs noted, duplicates included: tells a directory that held none

	// hasMetainfo says whether the engine stores the hash with its metainfo; a test
	// swaps it.
	hasMetainfo func(hash string) bool

	// onDone gets the walk's outcome once the prunes have run; nil in tests that do not
	// need it.
	onDone func(scanResult)

	// What the prunes and the unstored count left behind (guarded by mu).
	readability pruneResult
	fileState   pruneResult
	inodes      pruneResult
	unstoredRan bool
	unstoredN   int
	unstoredOf  int

	// Why the walk was not whole, for the prune reasons: a source directory that was
	// missing (the inode prune needs both) and one that held no stubs (the readability
	// and file-state prunes need a stub in each).
	missingDirs []string
	emptyDirs   []string
}

// NewStartupCacheBuilder creates a new startup cache builder
func NewStartupCacheBuilder(sourcePath string, metaCache *cache.LRUCache, logger *slog.Logger) *StartupCacheBuilder {
	return &StartupCacheBuilder{
		sourcePath:  sourcePath,
		metaCache:   metaCache,
		logger:      logger,
		startTime:   time.Now(),
		foundFiles:  make(map[string]bool),
		foundHashes: make(map[string]bool),
		hasMetainfo: engineHasMetainfo,
	}
}

// Start begins background cache population
// Returns immediately, processing happens in goroutine
func (b *StartupCacheBuilder) Start() {
	b.logger.Info(fmt.Sprintf("Starting cache pre-population from %s", b.sourcePath), "source_path", b.sourcePath)

	go func() {
		walked, whole := b.walkLibrary()

		// Log final statistics
		duration := time.Since(b.startTime)
		b.mu.Lock()
		processed := b.filesProcessed
		skipped := b.filesSkipped
		errors := b.errors
		invalid := b.invalidStubs
		b.mu.Unlock()

		b.logger.Info(fmt.Sprintf("Cache pre-population complete: %d processed, %d skipped, %d invalid (size out of range, prunes still run), %d errors in %v", processed, skipped, invalid, errors, duration), "processed", processed, "skipped", skipped, "invalid", invalid, "errors", errors, "duration", duration)

		// Log cache statistics
		stats := b.metaCache.Stats()
		b.logger.Info(fmt.Sprintf("Cache after startup: %d entries, %.2f MB used of %.2f MB capacity", stats.Entries, float64(stats.Size)/(1024*1024), float64(stats.Capacity)/(1024*1024)), "entries", stats.Entries)

		b.pruneReadability(whole)
		b.pruneFileState(whole)
		b.warnUnstored()

		// V133: Save inode map after startup scan completes
		// This ensures the map is persisted even if no background save triggered
		if globalInodeMap != nil && globalInodeMap.IsDirty() {
			if err := globalInodeMap.SaveToDisk(); err != nil {
				b.logger.Info(fmt.Sprintf("InodeMap: Post-startup save error: %v", err), "err", err)
			} else {
				files, dirs, _, _ := GetInodeMapStats()
				b.logger.Info(fmt.Sprintf("InodeMap: Saved after startup scan (%d files, %d dirs)", files, dirs), "files", files, "dirs", dirs)
			}
		}

		// V142: Inode GC - Remove ghost entries (files that no longer exist)
		b.pruneInodes(walked)

		if b.onDone != nil {
			b.onDone(b.result())
		}
	}()
}

// result snapshots what the walk and its prunes found.
func (b *StartupCacheBuilder) result() scanResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	return scanResult{
		invalidStubs: b.invalidStubs,
		unstoredRan:  b.unstoredRan, unstoredN: b.unstoredN, unstoredOf: b.unstoredOf,
		readability: b.readability, fileState: b.fileState, inodes: b.inodes,
	}
}

// walkLibrary walks movies, then tv. It reports whether both exist (walked) and
// whether each also held at least one stub (whole). A directory that is present and
// empty (a separate mount that did not come up) must not let the other directory's
// stubs stand for the whole library: the prunes would delete the rows of every
// torrent filed in the empty one.
func (b *StartupCacheBuilder) walkLibrary() (walked, whole bool) {
	walked, whole = true, true
	for _, d := range []struct {
		name      string
		recursive bool // tv is recursive for seasons
	}{{"movies", false}, {"tv", true}} {
		path := filepath.Join(b.sourcePath, d.name)
		if _, err := os.Stat(path); err != nil {
			walked, whole = false, false
			b.mu.Lock()
			b.missingDirs = append(b.missingDirs, d.name+" directory missing")
			b.mu.Unlock()
			continue
		}
		b.mu.Lock()
		before := b.stubsSeen
		b.mu.Unlock()
		b.processDirectory(path, d.recursive)
		b.mu.Lock()
		if b.stubsSeen == before {
			whole = false
			b.emptyDirs = append(b.emptyDirs, fmt.Sprintf(dirNoStubsFmt, d.name))
			flog.Printf(b.logger, "Startup cache: "+dirNoStubsFmt, d.name)
		}
		b.mu.Unlock()
	}
	return walked, whole
}

// processDirectory walks directory and processes all .mkv files
// If recursive is true, also processes subdirectories
func (b *StartupCacheBuilder) processDirectory(dirPath string, recursive bool) {
	// Semaphore to limit concurrent file operations
	// Too many concurrent reads can overwhelm the filesystem
	sem := make(chan struct{}, 10) // Max 10 concurrent
	var wg sync.WaitGroup

	// Walk directory
	err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			b.logger.Info(fmt.Sprintf("Startup cache: error accessing %s: %v", path, err), "path", path, "err", err)
			b.incrementErrors()
			return nil // Continue walking
		}

		// Skip directories if not recursive
		if info.IsDir() {
			if !recursive && path != dirPath {
				return filepath.SkipDir
			}
			// V133: Register directory in inode map, keyed by the full path the FUSE
			// call sites look up — a relative key leaves an entry nobody reads.
			if path != b.sourcePath {
				getDirInodeFromMap(path)
			}
			return nil
		}

		// Process only .mkv files
		if filepath.Ext(path) != ".mkv" {
			return nil
		}

		// Check if already in cache (fast path)
		if cached, ok := b.metaCache.Get(path); ok {
			// Skipped files still belong to the library: a FUSE lookup can fill the
			// cache while this walk runs, and their rows must not be pruned.
			b.noteHash(cached.URL)
			b.incrementSkipped()
			return nil
		}

		// Acquire semaphore
		wg.Add(1)
		sem <- struct{}{}

		// Process file in goroutine
		go func(filePath string) {
			defer wg.Done()
			defer func() { <-sem }()

			b.processFile(filePath)
		}(path)

		return nil
	})

	if err != nil {
		b.logger.Info(fmt.Sprintf("Startup cache: error walking %s: %v", dirPath, err), "dir_path", dirPath, "err", err)
		b.incrementErrors()
	}

	// Wait for all goroutines to complete
	wg.Wait()
}

// processFile reads metadata for a single file and adds to cache
func (b *StartupCacheBuilder) processFile(path string) {
	// Read metadata from file
	fileMeta, err := vfs.ReadMetadataFromFile(path)
	if errors.Is(err, vfs.ErrInvalidSize) {
		b.noteInvalidStub(path, err)
		return
	}
	if err != nil {
		b.logger.Info(fmt.Sprintf("Startup cache: error reading metadata for %s: %v", path, err), "path", path, "err", err)
		b.incrementErrors()
		return
	}

	// Convert to Metadata format
	meta := &vfs.Metadata{
		URL:    fileMeta.URL,
		Size:   fileMeta.Size,
		Mtime:  fileMeta.Mtime,
		Path:   fileMeta.Path,
		ImdbID: fileMeta.ImdbID,
	}

	// Calculate approximate size
	size := approximateMetadataSize(meta)

	// Add to cache
	b.metaCache.Put(path, meta, size)

	// V133: Add to inode map for deterministic inode generation
	// This ensures Plex sees the same inode after restarts
	// BUG FIX: Pass full path instead of just filename to avoid collisions
	addFileToInodeMap(path, fileMeta.URL)

	b.noteHash(fileMeta.URL)
	b.incrementProcessed(path)
}

// noteInvalidStub counts a stub that parses but declares a size outside the valid
// range. It is a known fault of that one file, not a walk error: it is logged with its
// path and the prunes still run. The stub is still in the library, so its inode entry
// and its hash are kept; they come from a second parse that accepts any size.
func (b *StartupCacheBuilder) noteInvalidStub(path string, cause error) {
	b.logger.Info(fmt.Sprintf("Startup cache: invalid stub %s: %v", path, cause), "path", path, "cause", cause)
	any := vfs.SizeLimits{Min: math.MinInt64, Max: math.MaxInt64}
	meta, err := vfs.ReadMetadataFromFileWithLimits(path, any)
	b.mu.Lock()
	b.invalidStubs++
	b.foundFiles[path] = true
	b.mu.Unlock()
	if err == nil {
		b.noteHash(meta.URL)
	}
}

// noteHash records the info hash of a stub's stream URL as part of the library.
func (b *StartupCacheBuilder) noteHash(url string) {
	hash, _ := vfs.ExtractHashAndIndex(url)
	if hash == "" {
		return
	}
	b.mu.Lock()
	b.foundHashes[hash] = true
	b.stubsSeen++
	b.mu.Unlock()
}

// libraryIfWhole returns the hashes of the stubs the walk found, only when the walk saw
// the library whole: a missing source directory, any walk error or an empty result
// means it did not, and an unmounted library must not wipe a table.
func (b *StartupCacheBuilder) libraryIfWhole(what string, complete bool) (map[string]bool, pruneResult, bool) {
	b.mu.Lock()
	errs, keep := b.errors, b.foundHashes
	b.mu.Unlock()
	gap := b.gap(true)
	if !guardWhy(b.logger, what, complete, errs, len(keep), gap, nil) {
		return nil, pruneResult{Status: pruneSkipped, Reason: pruneReason(complete, errs, len(keep), gap), guarded: true}, false
	}
	return keep, pruneResult{}, true
}

// pruneGuard is the "the walk was whole" test every prune shares: the walk saw both
// directories (complete), met no error and found at least one stub. It logs a skip;
// with a non-nil skipped it logs once per spell of skips, not once per call.
func pruneGuard(logger *slog.Logger, what string, complete bool, errs, stubs int, skipped *atomic.Bool) bool {
	return guardWhy(logger, what, complete, errs, stubs, "", skipped)
}

// guardWhy is pruneGuard with the walk's gap (a missing or stub-less directory) in the
// skip line and in the reason, so the log and the report say the same thing.
func guardWhy(logger *slog.Logger, what string, complete bool, errs, stubs int, gap string, skipped *atomic.Bool) bool {
	if complete && errs == 0 && stubs > 0 {
		if skipped != nil {
			skipped.Store(false)
		}
		return true
	}
	if skipped == nil || !skipped.Swap(true) {
		logger.Info(fmt.Sprintf("%s prune skipped: %s", what, pruneReason(complete, errs, stubs, gap)))
	}
	return false
}

// pruneReason is the part of the "prune skipped" line after the colon, and the reason
// the startup report carries. A gap (e.g. "movies holds no stubs") leads it.
func pruneReason(complete bool, errs, stubs int, gap string) string {
	r := fmt.Sprintf("complete=%v errors=%d stubs=%d", complete, errs, stubs)
	if gap != "" {
		return gap + " (" + r + ")"
	}
	return r
}

// gap names the directories the walk found missing and, with empty, those that held
// no stubs; "" when there were none.
func (b *StartupCacheBuilder) gap(empty bool) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	g := append([]string(nil), b.missingDirs...)
	if empty {
		g = append(g, b.emptyDirs...)
	}
	return strings.Join(g, ", ")
}

// pruneInodes drops the inode entries of files the walk did not find, under the same
// guard as the other prunes: a wrong or unmounted source path must not empty the file
// rows.
func (b *StartupCacheBuilder) pruneInodes(complete bool) {
	res := b.runPruneInodes(complete)
	b.mu.Lock()
	b.inodes = res
	b.mu.Unlock()
}

func (b *StartupCacheBuilder) runPruneInodes(complete bool) pruneResult {
	if globalInodeMap == nil {
		return pruneResult{Status: pruneSkipped, Reason: "inode map not initialised"}
	}
	b.mu.Lock()
	errs, found := b.errors, b.foundFiles
	n := len(found)
	b.mu.Unlock()
	gap := b.gap(false)
	if !guardWhy(b.logger, "Inode", complete, errs, n, gap, nil) {
		return pruneResult{Status: pruneSkipped, Reason: pruneReason(complete, errs, n, gap), guarded: true}
	}
	pruned := globalInodeMap.PruneMissing(found)
	if pruned > 0 {
		flog.Printf(b.logger, ghostInodesFmt, pruned)
	}
	return pruneResult{Status: pruneRan, RowsPruned: pruned}
}

// pruneReadability drops readability rows of torrents that left the library. It runs
// only after a complete walk. Rows touched since the process started are kept.
func (b *StartupCacheBuilder) pruneReadability(complete bool) {
	res := b.runPruneReadability(complete)
	b.mu.Lock()
	b.readability = res
	b.mu.Unlock()
}

func (b *StartupCacheBuilder) runPruneReadability(complete bool) pruneResult {
	if stateDB == nil {
		return pruneResult{Status: pruneSkipped, Reason: "state database off"}
	}
	keep, skipped, ok := b.libraryIfWhole("Readability", complete)
	if !ok {
		return skipped
	}
	n, err := stateDB.PruneReadability(keep, b.startTime)
	if err != nil {
		b.logger.Warn(fmt.Sprintf("WARNING: readability prune: %v", err), "err", err)
		return pruneResult{Status: pruneFailed, Reason: err.Error()}
	}
	if n > 0 {
		b.logger.Info(fmt.Sprintf("Readability prune: removed %d records of torrents no longer in the library", n), "n", n)
	}
	return pruneResult{Status: pruneRan, RowsPruned: n}
}

// pruneFileState drops the played and evicted marks of torrents that left the library,
// under the same guard as pruneReadability.
func (b *StartupCacheBuilder) pruneFileState(complete bool) {
	res := b.runPruneFileState(complete)
	b.mu.Lock()
	b.fileState = res
	b.mu.Unlock()
}

func (b *StartupCacheBuilder) runPruneFileState(complete bool) pruneResult {
	if stateDB == nil {
		return pruneResult{Status: pruneSkipped, Reason: "state database off"}
	}
	keep, skipped, ok := b.libraryIfWhole("File state", complete)
	if !ok {
		return skipped
	}
	dropped, err := stateDB.PruneFileStates(keep, b.startTime)
	if err != nil {
		b.logger.Warn(fmt.Sprintf("WARNING: file state prune: %v", err), "err", err)
		return pruneResult{Status: pruneFailed, Reason: err.Error()}
	}
	for _, h := range dropped {
		fileStates.ForgetMemory(h)
	}
	if len(dropped) > 0 {
		b.logger.Info(fmt.Sprintf("File state prune: removed the marks of %d torrents no longer in the library", len(dropped)))
	}
	return pruneResult{Status: pruneRan, RowsPruned: len(dropped)}
}

// warnUnstored counts the library torrents the engine stores no metainfo for, logs a
// WARNING naming the first few hashes (no paths) and sets libraryUnstored. With none
// missing it logs a plain line, so a log shows the count ran. It reads the
// engine database once per distinct hash, in this background goroutine after the walk,
// so it never delays the mount. It changes nothing.
func (b *StartupCacheBuilder) warnUnstored() {
	b.mu.Lock()
	hashes := make([]string, 0, len(b.foundHashes))
	for h := range b.foundHashes {
		hashes = append(hashes, h)
	}
	b.mu.Unlock()
	if len(hashes) == 0 || b.hasMetainfo == nil {
		return
	}
	sort.Strings(hashes)
	var missing []string
	for _, h := range hashes {
		if !b.hasMetainfo(h) {
			missing = append(missing, h)
		}
	}
	n := len(missing)
	libraryUnstored.Store(&n)
	b.mu.Lock()
	b.unstoredRan, b.unstoredN, b.unstoredOf = true, n, len(hashes)
	b.mu.Unlock()
	if n == 0 {
		b.logger.Info(fmt.Sprintf("Library torrents with no stored engine record with metainfo: 0 of %d", len(hashes)))
		return
	}
	shown := missing[:min(n, unstoredLogHashes)]
	b.logger.Warn(fmt.Sprintf("WARNING: %d of %d library torrents have no stored engine record with metainfo (first %d: %s); one opened after an idle close is refused until it is added again", n, len(hashes), len(shown), strings.Join(shown, " ")), "n", n)
}

// Statistics helpers (thread-safe)
func (b *StartupCacheBuilder) incrementProcessed(path string) {
	b.mu.Lock()
	b.filesProcessed++
	b.foundFiles[path] = true // V142: Mark as found
	b.mu.Unlock()
}

func (b *StartupCacheBuilder) incrementSkipped() {
	b.mu.Lock()
	b.filesSkipped++
	b.mu.Unlock()
}

func (b *StartupCacheBuilder) incrementErrors() {
	b.mu.Lock()
	b.errors++
	b.mu.Unlock()
}
