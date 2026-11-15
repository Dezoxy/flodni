package settings

import (
	"testing"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

func errCounts() [3]int64 {
	set, rem, clr := DBWriteErrors()
	return [3]int64{set, rem, clr}
}

func errRecord(b byte) *TorrentDB {
	rec := &TorrentDB{TorrentSpec: &torrent.TorrentSpec{InfoBytes: []byte("info")}, Title: "t"}
	rec.InfoHash = metainfo.Hash{b}
	return rec
}

// A failed commit reaches the caller of AddTorrent and moves the set counter only.
func TestFailedCommitIsReportedAndCounted(t *testing.T) {
	withFreshDB(t, t.TempDir())
	flaky := &flakyDB{TDB: NewTDB().(*TDB)}
	tdb = NewDBReadCache(flaky)

	before := errCounts()
	flaky.fail = true
	if err := AddTorrent(errRecord(1)); err == nil {
		t.Fatal("AddTorrent returned nil for a failed commit")
	}
	after := errCounts()
	if after[0] != before[0]+1 || after[1] != before[1] || after[2] != before[2] {
		t.Errorf("counters %v -> %v, want only set +1", before, after)
	}
}

func TestSuccessfulCommitIsNotCounted(t *testing.T) {
	withFreshDB(t, t.TempDir())
	tdb = NewDBReadCache(&flakyDB{TDB: NewTDB().(*TDB)})

	before := errCounts()
	if err := AddTorrent(errRecord(2)); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	if err := AddTorrent(errRecord(2)); err != nil { // unchanged: not committed again
		t.Fatalf("AddTorrent again: %v", err)
	}
	if err := RemTorrent(errRecord(2).InfoHash); err != nil {
		t.Fatalf("RemTorrent: %v", err)
	}
	if after := errCounts(); after != before {
		t.Errorf("counters moved on success: %v -> %v", before, after)
	}
}

func TestFailedRemAndClearAreCounted(t *testing.T) {
	withFreshDB(t, t.TempDir())
	flaky := &flakyDB{TDB: NewTDB().(*TDB)}
	tdb = NewDBReadCache(flaky)
	if err := AddTorrent(errRecord(3)); err != nil {
		t.Fatal(err)
	}

	before := errCounts()
	flaky.fail = true
	if err := RemTorrent(errRecord(3).InfoHash); err == nil {
		t.Error("RemTorrent returned nil for a failed commit")
	}
	tdb.Clear("Torrents")
	after := errCounts()
	if after[0] != before[0] || after[1] != before[1]+1 || after[2] != before[2]+1 {
		t.Errorf("counters %v -> %v, want rem +1 and clear +1", before, after)
	}
	flaky.fail = false
	if got := GetTorrent(errRecord(3).InfoHash); got == nil {
		t.Error("the record is gone although its removal and clear failed")
	}
}

// The daemon's database is the cache over the router over bbolt: the router must pass the
// commit result up, or the cache keeps a record that was never written.
func TestRouterReportsFailedCommit(t *testing.T) {
	withFreshDB(t, t.TempDir())
	flaky := &flakyDB{TDB: NewTDB().(*TDB)}
	setupDatabaseRouting(flaky, NewJsonDB(), false)

	before := errCounts()
	flaky.fail = true
	rec := errRecord(4)
	if err := AddTorrent(rec); err == nil {
		t.Fatal("AddTorrent returned nil through the router for a failed commit")
	}
	if errCounts()[0] != before[0]+1 {
		t.Errorf("set counter did not move through the router")
	}
	if got := tdb.Get("Torrents", rec.InfoHash.HexString()); got != nil {
		t.Errorf("cache holds a record that was never committed: %q", got)
	}
	flaky.fail = false
	if err := AddTorrent(rec); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if GetTorrent(rec.InfoHash) == nil {
		t.Error("retry did not commit the record")
	}
}
