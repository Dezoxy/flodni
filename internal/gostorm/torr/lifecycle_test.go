package torr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sets "flodni/internal/gostorm/settings"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

// These tests pin lifeMu: a removal and a wake, a save or an idle close of the same
// hash are one step against each other, so a removed torrent stays removed. The
// engine is replaced by newTorrent's seam: it registers a wrapper in the BTServer
// the way NewTorrent does, with no client.

func lifeHash(i int) metainfo.Hash { return metainfo.NewHashFromHex(fmt.Sprintf("%040x", i+1)) }

// putRecord stores a record the way a save does, with metainfo unless info is nil.
func putRecord(h metainfo.Hash, info []byte, title string) {
	sets.AddTorrent(&sets.TorrentDB{
		TorrentSpec: &torrent.TorrentSpec{InfoHash: h, InfoBytes: info, Trackers: [][]string{{"http://seeder.example/announce"}}},
		Title:       title, Category: "movie", Poster: "p", Data: "d", Size: 7, Timestamp: 42,
	})
}

// register adds a wrapper to the engine's map, as NewTorrent does.
func register(bt *BTServer, spec *torrent.TorrentSpec) *Torrent {
	tr := &Torrent{bt: bt, TorrentSpec: spec, expiredTime: time.Now().Add(-time.Second)}
	bt.mu.Lock()
	bt.torrents[spec.InfoHash] = tr
	bt.mu.Unlock()
	return tr
}

// fakeAdder stands in for NewTorrent. gate, when set, blocks every call until it is closed.
type fakeAdder struct {
	calls     atomic.Int32
	infoBytes atomic.Int32 // length of the last spec's InfoBytes
	gate      chan struct{}
	entered   chan struct{} // a call reached the adder; tests that wait for it drain it
}

func useFakeAdder(t *testing.T, gate bool) *fakeAdder {
	t.Helper()
	a := &fakeAdder{entered: make(chan struct{}, 100)}
	if gate {
		a.gate = make(chan struct{})
	}
	prev := newTorrent
	newTorrent = func(spec *torrent.TorrentSpec, bt *BTServer) (*Torrent, error) {
		a.calls.Add(1)
		a.infoBytes.Store(int32(len(spec.InfoBytes)))
		select {
		case a.entered <- struct{}{}:
		default: // nobody is waiting for the signal
		}
		if a.gate != nil {
			<-a.gate
		}
		bt.mu.Lock()
		if held, ok := bt.torrents[spec.InfoHash]; ok {
			bt.mu.Unlock()
			return held, nil
		}
		bt.mu.Unlock()
		return register(bt, spec), nil
	}
	t.Cleanup(func() { newTorrent = prev })
	return a
}

func heldByEngine(h metainfo.Hash) *Torrent {
	btsMu.RLock()
	defer btsMu.RUnlock()
	return bts.GetTorrent(h)
}

func assertGone(t *testing.T, h metainfo.Hash) {
	t.Helper()
	if heldByEngine(h) != nil {
		t.Errorf("%s: still held by the engine", h.HexString())
	}
	if GetTorrentDB(h) != nil {
		t.Errorf("%s: still has a record", h.HexString())
	}
}

func TestRemoveStaysRemovedUnderConcurrentWakesSavesAndPeeks(t *testing.T) {
	_, tail := warmupFixture(t)
	useFakeAdder(t, false)
	dir := filepath.Dir(tail)

	const n = 50
	hashes := make([]metainfo.Hash, n)
	for i := range hashes {
		hashes[i] = lifeHash(i)
		putRecord(hashes[i], []byte("info"), fmt.Sprintf("title %d", i))
		for _, suffix := range []string{"-1.warmup", "-1.warmup-tail"} {
			if err := os.WriteFile(filepath.Join(dir, hashes[i].HexString()+suffix), make([]byte, 4096), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	loop := func(f func(h metainfo.Hash)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				f(hashes[i%n])
			}
		}()
	}
	loop(func(h metainfo.Hash) { _, _ = LoadStored(h.HexString()) })
	loop(func(h metainfo.Hash) { _ = PeekTorrent(h.HexString()) })
	loop(func(h metainfo.Hash) { // an idle close that holds a stale wrapper
		if tr := heldByEngine(h); tr != nil {
			ForceSaveTorrentToDB(tr)
			tr.UpdateStats()
		}
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, h := range hashes {
			RemTorrent(h.HexString())
			// Whatever the other goroutines do next, this hash stays removed.
			assertGone(t, h)
			if _, err := LoadStored(h.HexString()); !errors.Is(err, ErrNotStored) {
				t.Errorf("%s: LoadStored after RemTorrent = %v, want ErrNotStored", h.HexString(), err)
			}
		}
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("deadlock: RemTorrent loop did not finish")
	}
	close(stop)
	wg.Wait()

	for _, h := range hashes {
		assertGone(t, h)
		for _, suffix := range []string{"-1.warmup", "-1.warmup-tail"} {
			p := filepath.Join(dir, h.HexString()+suffix)
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Errorf("%s survived RemTorrent (stat err = %v)", filepath.Base(p), err)
			}
		}
	}
}

// A wake that has read the record and is adding it must finish before a removal of
// the same hash starts: the removal then takes the wrapper and the record both.
func TestRemovalWaitsForTheWakeThatIsAdding(t *testing.T) {
	warmupFixture(t)
	a := useFakeAdder(t, true)
	h := lifeHash(0)
	putRecord(h, []byte("info"), "title")

	loaded := make(chan error, 1)
	go func() {
		_, err := LoadStored(h.HexString())
		loaded <- err
	}()
	<-a.entered // the wake is inside newTorrent, holding lifeMu

	removed := make(chan struct{})
	go func() {
		RemTorrent(h.HexString())
		close(removed)
	}()
	select {
	case <-removed:
		t.Fatal("RemTorrent ran while a wake was adding the torrent")
	case <-time.After(100 * time.Millisecond):
	}
	close(a.gate)
	if err := <-loaded; err != nil {
		t.Fatalf("LoadStored: %v", err)
	}
	select {
	case <-removed:
	case <-time.After(10 * time.Second):
		t.Fatal("RemTorrent did not finish after the wake did")
	}
	assertGone(t, h)
	if a.infoBytes.Load() == 0 {
		t.Error("the wake did not carry the record's InfoBytes")
	}
}

// The other order: after the removal there is no record, so the wake refuses and
// never reaches the adder.
func TestWakeAfterRemovalIsRefusedWithoutAdding(t *testing.T) {
	warmupFixture(t)
	a := useFakeAdder(t, false)
	h := lifeHash(1)
	putRecord(h, []byte("info"), "title")
	RemTorrent(h.HexString())

	if _, err := LoadStored(h.HexString()); !errors.Is(err, ErrNotStored) {
		t.Fatalf("LoadStored = %v, want ErrNotStored", err)
	}
	if a.calls.Load() != 0 {
		t.Fatalf("the adder was called %d times, want 0", a.calls.Load())
	}
	assertGone(t, h)
}

// An idle close that lost the race to RemTorrent holds the wrapper it was about to
// save: it must not write the record back.
func TestIdleCloseAfterRemovalWritesNoRecord(t *testing.T) {
	warmupFixture(t)
	h := lifeHash(2)
	putRecord(h, []byte("info"), "title")
	spec := &torrent.TorrentSpec{InfoHash: h, InfoBytes: []byte("info")}
	tr := register(bts, spec)

	RemTorrent(h.HexString())
	tr.UpdateStats() // expired: it saves, then closes
	assertGone(t, h)
}

// The control for the test above: a held, expired wrapper does get its final save.
func TestIdleCloseOfAHeldWrapperSavesIt(t *testing.T) {
	warmupFixture(t)
	h := lifeHash(3)
	tr := register(bts, &torrent.TorrentSpec{InfoHash: h, InfoBytes: []byte("info")})
	if GetTorrentDB(h) != nil {
		t.Fatal("setup: the record exists already")
	}
	tr.UpdateStats()
	if GetTorrentDB(h) == nil {
		t.Fatal("an idle close of a held torrent saved no record")
	}
}

func TestSetTorrentUpdatesOnlyAnExistingRecord(t *testing.T) {
	warmupFixture(t)
	h := lifeHash(4)
	if got := SetTorrent(h.HexString(), "late rename", "", "", ""); got != nil || GetTorrentDB(h) != nil {
		t.Fatalf("SetTorrent created a record for a hash the engine does not store (got %v)", got)
	}
	putRecord(h, []byte("info"), "title")
	SetTorrent(h.HexString(), "renamed", "", "movie", "")
	if rec := GetTorrentDB(h); rec == nil || rec.Title != "renamed" {
		t.Fatalf("SetTorrent did not update the existing record: %+v", rec)
	}
}

func TestLoadStored(t *testing.T) {
	warmupFixture(t)
	a := useFakeAdder(t, false)

	t.Run("restores what the record kept", func(t *testing.T) {
		h := lifeHash(10)
		putRecord(h, []byte("info"), "kept title")
		tr, err := LoadStored(h.HexString())
		if err != nil {
			t.Fatal(err)
		}
		if tr.Title != "kept title" || tr.Category != "movie" || tr.Poster != "p" || tr.Data != "d" || tr.Size != 7 || tr.Timestamp != 42 {
			t.Errorf("restored %q %q %q %q %d %d", tr.Title, tr.Category, tr.Poster, tr.Data, tr.Size, tr.Timestamp)
		}
		if a.infoBytes.Load() != int32(len("info")) {
			t.Errorf("spec InfoBytes length = %d, want the record's", a.infoBytes.Load())
		}
		// A second wake returns the wrapper the engine holds and adds nothing.
		calls := a.calls.Load()
		again, err := LoadStored(h.HexString())
		if err != nil || again != tr || a.calls.Load() != calls {
			t.Errorf("second LoadStored = (%p, %v), calls %d, want the held wrapper %p and no add", again, err, a.calls.Load(), tr)
		}
	})
	t.Run("no record", func(t *testing.T) {
		calls := a.calls.Load()
		if _, err := LoadStored(lifeHash(11).HexString()); !errors.Is(err, ErrNotStored) || a.calls.Load() != calls {
			t.Errorf("err = %v, adds = %d", err, a.calls.Load()-calls)
		}
	})
	t.Run("a record without metainfo", func(t *testing.T) {
		h := lifeHash(12)
		putRecord(h, nil, "magnet only")
		calls := a.calls.Load()
		if _, err := LoadStored(h.HexString()); !errors.Is(err, ErrNotStored) || a.calls.Load() != calls {
			t.Errorf("err = %v, adds = %d", err, a.calls.Load()-calls)
		}
	})
	t.Run("malformed hash", func(t *testing.T) {
		if _, err := LoadStored("xyz"); !errors.Is(err, ErrNotStored) {
			t.Errorf("err = %v, want ErrNotStored", err)
		}
	})
}

// A panic in the engine's removal (gin's recovery swallows it) must leave neither
// lifeMu nor btsMu held: every later wake, save and removal would hang.
func TestPanickingRemovalReleasesTheLocks(t *testing.T) {
	warmupFixture(t)
	prev := engineRemove
	engineRemove = func(*BTServer, metainfo.Hash) bool { panic("boom") }
	t.Cleanup(func() { engineRemove = prev })

	func() {
		defer func() { _ = recover() }()
		RemTorrent(lifeHash(20).HexString())
	}()

	done := make(chan struct{})
	go func() {
		lifeMu.Lock()
		lifeMu.Unlock() //nolint:staticcheck // taking the lock is the check
		btsMu.Lock()
		btsMu.Unlock() //nolint:staticcheck // taking the lock is the check
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("lifeMu or btsMu is still held after a panicking removal")
	}
}

// A save skipped because the torrent is no longer held must not start the 30 s
// debounce: the same hash added again right after is saved.
func TestSkippedSaveDoesNotDebounceTheNextOne(t *testing.T) {
	warmupFixture(t)
	h := lifeHash(21)
	spec := &torrent.TorrentSpec{InfoHash: h, InfoBytes: []byte("info")}
	stale := register(bts, spec)
	RemTorrent(h.HexString())
	SaveTorrentToDB(stale) // not held any more: skipped
	if GetTorrentDB(h) != nil {
		t.Fatal("a removed torrent was saved")
	}

	fresh := register(bts, &torrent.TorrentSpec{InfoHash: h, InfoBytes: []byte("info")})
	SaveTorrentToDB(fresh) // within 30 s of the skipped one
	if GetTorrentDB(h) == nil {
		t.Fatal("the re-added torrent was not saved: a skipped save stamped the debounce")
	}
}

// A wake before the engine is up is ErrEngineNotReady, never ErrNotStored: it says
// nothing about the hash, and nothing is added.
func TestLoadStoredBeforeTheEngineIsUpIsNotReadyNotUnstored(t *testing.T) {
	warmupFixture(t)
	a := useFakeAdder(t, false)
	InitApiHelper(nil)
	_, err := LoadStored(lifeHash(1).HexString())
	if !errors.Is(err, ErrEngineNotReady) {
		t.Fatalf("LoadStored = %v, want ErrEngineNotReady", err)
	}
	if errors.Is(err, ErrNotStored) {
		t.Fatal("ErrEngineNotReady must not match ErrNotStored")
	}
	if a.calls.Load() != 0 {
		t.Fatalf("the adder was called %d times, want 0", a.calls.Load())
	}
}

func TestWaitSignalReturnsAtReadyAndAtTheBound(t *testing.T) {
	ready := make(chan struct{})
	start := time.Now()
	if waitSignal(ready, 50*time.Millisecond) {
		t.Fatal("waitSignal = true before ready")
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("waitSignal returned before the bound")
	}
	go func() { time.Sleep(20 * time.Millisecond); close(ready) }()
	start = time.Now()
	if !waitSignal(ready, 10*time.Second) {
		t.Fatal("waitSignal = false after ready")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("waitSignal waited for the bound although ready")
	}
	if !waitSignal(ready, 0) {
		t.Fatal("an already-ready signal must return true at once")
	}
}

func TestMarkEngineReadyIsOnceAndWaitSeesIt(t *testing.T) {
	markEngineReady()
	markEngineReady() // closing twice must not panic
	if !WaitEngineReady(0) {
		t.Fatal("WaitEngineReady = false after markEngineReady")
	}
}
