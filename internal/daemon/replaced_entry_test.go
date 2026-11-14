package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"flodni/internal/library"
	"flodni/internal/vfs"
)

// removeOnlyEngine is the engine side of a library.Manager: it only has to accept the
// removal of a torrent.
type removeOnlyEngine struct{ library.GoStorm }

func (removeOnlyEngine) RemoveTorrent(context.Context, string) error { return nil }

// servedSession is an open session that has been served by the swarm: closing it
// stamps a read, which inserts the readability row.
func servedSession(path string) *TTFFSession {
	s := &TTFFSession{path: path, size: 1 << 30, hash: rHash}
	s.openedAt.Store(time.Now().Add(-time.Minute).UnixNano())
	s.lastOff.Store(-1)
	s.recordRead(srcFetchBlock, time.Millisecond, 4<<20, 0)
	sessions.Store(path, s)
	return s
}

func withDirCache(t *testing.T) {
	t.Helper()
	prev := globalDirCache
	globalDirCache = vfs.NewDirCache(time.Second)
	t.Cleanup(func() { globalDirCache = prev })
}

// A replaced stub's open session closes where the stub is invalidated, which the
// library does before it clears the row: nothing is left to write the row back when
// the idle reaper would have closed it.
func TestReplacementClosesTheOpenSessionBeforeTheRowIsCleared(t *testing.T) {
	db := readabilityEnv(t, 3, true)
	withDirCache(t)
	path := filepath.Join(t.TempDir(), "Show.S01E01_0123abcd.mkv")
	s := servedSession(path)
	if err := db.RecordMetadataFailure(rHash, failReasonTimeout, failSourceMount, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	invalidateSyncRemovedPath(path) // the library's deleteStub
	if !s.closed.Load() {
		t.Fatal("the session is still open after the stub was invalidated")
	}
	if err := db.ClearMetadataFailure(rHash); err != nil { // dropTorrentIfUnused
		t.Fatal(err)
	}

	s.closeSession() // what the idle reaper would do later
	if rows := records(t, db); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none for the replaced torrent", rows)
	}
}

// rm through the mount reaches the library's removal, which invalidates the stub and
// drops the unused torrent with its row.
func TestUnlinkThroughTheMountLeavesNoReadabilityRow(t *testing.T) {
	db := readabilityEnv(t, 3, true)
	useTestGlobals(t)
	withDirCache(t)

	movies := t.TempDir()
	stub := filepath.Join(movies, "Movie_2000_0123abcd.mkv")
	if err := library.WriteStub(stub, "http://engine/stream?link="+rHash+"&index=1&play", 1<<30, "", ""); err != nil {
		t.Fatal(err)
	}
	mgr := library.New(library.Config{
		MoviesDir: movies, TVDir: t.TempDir(), GoStorm: removeOnlyEngine{}, Registry: db,
		InvalidatePath: invalidateSyncRemovedPath, Logger: logger,
	})
	prev := unlinkRemove
	t.Cleanup(func() { unlinkRemove = prev })
	unlinkRemove = func(ctx context.Context, p string) error {
		_, err := mgr.Remove(ctx, library.RemoveRequest{Path: p})
		return err
	}

	s := servedSession(stub)
	if err := db.RecordMetadataFailure(rHash, failReasonTimeout, failSourceMount, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	d := &VirtualDirNode{physicalPath: movies}
	if errno := d.Unlink(context.Background(), filepath.Base(stub)); errno != 0 {
		t.Fatalf("Unlink = %v", errno)
	}
	s.closeSession() // the reaper, later
	if rows := records(t, db); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none after the stub was deleted", rows)
	}
}

// Closing at replacement follows the normal close rules: a session younger than the
// verdict floor gets no dead-swarm verdict for failing, an old one still does.
func TestSessionClosedByReplacementKeepsTheVerdictRules(t *testing.T) {
	for _, tc := range []struct {
		name     string
		age      time.Duration
		wantRows int
	}{
		{"young session", 5 * time.Second, 0},
		{"old session", time.Minute, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := readabilityEnv(t, 0, true)
			withDirCache(t)
			path := filepath.Join(t.TempDir(), "x.mkv")
			s := &TTFFSession{path: path, size: 1 << 30, hash: rHash}
			s.openedAt.Store(time.Now().Add(-tc.age).UnixNano())
			s.lastOff.Store(-1)
			s.readFailed.Store(true)
			sessions.Store(path, s)

			invalidateSyncRemovedPath(path)

			rows := records(t, db)
			if len(rows) != tc.wantRows {
				t.Fatalf("rows = %+v, want %d", rows, tc.wantRows)
			}
			if tc.wantRows == 1 && rows[0].FailCount != 1 {
				t.Fatalf("fail_count = %d, want the one verdict", rows[0].FailCount)
			}
		})
	}
}
