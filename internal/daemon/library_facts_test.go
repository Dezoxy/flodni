package daemon

import (
	"testing"
	"time"

	"flodni/internal/warmup"
)

// libraryFileFacts feeds GET /api/library/file: the warm record, the marks and the open
// session, each read from where the daemon keeps it.
func TestFileFactsReadTheIndexAndTheOpenSession(t *testing.T) {
	old := fileStates
	x := warmIndex(t)
	fileStates = x
	t.Cleanup(func() { fileStates = old })

	k := warmup.FileKey{Hash: "aabbcc", FileIdx: 4}
	if f := libraryFileFacts("AABBCC", 4); f.Readability != "" || f.Cache != "" || f.Queued || !f.PlayedAt.IsZero() || f.Evicted || f.Confirmed {
		t.Fatalf("unknown file reports facts: %+v", f)
	}

	x.RecordWarm(warmUpdate{key: k, queuedAt: wt0})
	if f := libraryFileFacts("aabbcc", 4); !f.Queued {
		t.Fatalf("queued file: %+v", f)
	}
	x.RecordWarm(warmUpdate{key: k, readability: readWarmed, cache: cacheCached, deliveredAt: wt0, clearQueued: true})
	f := libraryFileFacts("AabbCC", 4)
	if f.Readability != readWarmed || f.Cache != cacheCached || f.Queued || f.Evicted || f.Protected {
		t.Fatalf("warm file: %+v", f)
	}

	x.MarkPlayed(k)
	x.OnEvicted([]warmup.Eviction{{Key: k}})
	f = libraryFileFacts("aabbcc", 4)
	if !f.Evicted || f.Cache != cacheEvicted || !f.Protected || !f.PlayedAt.Equal(wt0) || f.Readability != readWarmed {
		t.Fatalf("evicted and played file: %+v", f)
	}

	// The protection ends after its window; the play stays recorded.
	x.now = func() time.Time { return wt0.Add(91 * 24 * time.Hour) }
	if f = libraryFileFacts("aabbcc", 4); f.Protected || f.PlayedAt.IsZero() {
		t.Fatalf("after the window: %+v", f)
	}
	x.now = func() time.Time { return wt0 }

	// Another index of the same torrent is another file.
	if f = libraryFileFacts("aabbcc", 5); f.Evicted || !f.PlayedAt.IsZero() {
		t.Fatalf("index 5: %+v", f)
	}
}

func TestFileFactsConfirmedNeedsAnOpenConfirmedSession(t *testing.T) {
	newTestApp(t)
	const path = "/mnt/tv/Show/Show.S01E01.mkv"
	k := warmup.FileKey{Hash: "ddeeff", FileIdx: 2}
	ps := &PlaybackState{Path: path, OpenedAt: time.Now()}
	app.playbackRegistry.Store(path, ps)
	tailFillTargets.Store(path, tailFillTarget{hash: "DDEEFF", fileID: 2, size: 1})
	h := &MkvHandle{path: path}
	t.Cleanup(func() {
		app.playbackRegistry.Delete(path)
		tailFillTargets.Delete(path)
		app.activeHandles.Delete(h)
	})

	if libraryFileFacts(k.Hash, k.FileIdx).Confirmed {
		t.Fatal("confirmed before any webhook")
	}
	ps.mu.Lock()
	ps.IsHealthy = true
	ps.mu.Unlock()
	if libraryFileFacts(k.Hash, k.FileIdx).Confirmed {
		t.Fatal("confirmed with no open handle (a state restored at start)")
	}
	app.activeHandles.Store(h, true)
	if !libraryFileFacts(k.Hash, k.FileIdx).Confirmed {
		t.Fatal("an open, confirmed session is not reported")
	}
	if libraryFileFacts(k.Hash, 3).Confirmed {
		t.Fatal("another file of the torrent reported confirmed")
	}
	ps.mu.Lock()
	ps.IsStopped = true
	ps.mu.Unlock()
	if libraryFileFacts(k.Hash, k.FileIdx).Confirmed {
		t.Fatal("a stopped session is still confirmed")
	}
}
