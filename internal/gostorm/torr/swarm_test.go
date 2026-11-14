package torr

import (
	"testing"

	"flodni/internal/gostorm/torr/state"
)

// A closed or never-hydrated torrent has no signal and the call does not start one.
func TestSwarmSignalClosedOrBareIsNotOK(t *testing.T) {
	var nilT *Torrent
	if _, ok := nilT.SwarmSignal(); ok {
		t.Fatal("nil torrent reported a signal")
	}
	bare := &Torrent{}
	if _, ok := bare.SwarmSignal(); ok {
		t.Fatal("torrent without an engine handle reported a signal")
	}
	closed := &Torrent{}
	closed.Stat = state.TorrentClosed
	if _, ok := closed.SwarmSignal(); ok {
		t.Fatal("closed torrent reported a signal")
	}
	if closed.Stat != state.TorrentClosed {
		t.Fatal("SwarmSignal changed the state")
	}
}
