package torr

import (
	"testing"

	"github.com/anacrolix/torrent"
)

// A wake saves the torrent only while its metadata is missing: a save on every
// wake snapshots peers under the client lock during playback, and the expiry save
// keeps the peer list current.
func TestSaveMetadataIfMissingSavesOnlyWithoutMetadata(t *testing.T) {
	warmupFixture(t)

	have := register(bts, &torrent.TorrentSpec{InfoHash: lifeHash(40), InfoBytes: []byte("info")})
	SaveMetadataIfMissing(have)
	if GetTorrentDB(lifeHash(40)) != nil {
		t.Error("a wake with metadata present saved the torrent")
	}

	missing := register(bts, &torrent.TorrentSpec{InfoHash: lifeHash(41)})
	SaveMetadataIfMissing(missing)
	if GetTorrentDB(lifeHash(41)) == nil {
		t.Error("a wake without metadata did not save the torrent")
	}

	SaveMetadataIfMissing(nil) // must not panic
}
