package torrent

// ForgetPartialPiece makes the torrent forget the chunks it holds for an incomplete piece that
// nothing wants any more, so the storage may free the piece's buffer. A seek leaves such pieces
// behind: their requests are cancelled, the cache cannot evict them (it only evicts complete
// pieces), and each keeps a whole piece allocated until the torrent closes.
//
// What makes it safe: a chunk's dirty bit is set in the same client-lock hold that counts its
// write as pending (Peer.receiveChunk), and this runs under the client lock. So a piece with no
// pending write and no hashing, marking or queued hash has no chunk the torrent counts as stored
// that this call could orphan, and a chunk that lands afterwards is written and marked dirty
// again. The storage pairs that with a write counter, so it keeps the bytes of a chunk that
// landed between its own check and the release.
//
// It returns false while the piece is complete, wanted, being hashed or marked complete, queued
// for hashing or has a write in flight, or while a request for one of its chunks is outstanding
// or awaiting a cancel acknowledgement on a connection. Those request checks only avoid a
// wasted re-download: webseed requests, hedge duplicates and cancels to peers without the fast
// extension are not visible to them, and a chunk that arrives anyway is re-dirtied, so the cost
// is that chunk downloaded twice. A piece with no dirty chunk (a failed hash pends them all but
// keeps the buffer) is forgotten too: nothing needs pending, and the caller can free the buffer.
//
// Lock order: it takes the client lock and nothing else (no piece, storage or cache lock), so a
// caller must not hold a lock that a client-lock holder can wait for. The storage's cache holds
// none of its own while calling it. Adapted from upstream Tiramisu e7301dc.
func (t *Torrent) ForgetPartialPiece(index int) bool {
	t.cl.lock()
	defer t.cl.unlock()
	if t.closed.IsSet() || t.info == nil || index < 0 || index >= t.numPieces() {
		return false
	}
	p := t.piece(index)
	if t.pieceComplete(index) || t.wantPieceIndex(index) || p.hashing || p.marking ||
		p.queuedForHash() || p.atomicPendingWrites.Load() != 0 {
		return false
	}
	start := t.pieceRequestIndexOffset(index)
	end := t.pieceRequestIndexOffset(index + 1)
	for ri := start; ri < end; ri++ {
		if _, ok := t.requestState[ri]; ok {
			return false
		}
	}
	for pc := range t.conns {
		if roaringBitmapRangeCardinality[RequestIndex](&pc.requestState.Cancelled, start, end) != 0 {
			return false
		}
	}
	if p.hasDirtyChunks() {
		t.pendAllChunkSpecs(index)
	}
	t.clearPieceTouchers(index)
	t.updatePiecePriority(index, "Torrent.ForgetPartialPiece")
	return true
}
