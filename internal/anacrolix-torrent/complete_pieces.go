package torrent

// CompletePieces reports, for pieces [first, first+n), whether each is verified complete.
// Pieces outside the torrent read as false. It takes the client read lock once, the lock
// PieceState takes, and does only bitmap lookups under it, so a caller can scan a long run
// without holding the lock for more than a few microseconds.
func (t *Torrent) CompletePieces(first, n int) []bool {
	if n <= 0 {
		return nil
	}
	t.cl.rLock()
	defer t.cl.rUnlock()
	out := make([]bool, n)
	if !t.haveInfo() {
		return out
	}
	total := t.numPieces()
	for k := range out {
		if i := first + k; i >= 0 && i < total {
			out[k] = t.pieceComplete(i)
		}
	}
	return out
}
