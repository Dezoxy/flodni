package settings

import (
	"bytes"
	"encoding/json"
	"sort"
	"sync"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

type TorrentDB struct {
	*torrent.TorrentSpec

	Title    string `json:"title,omitempty"`
	Category string `json:"category,omitempty"`
	Poster   string `json:"poster,omitempty"`
	Data     string `json:"data,omitempty"`

	Timestamp int64 `json:"timestamp,omitempty"`
	Size      int64 `json:"size,omitempty"`
}

var mu sync.Mutex

// AddTorrent writes the record and returns the commit error: a record that did not
// commit is not saved, and the caller must not treat it as saved.
func AddTorrent(torr *TorrentDB) error {
	// V270: Only write the single torrent, not the entire list.
	// Previously this read ALL torrents, then re-wrote ALL of them (O(N) BoltDB transactions).
	// With 520 torrents, that was 520 fsync calls per add (~5s on SD card).
	mu.Lock()
	defer mu.Unlock()
	buf, err := json.Marshal(torr)
	if err != nil {
		return err
	}
	key := torr.InfoHash.HexString()
	// A wake saves the record it has just read: an identical one is not committed
	// again, which would cost two fdatasyncs under the wake's locks.
	if bytes.Equal(tdb.Get("Torrents", key), buf) {
		return nil
	}
	return trySetOn(tdb, "Torrents", key, buf)
}

func ListTorrent() []*TorrentDB {
	// Use read lock to prevent migration during read
	dbMigrationLock.RLock()
	defer dbMigrationLock.RUnlock()

	mu.Lock()
	defer mu.Unlock()

	var list []*TorrentDB
	keys := tdb.List("Torrents")
	for _, key := range keys {
		buf := tdb.Get("Torrents", key)
		if len(buf) > 0 {
			var torr *TorrentDB
			err := json.Unmarshal(buf, &torr)
			if err == nil {
				list = append(list, torr)
			}
		}
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Timestamp > list[j].Timestamp
	})
	return list
}

// V271: O(1) single torrent lookup by hash — reads one key from BoltDB.
// Previously GetTorrentDB called ListTorrent() which loaded and unmarshalled ALL torrents.
func GetTorrent(hash metainfo.Hash) *TorrentDB {
	mu.Lock()
	defer mu.Unlock()

	buf := tdb.Get("Torrents", hash.HexString())
	if len(buf) == 0 {
		return nil
	}
	var torr *TorrentDB
	if err := json.Unmarshal(buf, &torr); err != nil {
		return nil
	}
	return torr
}

// RemTorrent removes the record and returns the commit error: a record that is still
// there comes back on the next start.
func RemTorrent(hash metainfo.Hash) error {
	mu.Lock()
	defer mu.Unlock()
	return tryRemOn(tdb, "Torrents", hash.HexString())
}

// TorrentsReady says whether the torrent store is open; it opens in the engine's
// start-up, after the daemon has begun.
func TorrentsReady() bool {
	mu.Lock()
	defer mu.Unlock()
	return tdb != nil
}
