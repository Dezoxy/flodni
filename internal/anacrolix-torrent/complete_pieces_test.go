package torrent

import "testing"

func TestCompletePieces(t *testing.T) {
	tor := newForgetTorrent(t) // 4 pieces
	tor.cl.lock()
	tor._completedPieces.Add(1)
	tor._completedPieces.Add(3)
	tor.cl.unlock()

	got := tor.CompletePieces(0, 4)
	if len(got) != 4 || got[0] || !got[1] || got[2] || !got[3] {
		t.Fatalf("pieces 0..3: %v", got)
	}
	// Out of range reads false and keeps the slice aligned with the request.
	got = tor.CompletePieces(2, 5)
	if len(got) != 5 || got[0] || !got[1] || got[2] || got[3] || got[4] {
		t.Fatalf("pieces 2..6: %v", got)
	}
	if got = tor.CompletePieces(-1, 2); len(got) != 2 || got[0] || got[1] {
		t.Fatalf("pieces -1..0: %v", got)
	}
	if tor.CompletePieces(0, 0) != nil {
		t.Fatal("n=0 must give nil")
	}
}
