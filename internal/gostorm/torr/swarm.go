package torr

import (
	"time"

	"flodni/internal/gostorm/torr/state"
)

// SwarmSignal is what the engine already knows about the swarm of one open torrent
// (Dezoxy fork). Peers and Seeders are the connected counts; Seeders is only meaningful
// once the metainfo is in (SeedersKnown). LastPieceAt is the zero time until a piece
// completed. RateBps is the download rate the engine's 1 s ticker last measured.
type SwarmSignal struct {
	Peers        int
	Seeders      int
	SeedersKnown bool
	LastPieceAt  time.Time
	RateBps      float64
}

// SwarmSignal reads the signal from the engine's existing stats without waking
// anything: ok is false for a torrent that is closed, not hydrated or has no engine
// handle. muTorrent is held only to copy two fields, then released before the engine
// is asked, so it never nests with the engine's own locks (the same order Status uses).
func (t *Torrent) SwarmSignal() (s SwarmSignal, ok bool) {
	if t == nil {
		return s, false
	}
	t.muTorrent.Lock()
	stat, torr, rate, cache := t.Stat, t.Torrent, t.DownloadSpeed, t.cache
	t.muTorrent.Unlock()
	if torr == nil || stat == state.TorrentClosed || stat == state.TorrentInDB {
		return s, false
	}
	st := torr.Stats()
	s.Peers = st.ActivePeers
	s.Seeders = st.ConnectedSeeders
	s.RateBps = rate
	if info := torr.Info(); info != nil {
		s.SeedersKnown = true
		if unix, n := cache.LastDelivery(0, info.NumPieces()-1); n > 0 {
			s.LastPieceAt = time.Unix(unix, 0)
		}
	}
	return s, true
}
