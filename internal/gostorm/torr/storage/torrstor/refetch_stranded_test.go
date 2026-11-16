package torrstor

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"flodni/internal/gostorm/settings"
)

// newMetricsTestCache starts a client with a 40-piece torrent of 1 MiB pieces and a cache capped
// at 6 MiB. The 8 MiB head and tail of the file are never evicted, so the pieces the script
// evicts sit in the middle.
func newMetricsTestCache(t *testing.T) (*torrent.Torrent, *Cache) {
	t.Helper()
	const pieceLen = 1 << 20
	const pieces = 40
	oldSets := settings.BTsets
	settings.BTsets = &settings.BTSets{CacheSize: 6 * pieceLen, ReaderReadAHead: 50, ConnectionsLimit: 8}
	t.Cleanup(func() { settings.BTsets = oldSets })

	stor := NewStorage(6 * pieceLen)
	cfg := torrent.NewDefaultClientConfig()
	cfg.DefaultStorage = stor
	cfg.ListenPort = 0
	cfg.NoDHT = true
	cfg.DisableTrackers = true
	cfg.NoDefaultPortForwarding = true
	cfg.DisableUTP = true
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })

	info := metainfo.Info{PieceLength: pieceLen, Length: pieces * pieceLen, Name: "x", Pieces: make([]byte, 20*pieces)}
	ib, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	tor, err := cl.AddTorrent(&metainfo.MetaInfo{InfoBytes: ib})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-tor.GotInfo():
	case <-time.After(5 * time.Second):
		t.Fatal("no info")
	}
	cache := stor.GetCache(tor.InfoHash())
	if cache == nil {
		t.Fatal("no cache")
	}
	cache.SetTorrent(tor)
	return tor, cache
}

// fetch writes n bytes at the start of a piece the way the engine does, and stamps an access time
// so the eviction order is the script's, not the clock's.
func fetch(t *testing.T, c *Cache, id int, n int, accessed int64) {
	t.Helper()
	if _, err := c.pieces[id].WriteAt(make([]byte, n), 0); err != nil {
		t.Fatal(err)
	}
	atomic.StoreInt64(&c.pieces[id].Accessed, accessed)
}

// A scripted read, seek and eviction sequence: the counter counts only bytes fetched again for a
// piece that had completed before and was evicted, and the gauge counts partial pieces outside
// every reader's window.
func TestRefetchedBytesAndStrandedPartial(t *testing.T) {
	const mib = int64(1 << 20)
	tor, c := newMetricsTestCache(t)
	r := c.NewReader(tor.Files()[0])
	defer c.CloseReader(r)
	if _, err := r.Seek(20*mib, 0); err != nil { // window: pieces 17..22
		t.Fatal(err)
	}

	refetched0 := RefetchedBytes()
	delta := func() int64 { return RefetchedBytes() - refetched0 }

	// Three complete pieces behind the reader, a half-written one behind it and one inside the window.
	for i, id := range []int{10, 11, 12} {
		fetch(t, c, id, int(mib), int64(i+1))
		if err := c.pieces[id].MarkComplete(); err != nil {
			t.Fatal(err)
		}
	}
	fetch(t, c, 13, int(mib/2), 50)
	fetch(t, c, 21, int(mib/2), 50)
	if got := c.strandedPartialBytes(); got != mib/2 {
		t.Errorf("stranded before eviction = %d, want %d (piece 13 only; 21 is in the window)", got, mib/2)
	}
	if got := delta(); got != 0 {
		t.Errorf("refetched after first fetches = %d, want 0", got)
	}

	// Two more complete pieces push the cache over 6 MiB; the two oldest complete pieces go.
	for _, id := range []int{25, 26, 27} {
		fetch(t, c, id, int(mib), 100+int64(id))
		if err := c.pieces[id].MarkComplete(); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []int{10, 11} {
		if atomic.LoadInt64(&c.pieces[id].Size) != 0 || c.pieces[id].Complete.Load() {
			t.Fatalf("setup: piece %d not evicted (size %d)", id, atomic.LoadInt64(&c.pieces[id].Size))
		}
	}
	if got := delta(); got != 0 {
		t.Errorf("refetched after eviction = %d, want 0 (eviction is not a fetch)", got)
	}

	// Fetch piece 10 again in full and piece 11 by half: 1.5 MiB came twice. Finishing piece 13,
	// which never completed, is not a refetch.
	fetch(t, c, 10, int(mib), 200)
	fetch(t, c, 11, int(mib/2), 200)
	if _, err := c.pieces[13].WriteAt(make([]byte, mib/2), mib/2); err != nil {
		t.Fatal(err)
	}
	if got, want := delta(), mib+mib/2; got != want {
		t.Errorf("refetched = %d, want %d", got, want)
	}

	// Piece 10 is now whole but not complete, 11 and 13 are partial, 21 is partial in the window.
	// All of 10, 11 and 13 lie outside it: 1 + 0.5 + 1 MiB.
	if got, want := c.strandedPartialBytes(), mib+mib/2+mib; got != want {
		t.Errorf("stranded = %d, want %d", got, want)
	}

	// Seek to 12 MiB: window 9..14 takes in 10, 11 and 13 and leaves 21 behind.
	if _, err := r.Seek(12*mib, 0); err != nil {
		t.Fatal(err)
	}
	if got, want := c.strandedPartialBytes(), mib/2; got != want {
		t.Errorf("stranded after seek = %d, want %d (piece 21)", got, want)
	}
}

// A cache that closes is no longer sampled.
func TestStrandedPartialBytesSkipsClosedCache(t *testing.T) {
	_, c := newMetricsTestCache(t)
	fetch(t, c, 12, 1<<20, 1)
	if got := StrandedPartialBytes(); got != 1<<20 {
		t.Fatalf("stranded = %d, want %d", got, 1<<20)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if got := StrandedPartialBytes(); got != 0 {
		t.Errorf("stranded after Close = %d, want 0", got)
	}
}
