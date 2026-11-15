package daemon

import (
	"errors"
	"flodni/internal/gostorm/torr"
	"flodni/internal/gostorm/torr/state"
	"flodni/internal/library"
	"flodni/internal/promtext"
	"flodni/internal/warmup"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/anacrolix/torrent/metainfo"

	"flodni/internal/gostorm/web"
)

// Index drift (Dezoxy fork): a stub names its file by the index the engine gave it when
// the stub was written. An older stub, or an engine that numbered the files another way,
// can leave that index naming a different file. These helpers decide which file an open
// serves, and count the stubs found stale.

// engineFileList is the resident torrent's files in the engine's own id order, nil when
// the torrent is not resident. A var so a test can stand in the engine.
var engineFileList = residentFiles

func residentFiles(hash string) []library.TorrentFile {
	t := web.BTS.GetTorrent(metainfo.NewHashFromHex(hash))
	if t == nil {
		return nil
	}
	return filesFromStats(t.Status().FileStats)
}

// storedFileList is the file list saved with the torrent's record, read without waking
// the torrent: nil when there is none. A var so a test can stand in the database.
var storedFileList = func(hash string) []library.TorrentFile {
	return filesFromStats(torr.StoredFiles(hash))
}

// storedRecordList is every torrent record the engine stores, as hash and name only. A
// stored name that looks like a URL or a magnet link is left out, so an entry never
// carries a tracker address. Reads the database, so only the on-demand audit calls it.
func storedRecordList() []library.StoredRecord {
	dbs := torr.ListTorrentsDB()
	out := make([]library.StoredRecord, 0, len(dbs))
	for h, t := range dbs {
		name := t.Title
		if strings.Contains(name, "://") || strings.HasPrefix(strings.ToLower(name), "magnet:") {
			name = ""
		}
		out = append(out, library.StoredRecord{Hash: h.HexString(), Name: name})
	}
	return out
}

// knownFiles is the torrent's file list: the resident one, else the stored one.
func knownFiles(hash string) []library.TorrentFile {
	if files := engineFileList(hash); len(files) > 0 {
		return files
	}
	return storedFileList(hash)
}

// filesFromStats takes the ids and the order from the engine's own list (Status()'s
// FileStats, the list the engine reader resolves ids through). A fresh sort need not
// reproduce it: paths that tie in the comparison, such as 1.mkv and 01.mkv, can come out
// the other way round.
func filesFromStats(stats []*state.TorrentFileStat) []library.TorrentFile {
	files := make([]library.TorrentFile, 0, len(stats))
	for _, s := range stats {
		if s != nil {
			files = append(files, library.TorrentFile{Index: s.Id, Path: s.Path, Length: s.Length})
		}
	}
	return files
}

// suspectHead reports a head under the stub's own index that may belong to another
// file. With the torrent's file list known (resident, or the stored record: neither
// wakes it), the file at the stub's index must have the stub's length. Without a list,
// the head must not be longer than the stub's whole file, which only another file's
// head can be. Open runs it before choosing the wake: a suspect head makes a cold open,
// so the wake is synchronous and openTarget resolves against the engine's list.
func suspectHead(hash string, idx int, size int64) bool {
	if files := knownFiles(hash); len(files) > 0 {
		for _, f := range files {
			if f.Index == idx {
				return f.Length != size
			}
		}
		return true
	}
	return warmup.DiskWarmup.GetAvailableRange(hash, idx) > size
}

// openTarget decides the (hash, file index) an open serves, and whether the warm head
// under it is ready; err is set when nothing could be resolved (the open then relies on
// the cache and a retry), or is library.ErrStubMismatch, which Open turns into EIO.
// headReady says a head exists under the stub's own index.
//
// With the torrent's file list known, the one rule in library.ResolveOpenTarget decides:
// the stub's index when the file there has the stub's length, else the file of that
// length, else ErrStubMismatch. Every other pair than the stub's is counted. With no
// list (closed, no stored record) the stub is trusted, as Open has already refused a
// head longer than the stub's file.
func openTarget(url string, size int64, path, hashStr string, urlIdx int, headReady bool) (hash string, idx int, ready bool, err error) {
	var files []library.TorrentFile
	if hashStr != "" {
		files = knownFiles(hashStr)
	}
	if headReady && hashStr != "" && len(files) == 0 {
		return hashStr, urlIdx, true, nil
	}
	t, err := resolveOpenTarget(url, size, path, files, true)
	if err != nil {
		if errors.Is(err, library.ErrStubMismatch) {
			noteStubIndexMismatch(path, hashStr, urlIdx, 0, true)
			return "", 0, false, err
		}
		if headReady && hashStr != "" {
			return hashStr, urlIdx, true, nil // unresolvable (no bridge): today's shortcut
		}
		return "", 0, false, err
	}
	if hashStr != "" && urlIdx > 0 && t.FileIndex != urlIdx {
		noteStubIndexMismatch(path, hashStr, urlIdx, t.FileIndex, false)
	}
	switch {
	case !headReady:
		ready = false
	case t.Hash == hashStr && t.FileIndex == urlIdx:
		ready = true
	default:
		ready = warmup.DiskWarmup.HeadReady(t.Hash, t.FileIndex)
	}
	return t.Hash, t.FileIndex, ready, nil
}

// stubIndexMismatches counts the paths whose stub index differs from the file the open
// resolved, once per path (the same path opens many times).
var stubIndexMismatches stubIndexTripwire

type stubIndexTripwire struct {
	seen  sync.Map // path -> struct{}
	total atomic.Int64
}

func (s *stubIndexTripwire) reset() {
	s.seen.Clear()
	s.total.Store(0)
}

// noteStubIndexMismatch logs and counts the first open of a path whose stub index is
// not the file the open resolved.
func noteStubIndexMismatch(path, hash string, stubIdx, resolvedIdx int, unverified bool) {
	if _, dup := stubIndexMismatches.seen.LoadOrStore(path, struct{}{}); dup {
		return
	}
	stubIndexMismatches.total.Add(1)
	if unverified {
		logger.Warn(fmt.Sprintf("WARNING: stub index: %s names file %d of %s, which has another length than the stub, and no file has the stub's length; the stub is stale and the open fails with EIO (remove it from the library and add it again)", path, stubIdx, hash), "path", path, "stub_idx", stubIdx, "hash", hash)
		return
	}
	logger.Warn(fmt.Sprintf("WARNING: stub index: %s names file %d of %s but resolves to file %d; the stub is stale (remove it from the library and add it again)", path, stubIdx, hash, resolvedIdx), "path", path, "stub_idx", stubIdx, "hash", hash, "resolved_idx", resolvedIdx)
}

func collectStubIndexProm(w *promtext.Writer) {
	w.Counter("flodni_stub_index_mismatch_total",
		"Library paths whose stub file index differs from the file the open resolved by size and name, counted once per path per process. Non-zero means a stale stub: repair it with library remove and add.",
		promSample(stubIndexMismatches.total.Load()))
}
