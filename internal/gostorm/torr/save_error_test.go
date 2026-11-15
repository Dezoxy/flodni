package torr

import (
	"errors"
	"testing"

	sets "flodni/internal/gostorm/settings"

	"github.com/anacrolix/torrent"
)

// A save whose commit failed is not saved: it reports false, counts, and leaves the
// debounce unstamped so the next save retries at once instead of waiting 30 s.
func TestFailedCommitDoesNotDebounceTheRetry(t *testing.T) {
	warmupFixture(t)
	h := lifeHash(30)
	tr := register(bts, &torrent.TorrentSpec{InfoHash: h, InfoBytes: []byte("info")})

	setBefore, _, _ := sets.DBWriteErrors()
	restore := sets.InjectCommitFault(func() error { return errors.New("injected: disk full") })
	defer restore()

	if saveIfHeld(tr, false) {
		t.Error("saveIfHeld reported saved for a failed commit")
	}
	SaveTorrentToDB(tr)
	if _, stamped := lastSaveTimes.Load(h); stamped {
		t.Error("a failed commit stamped the save debounce")
	}
	if GetTorrentDB(h) != nil {
		t.Fatal("record present after failed commits")
	}
	if set, _, _ := sets.DBWriteErrors(); set < setBefore+2 {
		t.Errorf("set errors %d -> %d, want at least +2", setBefore, set)
	}

	restore()
	SaveTorrentToDB(tr) // immediately: the debounce must not hold it back
	if GetTorrentDB(h) == nil {
		t.Fatal("the retry right after recovery did not commit the record")
	}
	if _, stamped := lastSaveTimes.Load(h); !stamped {
		t.Error("a committed save did not stamp the debounce")
	}
}

func TestSuccessfulSaveDoesNotCountAWriteError(t *testing.T) {
	warmupFixture(t)
	h := lifeHash(31)
	tr := register(bts, &torrent.TorrentSpec{InfoHash: h, InfoBytes: []byte("info")})
	set, rem, clr := sets.DBWriteErrors()
	if !saveIfHeld(tr, false) {
		t.Fatal("saveIfHeld = false for a held torrent")
	}
	if s, r, c := sets.DBWriteErrors(); s != set || r != rem || c != clr {
		t.Errorf("counters moved on a successful save")
	}
}
