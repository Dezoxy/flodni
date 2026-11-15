package torr

import (
	"encoding/json"

	"flodni/internal/gostorm/settings"
	"flodni/internal/gostorm/torr/state"

	"github.com/anacrolix/torrent/metainfo"
)

type tsFiles struct {
	GoStorm struct {
		Files []*state.TorrentFileStat `json:"Files"`
	} `json:"GoStorm"`
}

// AddTorrentDB writes the record. posterOK says the poster URL passed CheckImgUrl; the
// caller checks it first, because that is a network call and must not run under lifeMu.
// It returns the commit error: a record that did not commit is not saved.
func AddTorrentDB(torr *Torrent, posterOK bool) error {
	t := new(settings.TorrentDB)
	t.TorrentSpec = torr.TorrentSpec
	t.Title = torr.Title
	t.Category = torr.Category
	if torr.Data == "" {
		files := new(tsFiles)
		files.GoStorm.Files = torr.Status().FileStats
		buf, err := json.Marshal(files)
		if err == nil {
			t.Data = string(buf)
			torr.Data = t.Data
		}
	} else {
		t.Data = torr.Data
	}
	if posterOK {
		t.Poster = torr.Poster
	}
	t.Size = torr.Size
	if t.Size == 0 && torr.Torrent != nil {
		t.Size = torr.Torrent.Length()
	}
	// don't override timestamp from DB on edit
	t.Timestamp = torr.Timestamp // time.Now().Unix()

	return settings.AddTorrent(t)
}

// StoredFiles is the engine's file list saved with a torrent's record (id, path, length),
// read without waking or loading the torrent (Dezoxy fork). The ids are the ones
// AddTorrentDB wrote from Status().FileStats: the engine's own, neither the metainfo's
// order nor a fresh sort. It takes only the settings mutex and the database read: no
// lifeMu, btsMu, bt.mu or muTorrent. Nil when there is no record, no list (saved before
// the metainfo) or an unreadable one.
func StoredFiles(hashHex string) []*state.TorrentFileStat {
	hash, ok := parseHash(hashHex)
	if !ok {
		return nil
	}
	db := settings.GetTorrent(hash)
	if db == nil || db.Data == "" {
		return nil
	}
	return parseStoredFiles(db.Data)
}

func parseStoredFiles(data string) []*state.TorrentFileStat {
	var f tsFiles
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		return nil
	}
	return f.GoStorm.Files
}

func GetTorrentDB(hash metainfo.Hash) *Torrent {
	// V271: O(1) direct lookup instead of loading all torrents
	db := settings.GetTorrent(hash)
	if db == nil {
		return nil
	}
	torr := new(Torrent)
	torr.TorrentSpec = db.TorrentSpec
	torr.Title = db.Title
	torr.Poster = db.Poster
	torr.Category = db.Category
	torr.Timestamp = db.Timestamp
	torr.Size = db.Size
	torr.Data = db.Data
	torr.Stat = state.TorrentInDB
	return torr
}

func RemTorrentDB(hash metainfo.Hash) error {
	return settings.RemTorrent(hash)
}

func ListTorrentsDB() map[metainfo.Hash]*Torrent {
	ret := make(map[metainfo.Hash]*Torrent)
	list := settings.ListTorrent()
	for _, db := range list {
		torr := new(Torrent)
		torr.TorrentSpec = db.TorrentSpec
		torr.Title = db.Title
		torr.Poster = db.Poster
		torr.Category = db.Category
		torr.Timestamp = db.Timestamp
		torr.Size = db.Size
		torr.Data = db.Data
		torr.Stat = state.TorrentInDB
		ret[torr.TorrentSpec.InfoHash] = torr
	}
	return ret
}
