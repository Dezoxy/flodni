package torr

import (
	"errors"
	"testing"
	"time"

	sets "flodni/internal/gostorm/settings"
	"flodni/internal/gostorm/torr/state"

	"github.com/anacrolix/torrent"
)

// holdWithInfo registers a wrapper whose metadata counts as arrived (the preload state
// answers GotInfo at once), the way an upload leaves one.
func holdWithInfo(spec *torrent.TorrentSpec) *Torrent {
	tr := register(bts, spec)
	tr.Stat = state.TorrentPreload
	return tr
}

// An add that asks for a record answers only after the record is committed.
func TestSaveAddedWritesTheRecordBeforeItReturns(t *testing.T) {
	warmupFixture(t)
	h := lifeHash(40)
	tr := holdWithInfo(&torrent.TorrentSpec{InfoHash: h, InfoBytes: []byte("info")})

	if err := SaveAdded(tr, "Some_Name", 0); err != nil {
		t.Fatalf("SaveAdded: %v", err)
	}
	rec := GetTorrentDB(h)
	if rec == nil {
		t.Fatal("no record when SaveAdded returned")
	}
	if rec.Title != "Some Name" {
		t.Errorf("title = %q, want the display name cleaned", rec.Title)
	}
}

// A commit that fails is an error to the caller, and no record exists.
func TestSaveAddedFailsWhenTheStoreFails(t *testing.T) {
	warmupFixture(t)
	h := lifeHash(41)
	tr := holdWithInfo(&torrent.TorrentSpec{InfoHash: h, InfoBytes: []byte("info")})
	restore := sets.InjectCommitFault(func() error { return errors.New("injected: disk full") })
	defer restore()

	err := SaveAdded(tr, "n", 0)
	if !errors.Is(err, ErrNotSaved) {
		t.Fatalf("err = %v, want ErrNotSaved", err)
	}
	if GetTorrentDB(h) != nil {
		t.Fatal("a record exists after a failed commit")
	}
}

// The debounce must not turn an add into a silent success: a save made a moment ago for
// this hash does not excuse the add from writing its own record.
func TestSaveAddedIgnoresTheSaveDebounce(t *testing.T) {
	warmupFixture(t)
	h := lifeHash(42)
	tr := holdWithInfo(&torrent.TorrentSpec{InfoHash: h, InfoBytes: []byte("info")})
	lastSaveTimes.Store(h, time.Now())
	t.Cleanup(func() { lastSaveTimes.Delete(h) })

	if err := SaveAdded(tr, "n", 0); err != nil {
		t.Fatal(err)
	}
	if GetTorrentDB(h) == nil {
		t.Fatal("the debounce swallowed the add's save")
	}
}

// No metadata before the timeout: an error naming it, and no record.
func TestSaveAddedFailsOnAMetadataTimeout(t *testing.T) {
	warmupFixture(t)
	h := lifeHash(43)
	tr := register(bts, &torrent.TorrentSpec{InfoHash: h}) // no anacrolix torrent: WaitInfo gives up
	err := SaveAdded(tr, "n", 0)
	if !errors.Is(err, ErrNoMetadata) {
		t.Fatalf("err = %v, want ErrNoMetadata", err)
	}
	if GetTorrentDB(h) != nil {
		t.Fatal("a record exists for a torrent with no metadata")
	}
}

// A torrent removed between the add and the save has nothing to save: not a success.
func TestSaveAddedFailsWhenTheTorrentIsNoLongerHeld(t *testing.T) {
	warmupFixture(t)
	h := lifeHash(44)
	tr := holdWithInfo(&torrent.TorrentSpec{InfoHash: h, InfoBytes: []byte("info")})
	bts.mu.Lock()
	delete(bts.torrents, h)
	bts.mu.Unlock()
	if err := SaveAdded(tr, "n", 0); !errors.Is(err, ErrNotSaved) {
		t.Fatalf("err = %v, want ErrNotSaved", err)
	}
}

// An upload leaves the torrent held with its metadata; the add that follows (a magnet
// for the same hash) finds it, keeps the uploaded info bytes and saves them.
func TestAddAfterUploadKeepsTheUploadedMetadata(t *testing.T) {
	warmupFixture(t)
	useFakeAdder(t, false)
	h := lifeHash(45)
	up := &torrent.TorrentSpec{InfoHash: h, InfoBytes: []byte("uploaded-info"), Trackers: [][]string{{"http://t.example/announce"}}}
	first, err := AddTorrent(up, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	first.Stat = state.TorrentPreload

	second, err := AddTorrent(&torrent.TorrentSpec{InfoHash: h}, "Title", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatal("the add built a second wrapper for the uploaded hash")
	}
	if err := SaveAdded(second, "Title", 0); err != nil {
		t.Fatal(err)
	}
	rec := GetTorrentDB(h)
	if rec == nil || string(rec.TorrentSpec.InfoBytes) != "uploaded-info" {
		t.Fatalf("record %+v, want the uploaded info bytes", rec)
	}
}

// The caller's wait bounds the metadata wait: a torrent whose metadata never comes is given
// up on at the caller's limit, closed, and no record is written.
func TestSaveAddedHonoursTheCallersWait(t *testing.T) {
	warmupFixture(t)
	h := lifeHash(46)
	tr := register(bts, &torrent.TorrentSpec{InfoHash: h})
	release := make(chan struct{})
	prev := waitInfo
	waitInfo = func(*Torrent) bool { <-release; return false }
	defer func() { close(release); waitInfo = prev }()

	start := time.Now()
	err := SaveAdded(tr, "n", 150*time.Millisecond)
	if !errors.Is(err, ErrNoMetadata) {
		t.Fatalf("err = %v, want ErrNoMetadata", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v for a 150 ms wait", d)
	}
	if GetTorrentDB(h) != nil {
		t.Fatal("a record exists for a torrent that gave no metadata")
	}
	if heldByEngine(h) != nil {
		t.Fatal("the torrent that gave no metadata is still held")
	}
}
