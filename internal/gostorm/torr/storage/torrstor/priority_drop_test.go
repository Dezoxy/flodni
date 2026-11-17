package torrstor

import (
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	torrenttypes "github.com/anacrolix/torrent/types"
)

// newPriorityTestCache starts a client with a capped cache and an 8-piece torrent.
func newPriorityTestCache(t *testing.T) (*torrent.Torrent, *Cache) {
	t.Helper()
	return newPriorityTestCacheN(t, 8)
}

// newPriorityTestCacheN is the same with a torrent of this many 16 KiB pieces.
func newPriorityTestCacheN(t *testing.T, pieces int) (*torrent.Torrent, *Cache) {
	t.Helper()
	return newPriorityTestCacheSized(t, pieces, 16<<10)
}

// newPriorityTestCacheSized is the same with a piece length of pieceLen bytes. The pieces carry
// no data, so a large piece length costs nothing.
func newPriorityTestCacheSized(t *testing.T, pieces int, pieceLen int64) (*torrent.Torrent, *Cache) {
	t.Helper()
	stor := NewStorage(int64(pieces) * pieceLen)

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

	info := metainfo.Info{PieceLength: pieceLen, Length: int64(pieces) * pieceLen, Name: "x", Pieces: make([]byte, 20*pieces)}
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

// A piece that completed must lose the priority this cache gave it: with a storage cap the
// engine spends the cap on pieces in priority order, complete or not, so a stale priority on a
// complete piece keeps the pieces a reader waits for from being requested.
func TestMarkCompleteDropsCachePriority(t *testing.T) {
	tor, cache := newPriorityTestCache(t)

	// What setLoadPriority does for a piece under a reader.
	const id = 3
	cache.muPriority.Lock()
	cache.localPriority[id] = torrent.PiecePriorityNow
	tor.SetPiecePriorities(map[int]torrenttypes.PiecePriority{id: torrent.PiecePriorityNow})
	cache.muPriority.Unlock()
	if got := tor.PieceState(id).Priority; got != torrent.PiecePriorityNow {
		t.Fatalf("setup: priority %v, want Now", got)
	}

	if err := cache.pieces[id].MarkComplete(); err != nil {
		t.Fatal(err)
	}

	if got := tor.PieceState(id).Priority; got != torrent.PiecePriorityNone {
		t.Errorf("engine priority after MarkComplete = %v, want None", got)
	}
	cache.muPriority.Lock()
	_, kept := cache.localPriority[id]
	cache.muPriority.Unlock()
	if kept {
		t.Error("localPriority entry kept after MarkComplete")
	}
}

// RequestRange raises a piece to Normal through the engine's DownloadPieces, outside this
// cache's localPriority. The completing piece must lose that priority too: the engine charges
// every scanned piece against its storage cap, complete or not.
func TestMarkCompleteDropsEnginePriorityOutsideLocal(t *testing.T) {
	tor, cache := newPriorityTestCache(t)

	const id = 3
	tor.DownloadPieces(id, id+1)
	if got := tor.PieceState(id).Priority; got != torrent.PiecePriorityNormal {
		t.Fatalf("setup: priority %v, want Normal", got)
	}
	cache.muPriority.Lock()
	_, local := cache.localPriority[id]
	cache.muPriority.Unlock()
	if local {
		t.Fatal("setup: piece is in localPriority")
	}

	if err := cache.pieces[id].MarkComplete(); err != nil {
		t.Fatal(err)
	}

	if got := tor.PieceState(id).Priority; got != torrent.PiecePriorityNone {
		t.Errorf("engine priority after MarkComplete = %v, want None", got)
	}
}

// A capped cache must evict as a piece completes, not up to a second later: until it does the
// engine counts every piece complete and tells the swarm so.
func TestMarkCompleteEvictsOverCapacity(t *testing.T) {
	const pieceLen = int64(1 << 20)
	const pieces = 6
	c := &Cache{
		capacity:      3 * pieceLen,
		pieceLength:   pieceLen,
		pieceCount:    pieces,
		pieces:        make(map[int]*Piece),
		readers:       make(map[*Reader]struct{}),
		cleanTrigger:  make(chan struct{}, 1),
		cleanStop:     make(chan struct{}),
		localPriority: make(map[int]torrenttypes.PiecePriority),
		pieceInRange:  make([]bool, pieces),
	}
	defer close(c.cleanStop)
	for i := 0; i < pieces; i++ {
		p := &Piece{Id: i, cache: c, mPiece: &MemPiece{}}
		p.mPiece.piece = p
		p.mPiece.buffer = make([]byte, pieceLen)
		p.Size = pieceLen
		// The completing piece is the oldest, so it is the first victim if it counts as complete.
		p.Accessed = int64(pieces - i)
		p.Complete.Store(i != pieces-1)
		c.pieces[i] = p
	}
	last := c.pieces[pieces-1]
	if err := last.MarkComplete(); err != nil {
		t.Fatal(err)
	}
	var stored int64
	complete := 0
	for _, p := range c.pieces {
		stored += p.Size
		if p.Complete.Load() {
			complete++
		}
	}
	if stored > c.capacity {
		t.Errorf("stored %d bytes after MarkComplete, capacity %d", stored, c.capacity)
	}
	if complete == pieces {
		t.Error("every piece still reports complete")
	}
	if !last.Complete.Load() || last.Size != pieceLen {
		t.Errorf("the completing piece was evicted: complete=%v size=%d, want true and %d",
			last.Complete.Load(), last.Size, pieceLen)
	}
}
