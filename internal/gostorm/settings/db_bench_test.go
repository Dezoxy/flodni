package settings

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	bolt "go.etcd.io/bbolt"
)

// The save the wake path does (torr.saveIfHeld -> AddTorrent): one JSON record with the
// metainfo, a peer list and the file list, one bbolt commit. The debounce allows one per
// torrent per 30 s, so the cost here is what each save adds under lifeMu.
func benchSave(b *testing.B, noSync bool) {
	db, err := bolt.Open(filepath.Join(b.TempDir(), "config.db"), 0o600, &bolt.Options{Timeout: 5 * time.Second, NoSync: noSync})
	if err != nil {
		b.Fatal(err)
	}
	old := tdb
	tdb = &TDB{db: db}
	b.Cleanup(func() { db.Close(); tdb = old })

	info := make([]byte, 200<<10)
	peers := make([]string, 30)
	for i := range peers {
		peers[i] = fmt.Sprintf("192.0.2.%d:51413", i)
	}
	rec := &TorrentDB{
		TorrentSpec: &torrent.TorrentSpec{InfoBytes: info, PeerAddrs: peers},
		Title:       "bench",
		Data:        string(make([]byte, 8<<10)),
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec.InfoHash = metainfo.Hash{byte(i), byte(i >> 8), byte(i >> 16)}
		AddTorrent(rec)
	}
}

func BenchmarkSaveRecordSynced(b *testing.B) { benchSave(b, false) }
func BenchmarkSaveRecordNoSync(b *testing.B) { benchSave(b, true) }
