package daemon

import (
	"log"
	"sync/atomic"
	"time"

	"flodni/internal/gostorm/torr"
	"flodni/internal/promtext"
)

// swarmSignalInterval is how often the swarm snapshot is rebuilt. The health handler and
// the scrape read the stored snapshot only, so neither walks the engine.
const swarmSignalInterval = 15 * time.Second

// swarmHealth is the EXPERIMENTAL "swarm" object of /api/torrents/health (README). It is
// null for a torrent that was not open at the last snapshot.
type swarmHealth struct {
	Awake       bool    `json:"awake"`
	Peers       int     `json:"peers"`
	Seeders     *int    `json:"seeders"`
	LastPieceAt *string `json:"last_piece_at"`
	RateBps     int64   `json:"rate_bps"`
}

// swarmObservation is one open torrent as the engine reported it.
type swarmObservation struct {
	Hash string
	torr.SwarmSignal
}

// swarmSource lists the open torrents' signals. It must not wake a closed torrent.
type swarmSource func() []swarmObservation

// swarmSnapshot is hash -> swarm object, immutable once stored. nil until the first
// refresh, which reads as "not known": the field serves null and the gauge is omitted.
var swarmSnapshot atomic.Pointer[map[string]swarmHealth]

// engineSwarmSource walks the in-memory torrents only (ListActiveTorrent never touches
// the database nor wakes one) and asks each for its signal.
func engineSwarmSource() []swarmObservation {
	var out []swarmObservation
	for _, t := range torr.ListActiveTorrent() {
		if s, ok := t.SwarmSignal(); ok {
			out = append(out, swarmObservation{Hash: t.Hash().HexString(), SwarmSignal: s})
		}
	}
	return out
}

func refreshSwarmSnapshot(src swarmSource) {
	obs := src()
	m := make(map[string]swarmHealth, len(obs))
	for _, o := range obs {
		h := swarmHealth{Awake: true, Peers: o.Peers, RateBps: int64(o.RateBps)}
		if o.SeedersKnown {
			n := o.Seeders
			h.Seeders = &n
		}
		if !o.LastPieceAt.IsZero() {
			s := o.LastPieceAt.UTC().Format(time.RFC3339)
			h.LastPieceAt = &s
		}
		m[o.Hash] = h
	}
	swarmSnapshot.Store(&m)
}

// swarmFor serves the swarm object of a hash: nil (JSON null) for a torrent that is not
// open, or before the first snapshot.
func swarmFor(hash string) *swarmHealth {
	m := swarmSnapshot.Load()
	if m == nil {
		return nil
	}
	if h, ok := (*m)[hash]; ok {
		return &h
	}
	return nil
}

// startSwarmSnapshot refreshes now and then every interval until stop closes.
func startSwarmSnapshot(src swarmSource, stop <-chan struct{}, logger *log.Logger) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("WARNING: swarm snapshot stopped: %v", r)
			}
		}()
		refreshSwarmSnapshot(src)
		t := time.NewTicker(swarmSignalInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				refreshSwarmSnapshot(src)
			case <-stop:
				return
			}
		}
	}()
}

// collectSwarmProm writes the awake-torrent count from the snapshot. No series before
// the first snapshot: a zero would read as "none awake".
func collectSwarmProm(w *promtext.Writer) {
	m := swarmSnapshot.Load()
	if m == nil {
		return
	}
	w.Gauge("flodni_swarm_awake_torrents",
		"Torrents the engine has open, as of the last snapshot (refreshed every 15 s). Experimental.",
		promSample(int64(len(*m))))
}
