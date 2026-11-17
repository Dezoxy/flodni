package torrent

import (
	"testing"
	"time"

	"github.com/anacrolix/missinggo/v2/bitmap"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// newForgetTorrent is a torrent of 4 pieces of 2 chunks each, no peers and no storage traffic.
func newForgetTorrent(t *testing.T) *Torrent {
	t.Helper()
	cfg := NewDefaultClientConfig()
	cfg.DataDir = t.TempDir()
	cfg.ListenHost = func(string) string { return "127.0.0.1" }
	cfg.ListenPort = 0
	cfg.NoDefaultPortForwarding = true
	cfg.DisableTrackers = true
	cfg.DisableIPv6 = true
	cfg.NoDHT = true
	cl, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	const pieces = 4
	b, err := bencode.Marshal(metainfo.Info{Name: "forget", Length: 32768 * pieces, PieceLength: 32768, Pieces: make([]byte, 20*pieces)})
	if err != nil {
		t.Fatal(err)
	}
	tor, _, err := cl.AddTorrentSpec(&TorrentSpec{InfoHash: metainfo.HashBytes(b), InfoBytes: b})
	if err != nil {
		t.Fatal(err)
	}
	// A new torrent verifies its pieces once; wait for that to finish, or a piece reads as hashing.
	deadline := time.Now().Add(10 * time.Second)
	for {
		tor.cl.lock()
		busy := false
		for i := 0; i < pieces; i++ {
			p := tor.piece(i)
			busy = busy || p.hashing || p.marking || p.queuedForHash()
		}
		tor.cl.unlock()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the initial piece check did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return tor
}

// dirty marks both chunks of piece i as written, the state a piece is in once a seek abandons it.
func dirty(tor *Torrent, i int) {
	tor.cl.lock()
	defer tor.cl.unlock()
	p := tor.piece(i)
	p.unpendChunkIndex(0)
	p.unpendChunkIndex(1)
}

func dirtyCount(tor *Torrent, i int) int {
	tor.cl.lock()
	defer tor.cl.unlock()
	return int(tor.piece(i).numDirtyChunks())
}

// Each guard is tested alone: the piece is abandoned in every respect but one, and that one must
// keep the chunks. Removing any single guard from ForgetPartialPiece fails exactly its case.
func TestForgetPartialPieceRefuses(t *testing.T) {
	cases := []struct {
		name  string
		setup func(tor *Torrent, p *Piece)
		undo  func(tor *Torrent, p *Piece)
	}{
		{"complete", func(tor *Torrent, p *Piece) { tor._completedPieces.Add(uint32(p.index)) }, nil},
		{"wanted", func(tor *Torrent, p *Piece) {
			p.priority = PiecePriorityNormal
			tor.updatePiecePriority(p.index, "test")
		}, func(tor *Torrent, p *Piece) { p.priority = PiecePriorityNone }},
		{"hashing", func(tor *Torrent, p *Piece) { p.hashing = true }, func(tor *Torrent, p *Piece) { p.hashing = false }},
		{"marking", func(tor *Torrent, p *Piece) { p.marking = true }, func(tor *Torrent, p *Piece) { p.marking = false }},
		{"queued for hash", func(tor *Torrent, p *Piece) { tor.piecesQueuedForHash.Add(bitmap.BitIndex(p.index)) },
			func(tor *Torrent, p *Piece) { tor.piecesQueuedForHash.Remove(bitmap.BitIndex(p.index)) }},
		{"pending write", func(tor *Torrent, p *Piece) { p.incrementPendingWrites() },
			func(tor *Torrent, p *Piece) { p.decrementPendingWrites() }},
		{"outstanding request", func(tor *Torrent, p *Piece) {
			tor.requestState[tor.pieceRequestIndexOffset(p.index)+1] = requestState{}
		}, func(tor *Torrent, p *Piece) { delete(tor.requestState, tor.pieceRequestIndexOffset(p.index)+1) }},
		{"awaiting cancel", func(tor *Torrent, p *Piece) {
			pc := &PeerConn{}
			pc.requestState.Cancelled.Add(tor.pieceRequestIndexOffset(p.index))
			tor.conns[pc] = struct{}{}
		}, func(tor *Torrent, p *Piece) {
			for pc := range tor.conns {
				delete(tor.conns, pc)
			}
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tor := newForgetTorrent(t)
			idx := i % 4
			dirty(tor, idx)
			p := tor.piece(idx)
			tor.cl.lock()
			tc.setup(tor, p)
			tor.cl.unlock()
			// Whatever the setup left behind must be gone before the client closes.
			t.Cleanup(func() {
				tor.cl.lock()
				defer tor.cl.unlock()
				if tc.undo != nil {
					tc.undo(tor, p)
				}
			})
			if tor.ForgetPartialPiece(idx) {
				t.Fatalf("forgot a piece that is %s", tc.name)
			}
			if got := dirtyCount(tor, idx); got != 2 {
				t.Errorf("dirty chunks after a refusal = %d, want 2", got)
			}
		})
	}
}

// A piece nothing holds and nothing wants is forgotten: its chunks are pended again, so a later
// request fetches them. A piece with no dirty chunk, an unknown index and a closed torrent are
// refused.
func TestForgetPartialPieceForgets(t *testing.T) {
	tor := newForgetTorrent(t)
	if tor.ForgetPartialPiece(1) {
		t.Error("forgot a piece with no dirty chunk")
	}
	dirty(tor, 1)
	dirty(tor, 2)
	if !tor.ForgetPartialPiece(1) {
		t.Fatal("refused a piece nothing holds")
	}
	if got := dirtyCount(tor, 1); got != 0 {
		t.Errorf("dirty chunks after the forget = %d, want 0", got)
	}
	if got := dirtyCount(tor, 2); got != 2 {
		t.Errorf("a neighbour lost its chunks: dirty = %d, want 2", got)
	}
	for _, bad := range []int{-1, 4, 99} {
		if tor.ForgetPartialPiece(bad) {
			t.Errorf("forgot out-of-range piece %d", bad)
		}
	}
	tor.Drop()
	if tor.ForgetPartialPiece(2) {
		t.Error("forgot a piece of a closed torrent")
	}
}
