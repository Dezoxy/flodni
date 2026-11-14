package daemon

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flodni/internal/cache"
	"flodni/internal/metadb"
	"flodni/internal/promtext"
	"flodni/internal/vfs"
)

const (
	inLibHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	orphHash  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func pruneEnv(t *testing.T) (*StartupCacheBuilder, *metadb.DB) {
	t.Helper()
	db, err := metadb.New(filepath.Join(t.TempDir(), "state.db"), discardLogger{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	prev := stateDB
	stateDB = db
	t.Cleanup(func() { stateDB = prev })

	old := time.Now().Add(-time.Hour)
	for _, h := range []string{inLibHash, orphHash} {
		if err := db.RecordMetadataFailure(h, failReasonTimeout, failSourceMount, old); err != nil {
			t.Fatal(err)
		}
	}
	b := NewStartupCacheBuilder(t.TempDir(), cache.NewLRUCache(1<<20, time.Hour), log.New(io.Discard, "", 0))
	b.foundHashes[inLibHash] = true
	return b, db
}

func rowCount(t *testing.T, db *metadb.DB) int {
	t.Helper()
	rows, err := db.ListReadability(0)
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

func TestPruneReadabilityGuards(t *testing.T) {
	tests := []struct {
		name     string
		complete bool
		errors   int
		empty    bool
	}{
		{"incomplete walk", false, 0, false},
		{"walk errors", true, 2, false},
		{"empty stub set", true, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, db := pruneEnv(t)
			b.errors = tt.errors
			if tt.empty {
				b.foundHashes = map[string]bool{}
			}
			b.pruneReadability(tt.complete)
			if got := rowCount(t, db); got != 2 {
				t.Fatalf("rows = %d, want 2: nothing may be deleted", got)
			}
		})
	}
}

func TestPruneReadabilityDeletesOnlyTheOrphanAfterACompleteWalk(t *testing.T) {
	b, db := pruneEnv(t)
	b.pruneReadability(true)
	rows, err := db.ListReadability(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Hash != inLibHash {
		t.Fatalf("rows = %+v, want only the in-library hash", rows)
	}
}

func TestPruneReadabilityNeedsTheStateDatabase(t *testing.T) {
	b, _ := pruneEnv(t)
	stateDB = nil
	b.pruneReadability(true) // must not panic
}

func TestSkippedStubStillLandsInFoundHashes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Movie_2000_1080p_cafe.mkv")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	mc := cache.NewLRUCache(1<<20, time.Hour)
	mc.Put(path, &vfs.Metadata{URL: "http://engine/stream?link=" + inLibHash + "&index=1&play"}, 100)
	b := NewStartupCacheBuilder(dir, mc, log.New(io.Discard, "", 0))

	b.processDirectory(dir, false)

	if !b.foundHashes[inLibHash] {
		t.Fatalf("foundHashes = %v, want the cached stub's hash", b.foundHashes)
	}
	if b.filesSkipped != 1 || b.filesProcessed != 0 {
		t.Fatalf("skipped %d, processed %d: the stub should have taken the skip path", b.filesSkipped, b.filesProcessed)
	}
}

// pruneFileEnv adds one played row for the in-library hash and one for the orphan, both
// stamped before the builder started, and the same marks in memory.
func pruneFileEnv(t *testing.T) (*StartupCacheBuilder, *metadb.DB) {
	t.Helper()
	b, db := pruneEnv(t)
	old := time.Now().Add(-time.Hour)
	prev := fileStates
	fileStates = newFileStateIndex(nil, 90)
	t.Cleanup(func() { fileStates = prev })
	for _, h := range []string{inLibHash, orphHash} {
		if err := db.MarkFilePlayed(h, 1, old); err != nil {
			t.Fatal(err)
		}
		fileStates.MarkPlayed(fk(h, 1))
	}
	return b, db
}

func fileStateRows(t *testing.T, db *metadb.DB) int {
	t.Helper()
	rows, err := db.LoadFileStates()
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

func TestPruneFileStateGuards(t *testing.T) {
	tests := []struct {
		name     string
		complete bool
		errors   int
		empty    bool
	}{
		{"incomplete walk", false, 0, false},
		{"walk errors", true, 2, false},
		{"empty stub set", true, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, db := pruneFileEnv(t)
			b.errors = tt.errors
			if tt.empty {
				b.foundHashes = map[string]bool{}
			}
			b.pruneFileState(tt.complete)
			if got := fileStateRows(t, db); got != 2 {
				t.Fatalf("rows = %d, want 2: nothing may be deleted", got)
			}
			if !fileStates.EverPlayed(fk(orphHash, 1)) {
				t.Fatal("memory was forgotten although nothing was pruned")
			}
		})
	}
}

func TestPruneFileStateDeletesOnlyTheOrphanAfterACompleteWalk(t *testing.T) {
	b, db := pruneFileEnv(t)
	b.pruneFileState(true)
	rows, err := db.LoadFileStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Hash != inLibHash {
		t.Fatalf("rows = %+v, want only the in-library hash", rows)
	}
	if fileStates.EverPlayed(fk(orphHash, 1)) || !fileStates.EverPlayed(fk(inLibHash, 1)) {
		t.Fatal("memory must forget the orphan and keep the library file")
	}
}

func TestPruneFileStateNeedsTheStateDatabase(t *testing.T) {
	b, _ := pruneFileEnv(t)
	stateDB = nil
	b.pruneFileState(true) // must not panic
	if !fileStates.EverPlayed(fk(orphHash, 1)) {
		t.Fatal("memory changed with no database")
	}
}

func unstoredEnv(t *testing.T, stored map[string]bool, hashes ...string) (*StartupCacheBuilder, *strings.Builder) {
	t.Helper()
	libraryUnstored.Store(nil)
	t.Cleanup(func() { libraryUnstored.Store(nil) })
	var out strings.Builder
	b := NewStartupCacheBuilder(t.TempDir(), cache.NewLRUCache(1<<20, time.Hour), log.New(&out, "", 0))
	for _, h := range hashes {
		b.foundHashes[h] = true
	}
	b.hasMetainfo = func(h string) bool { return stored[h] }
	return b, &out
}

func TestWarnUnstoredNamesTheFirstFewHashes(t *testing.T) {
	var hashes []string
	for i := 0; i < 8; i++ {
		hashes = append(hashes, strings.Repeat(string(rune('a'+i)), 40))
	}
	// h = "hhh...", the last, is stored; the other seven are not.
	b, out := unstoredEnv(t, map[string]bool{hashes[7]: true}, hashes...)
	b.warnUnstored()

	n := libraryUnstored.Load()
	if n == nil || *n != 7 {
		t.Fatalf("libraryUnstored = %v, want 7", n)
	}
	line := out.String()
	if !strings.Contains(line, "WARNING: 7 of 8 library torrents have no stored engine record with metainfo") {
		t.Errorf("line = %q", line)
	}
	for _, h := range hashes[:5] {
		if !strings.Contains(line, h) {
			t.Errorf("line lacks %s: %q", h, line)
		}
	}
	for _, h := range hashes[5:] {
		if strings.Contains(line, h) {
			t.Errorf("line names %s beyond the first five: %q", h, line)
		}
	}
	if strings.Contains(line, b.sourcePath) {
		t.Errorf("line carries a path: %q", line)
	}
}

func TestWarnUnstoredLogsAPlainZeroWhenEverythingIsStored(t *testing.T) {
	b, out := unstoredEnv(t, map[string]bool{inLibHash: true, orphHash: true}, inLibHash, orphHash)
	b.warnUnstored()
	if n := libraryUnstored.Load(); n == nil || *n != 0 {
		t.Fatalf("libraryUnstored = %v, want 0", n)
	}
	line := out.String()
	if !strings.Contains(line, "no stored engine record with metainfo: 0 of 2") || strings.Contains(line, "WARNING") {
		t.Errorf("line = %q, want a plain 0 of 2", line)
	}
}

func TestWarnUnstoredLeavesAnEmptyWalkUncounted(t *testing.T) {
	b, out := unstoredEnv(t, nil)
	b.warnUnstored()
	if libraryUnstored.Load() != nil || out.Len() != 0 {
		t.Errorf("empty library: gauge %v, log %q", libraryUnstored.Load(), out.String())
	}
}

func TestUnstoredGaugeIsAbsentUntilCountedThenExported(t *testing.T) {
	b, _ := unstoredEnv(t, map[string]bool{inLibHash: true}, inLibHash, orphHash)
	var w promtext.Writer
	collectUnstoredProm(&w)
	if strings.Contains(string(w.Bytes()), "flodni_library_torrents_unstored") {
		t.Fatalf("series present before the count: %s", w.Bytes())
	}
	b.warnUnstored()
	w = promtext.Writer{}
	collectUnstoredProm(&w)
	if !strings.Contains(string(w.Bytes()), "flodni_library_torrents_unstored 1\n") {
		t.Fatalf("series missing or wrong: %s", w.Bytes())
	}
	if err := promtext.Lint(w.Bytes()); err != nil {
		t.Fatal(err)
	}
}

// pruneInodeEnv registers two files in the inode map; the builder's walk found one.
func pruneInodeEnv(t *testing.T) (*StartupCacheBuilder, *vfs.InodeMap) {
	t.Helper()
	im := vfs.NewInodeMap(filepath.Join(t.TempDir(), "inodes.json"), &vfsLogger{log.New(io.Discard, "", 0)})
	prev := globalInodeMap
	globalInodeMap = im
	t.Cleanup(func() { globalInodeMap = prev })
	im.AddFile("/lib/movies/a.mkv", inLibHash, 1)
	im.AddFile("/lib/movies/b.mkv", orphHash, 1)
	b := NewStartupCacheBuilder(t.TempDir(), cache.NewLRUCache(1<<20, time.Hour), log.New(io.Discard, "", 0))
	b.foundFiles["/lib/movies/a.mkv"] = true
	b.foundHashes[inLibHash] = true
	return b, im
}

func inodeFiles(im *vfs.InodeMap) int {
	files, _, _, _ := im.Stats()
	return int(files)
}

func TestPruneInodesGuards(t *testing.T) {
	tests := []struct {
		name     string
		complete bool
		errors   int
		empty    bool
	}{
		{"incomplete walk", false, 0, false},
		{"walk errors", true, 2, false},
		{"empty result", true, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, im := pruneInodeEnv(t)
			b.errors = tt.errors
			if tt.empty {
				b.foundFiles = map[string]bool{}
			}
			b.pruneInodes(tt.complete)
			if got := inodeFiles(im); got != 2 {
				t.Fatalf("files = %d, want 2: nothing may be pruned", got)
			}
		})
	}
}

func TestPruneInodesDropsTheGhostAfterAWholeWalk(t *testing.T) {
	b, im := pruneInodeEnv(t)
	b.pruneInodes(true)
	if got := inodeFiles(im); got != 1 {
		t.Fatalf("files = %d, want only the found one", got)
	}
}

// A movies or tv directory that exists and holds no stubs (a mount that did not come
// up) must not make the walk count as whole: the other directory's stubs would then
// keep the prune from sparing the rows of every torrent filed in the empty one.
func TestWalkLibraryNeedsStubsInBothDirectories(t *testing.T) {
	for _, tc := range []struct {
		name       string
		movies, tv bool // which directory holds a stub
		noTVDir    bool
		wantWalked bool
		wantWhole  bool
	}{
		{"both hold stubs", true, true, false, true, true},
		{"movies empty", false, true, false, true, false},
		{"tv empty", true, false, false, true, false},
		{"tv missing", true, false, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			mc := cache.NewLRUCache(1<<20, time.Hour)
			for _, d := range []struct {
				dir  string
				has  bool
				skip bool
			}{{"movies", tc.movies, false}, {"tv", tc.tv, tc.noTVDir}} {
				if d.skip {
					continue
				}
				dir := filepath.Join(root, d.dir)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if !d.has {
					continue
				}
				path := filepath.Join(dir, "Title_"+d.dir+".mkv")
				if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
				mc.Put(path, &vfs.Metadata{URL: "http://engine/stream?link=" + inLibHash + "&index=1&play"}, 100)
			}
			b := NewStartupCacheBuilder(root, mc, log.New(io.Discard, "", 0))

			walked, whole := b.walkLibrary()

			if walked != tc.wantWalked || whole != tc.wantWhole {
				t.Fatalf("walked=%v whole=%v, want %v %v", walked, whole, tc.wantWalked, tc.wantWhole)
			}
		})
	}
}
