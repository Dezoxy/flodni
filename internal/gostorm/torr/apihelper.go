package torr

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"flodni/internal/gostorm/log"
	sets "flodni/internal/gostorm/settings"
	"flodni/internal/gostorm/torr/utils"
	"flodni/internal/redact"
	"flodni/internal/warmup"
)

// saveDebounce prevents redundant BoltDB writes for the same torrent.
// Critical on USB-attached SSDs (fsync ~10-30ms) and SD cards (50-200ms).
const saveDebounceInterval = 30 * time.Second

var lastSaveTimes sync.Map // hash string → time.Time

var (
	bts        *BTServer
	btsMu      sync.RWMutex
	settingsMu sync.Mutex // V227: Serialize SetSettings/SetDefSettings to prevent concurrent disconnect/connect
)

func InitApiHelper(bt *BTServer) {
	btsMu.Lock()
	defer btsMu.Unlock()
	bts = bt
}

// mergeTrackerTiers appends to base every tracker of extra it lacks, as one more tier.
func mergeTrackerTiers(base, extra [][]string) [][]string {
	seen := map[string]bool{}
	for _, tier := range base {
		for _, tr := range tier {
			seen[tr] = true
		}
	}
	var missing []string
	for _, tier := range extra {
		for _, tr := range tier {
			if tr != "" && !seen[tr] {
				seen[tr] = true
				missing = append(missing, tr)
			}
		}
	}
	if len(missing) == 0 {
		return base
	}
	return append(base, missing)
}

// countNewTrackers counts the trackers of req that known lacks. Only a count is ever
// logged: a tracker URL can carry a passkey.
func countNewTrackers(req, known [][]string) int {
	have := map[string]bool{}
	for _, tier := range known {
		for _, tr := range tier {
			have[tr] = true
		}
	}
	n := 0
	for _, tier := range req {
		for _, tr := range tier {
			if !have[tr] {
				n++
			}
		}
	}
	return n
}

// AddTorrent is the explicit add: the caller brings a spec (a magnet, a file, an
// upload) and the torrent starts from it. A wake of a closed torrent goes through
// LoadStored instead, which never adds from a caller's link.
func AddTorrent(spec *torrent.TorrentSpec, title, poster string, data string, category string) (*Torrent, error) {
	torr, err := addUnderLock(spec)
	if err != nil {
		log.TLogln("error add torrent:", redact.Err(err))
		return nil, err
	}

	torDB := GetTorrentDB(spec.InfoHash)

	if torr.Title == "" {
		torr.Title = title
		if title == "" && torDB != nil {
			torr.Title = torDB.Title
		}
		if torr.Title == "" && torr.Torrent != nil && torr.Torrent.Info() != nil {
			torr.Title = torr.Info().Name
		}
	}

	if torr.Category == "" {
		torr.Category = category
		if torr.Category == "" && torDB != nil {
			torr.Category = torDB.Category
		}
	}

	if torr.Poster == "" {
		torr.Poster = poster
		if torr.Poster == "" && torDB != nil {
			torr.Poster = torDB.Poster
		}
	}

	if torr.Data == "" {
		torr.Data = data
		if torr.Data == "" && torDB != nil {
			torr.Data = torDB.Data
		}
	}

	return torr, nil
}

// ErrNoMetadata and ErrNotSaved are why an add that asked for a record has none.
var (
	ErrNoMetadata = errors.New("no metadata before the timeout")
	ErrNotSaved   = errors.New("the record was not saved")
)

// SetAddedTitle gives an added torrent without a title the cleaned display name of its
// link, or the name its metadata carries. Call it once the metadata is in.
func SetAddedTitle(tor *Torrent, displayName string) {
	if tor.Title != "" {
		return
	}
	tor.Title = displayName // prefer dn over name
	tor.Title = strings.ReplaceAll(tor.Title, "rutor.info", "")
	tor.Title = strings.ReplaceAll(tor.Title, "_", " ")
	tor.Title = strings.Trim(tor.Title, " ")
	if tor.Title == "" && tor.Torrent != nil {
		tor.Title = tor.Name()
	}
}

// SaveAdded is the add that must not answer before its record exists: it waits for the
// metadata, titles the torrent and commits the record, and says which of the two failed.
// It does not use the save debounce: a save made a moment ago does not stand for this one.
func SaveAdded(tor *Torrent, displayName string) error {
	if !tor.GotInfo() {
		return ErrNoMetadata
	}
	SetAddedTitle(tor, displayName)
	if !saveTorrentToDB(tor) {
		return ErrNotSaved
	}
	lastSaveTimes.Store(tor.Hash(), time.Now())
	return nil
}

// ForceSaveTorrentToDB bypasses debounce — use only when the torrent is about
// to be removed (expiry) so the final peer snapshot is always persisted.
func ForceSaveTorrentToDB(torr *Torrent) {
	_ = saveTorrentToDB(torr)
}

// SaveMetadataIfMissing saves the torrent only while its InfoBytes are not stored yet, which
// is what lets the next Wake skip the metadata wait. Once they are, a save on every wake only
// snapshots peers under the client lock during playback; ForceSaveTorrentToDB at expiry keeps
// the peer list current. Adapted from upstream Tiramisu 4a25ffd.
func SaveMetadataIfMissing(torr *Torrent) {
	if torr == nil || torr.TorrentSpec == nil || len(torr.TorrentSpec.InfoBytes) > 0 {
		return
	}
	SaveTorrentToDB(torr)
}

// SaveTorrentToDB saves with a 30s debounce per hash to avoid redundant fsyncs.
func SaveTorrentToDB(torr *Torrent) {
	hash := torr.Hash()
	now := time.Now()
	if last, ok := lastSaveTimes.Load(hash); ok {
		if now.Sub(last.(time.Time)) < saveDebounceInterval {
			return // skip — saved recently
		}
	}
	// Stamped only by a save that wrote: one skipped because the torrent is no longer
	// held must not debounce the next save of the same hash after a re-add.
	if saveTorrentToDB(torr) {
		lastSaveTimes.Store(hash, now)
	}
}

// saveTorrentToDB reports whether it wrote the record; false when it was skipped or the
// commit failed.
func saveTorrentToDB(torr *Torrent) bool {
	// V255: Persist metadata InfoBytes to DB for instant re-activation.
	// Without this, TorrentSpec.InfoBytes stays nil (from original magnet parse),
	// forcing full metadata re-fetch from peers on every Wake() — adding 2-10s to TTFF.
	if torr.Torrent != nil && torr.Torrent.Info() != nil && len(torr.TorrentSpec.InfoBytes) == 0 {
		torr.TorrentSpec.InfoBytes = torr.Torrent.Metainfo().InfoBytes
	}
	// V255: Cache active peer addresses for instant reconnection on re-activation.
	// Same pattern as InfoBytes: PeerAddrs field exists in TorrentSpec but was never populated.
	// On next Wake(), AddTorrentSpec injects these as PeerSourceDirect/Trusted peers,
	// skipping the tracker/DHT discovery delay entirely.
	if torr.Torrent != nil {
		conns := torr.Torrent.PeerConns()
		if len(conns) > 0 {
			addrs := make([]string, 0, len(conns))
			for _, c := range conns {
				if addr := c.RemoteAddr.String(); addr != "" {
					addrs = append(addrs, addr)
				}
			}
			if len(addrs) > 0 {
				torr.TorrentSpec.PeerAddrs = addrs
			}
		}
	}
	// A network call: never under lifeMu. The poster is read under it, because a wake
	// restores the wrapper's fields under it after the wrapper is already visible.
	lifeMu.Lock()
	poster := torr.Poster
	lifeMu.Unlock()
	posterOK := utils.CheckImgUrl(poster)
	// The save and a removal are one step against each other: only a wrapper the
	// engine still holds may write a record, or an idle close that lost the race to
	// RemTorrent would bring the removed torrent back.
	return saveIfHeld(torr, posterOK)
}

func saveIfHeld(torr *Torrent, posterOK bool) bool {
	lifeMu.Lock()
	defer lifeMu.Unlock()
	btsMu.RLock()
	localBts := bts
	btsMu.RUnlock()
	hash := torr.Hash()
	if localBts == nil || localBts.GetTorrent(hash) != torr {
		log.TLogln("save to db skipped, torrent is not held:", hash)
		return false
	}
	log.TLogln("save to db:", hash)
	if err := AddTorrentDB(torr, posterOK); err != nil {
		// Not saved: the caller must not stamp the debounce, so the next save retries.
		log.TLogln("save to db failed, will retry:", hash)
		return false
	}
	return true
}

// parseHash validates before converting: metainfo.NewHashFromHex panics on
// anything that is not 40 hex chars, and these values come straight from API
// callers, so a truncated hash would take down the handler.
func parseHash(hashHex string) (metainfo.Hash, bool) {
	var h metainfo.Hash
	if len(hashHex) != 2*len(h) {
		return h, false
	}
	raw, err := hex.DecodeString(hashHex)
	if err != nil {
		return h, false
	}
	copy(h[:], raw)
	return h, true
}

// PeekTorrent gets a torrent from RAM or DB WITHOUT resetting its expiration timer (V301)
func PeekTorrent(hashHex string) *Torrent {
	btsMu.RLock()
	defer btsMu.RUnlock()

	if bts == nil {
		return nil
	}

	hash, ok := parseHash(hashHex)
	if !ok {
		return nil
	}
	tor := bts.GetTorrent(hash)
	if tor != nil {
		// Found in RAM, return as is (no AddExpiredTime)
		return tor
	}

	tr := GetTorrentDB(hash)
	return tr // Found in DB or nil
}

func GetTorrent(hashHex string) *Torrent {
	btsMu.RLock()
	defer btsMu.RUnlock()

	hash, ok := parseHash(hashHex)
	if !ok {
		return nil
	}
	timeout := time.Second * time.Duration(sets.BTsets.TorrentDisconnectTimeout)
	if timeout > time.Minute {
		timeout = time.Minute
	}
	tor := bts.GetTorrent(hash)
	if tor != nil {
		tor.AddExpiredTime(timeout)
		return tor
	}

	tr := GetTorrentDB(hash)
	if tr != nil {
		tor = tr
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.TLogln("[PANIC] NewTorrent goroutine recovered:", r)
				}
			}()
			log.TLogln("New torrent", tor.Hash())

			// The record may be gone by now (a removal, a refused hash): then nothing
			// is added, and the DB wrapper handed to the caller is all there is.
			t, err := LoadStored(tor.Hash().HexString())
			if err != nil {
				log.TLogln("New torrent not loaded:", redact.Err(err))
				return
			}
			t.GotInfo()
		}()
	}
	return tor
}

func SetTorrent(hashHex, title, poster, category string, data string) *Torrent {
	btsMu.RLock()
	localBts := bts
	btsMu.RUnlock()

	hash, ok := parseHash(hashHex)
	if !ok {
		log.TLogln("API SetTorrent: malformed hash:", hashHex)
		return nil
	}
	torr := localBts.GetTorrent(hash)
	torrDb := GetTorrentDB(hash)

	if title == "" && torr == nil && torrDb != nil {
		torr = GetTorrent(hashHex)
		if torr != nil {
			torr.GotInfo()
			if torr.Torrent != nil && torr.Torrent.Info() != nil {
				title = torr.Info().Name
			}
		}
	}

	if torr != nil {
		if title == "" && torr.Torrent != nil && torr.Torrent.Info() != nil {
			title = torr.Info().Name
		}
		torr.Title = title
		torr.Poster = poster
		torr.Category = category
		if data != "" {
			torr.Data = data
		}
	}
	// update torrent data in DB: only a record that still exists, read again under
	// the lock, so a removal that ran meanwhile is not undone.
	posterOK := utils.CheckImgUrl(poster) // a network call: never under lifeMu
	torrDb = updateRecord(hash, title, poster, category, data, posterOK)
	if torr != nil {
		return torr
	} else {
		return torrDb
	}
}

// updateRecord changes a record that still exists, read again under the lock so a
// removal that ran meanwhile is not undone. The unlock is deferred: a recovered panic
// must not leave lifeMu held.
func updateRecord(hash metainfo.Hash, title, poster, category, data string, posterOK bool) *Torrent {
	lifeMu.Lock()
	defer lifeMu.Unlock()
	rec := GetTorrentDB(hash)
	if rec == nil {
		return nil
	}
	rec.Title = title
	rec.Poster = poster
	rec.Category = category
	if data != "" {
		rec.Data = data
	}
	if err := AddTorrentDB(rec, posterOK); err != nil {
		log.TLogln("update record: not saved:", hash)
	}
	return rec
}

// engineRemove closes the engine's copy of a torrent; a seam for the panic test.
var engineRemove = func(b *BTServer, hash metainfo.Hash) bool { return b.RemoveTorrent(hash) }

// removeUnderLock takes the engine's copy, the record and the save debounce in one
// step against a wake or a save. Both locks are released by defer, so a panic that
// gin's recovery swallows leaves nothing held.
func removeUnderLock(hash metainfo.Hash) bool {
	lifeMu.Lock()
	defer lifeMu.Unlock()
	btsMu.RLock()
	defer btsMu.RUnlock()
	removed := engineRemove(bts, hash)
	if err := RemTorrentDB(hash); err != nil {
		log.TLogln("remove record: not removed:", hash)
	}
	lastSaveTimes.Delete(hash)
	return removed
}

func RemTorrent(hashHex string) {
	if sets.ReadOnly {
		log.TLogln("API RemTorrent: Read-only DB mode!", hashHex)
		return
	}
	hash, ok := parseHash(hashHex)
	if !ok {
		log.TLogln("API RemTorrent: malformed hash:", hashHex)
		return
	}
	// The engine's copy, the record and the save debounce go in one step against a
	// wake or a save (lifeMu). The file deletes and the warmup cache come after:
	// they wait on the disk, and nothing can bring the torrent back by then.
	removed := removeUnderLock(hash)

	if removed {
		if sets.BTsets.UseDisk && hashHex != "" && hashHex != "/" {
			name := filepath.Join(sets.BTsets.TorrentsSavePath, hashHex)
			if _, err := os.Stat(name); err == nil {
				ff, _ := os.ReadDir(name)
				for _, f := range ff {
					_ = os.Remove(filepath.Join(name, f.Name())) // the Remove(name) below reports what is left
				}
				if err := os.Remove(name); err != nil {
					log.TLogln("Error remove cache:", err)
				}
			}
		}
	}
	// An explicit removal takes the torrent's head/tail cache with it. Idle expiry and
	// DropTorrent do not: the cache has to outlive them.
	if warmup.DiskWarmup != nil {
		warmup.DiskWarmup.RemoveHash(hashHex)
	}
}

func ListTorrent() []*Torrent {
	btsMu.RLock()
	localBts := bts
	btsMu.RUnlock()

	btlist := localBts.ListTorrents()
	dblist := ListTorrentsDB()

	for hash, t := range dblist {
		if _, ok := btlist[hash]; !ok {
			btlist[hash] = t
		}
	}
	var ret []*Torrent

	for _, t := range btlist {
		ret = append(ret, t)
	}

	sort.Slice(ret, func(i, j int) bool {
		if ret[i].Timestamp != ret[j].Timestamp {
			return ret[i].Timestamp > ret[j].Timestamp
		} else {
			return ret[i].Title > ret[j].Title
		}
	})

	return ret
}

// V304BannedCount returns the number of currently banned peer IPs, including bans restored
// from persisted state at startup (not just ones applied since this process started).
func V304BannedCount() int {
	return torrent.V304BannedCount()
}

func ListActiveTorrent() []*Torrent {
	btsMu.RLock()
	localBts := bts
	btsMu.RUnlock()

	if localBts == nil {
		return nil
	}

	btlist := localBts.ListTorrents()
	var ret []*Torrent

	for _, t := range btlist {
		ret = append(ret, t)
	}

	sort.Slice(ret, func(i, j int) bool {
		if ret[i].Timestamp != ret[j].Timestamp {
			return ret[i].Timestamp > ret[j].Timestamp
		} else {
			return ret[i].Title > ret[j].Title
		}
	})

	return ret
}

func DropTorrent(hashHex string) {
	btsMu.RLock()
	defer btsMu.RUnlock()

	hash, ok := parseHash(hashHex)
	if !ok {
		log.TLogln("API DropTorrent: malformed hash:", hashHex)
		return
	}
	bts.RemoveTorrent(hash)
}

func SetSettings(set *sets.BTSets) {
	if sets.ReadOnly {
		log.TLogln("API SetSettings: Read-only DB mode!")
		return
	}
	// V227: Serialize — prevent concurrent SetSettings from interleaving disconnect/connect
	settingsMu.Lock()
	defer settingsMu.Unlock()

	sets.SetBTSets(set)
	log.TLogln("drop all torrents")
	dropAllTorrent()
	time.Sleep(time.Millisecond * 200)
	log.TLogln("disconect")

	btsMu.Lock()
	bts.Disconnect()
	btsMu.Unlock()

	log.TLogln("connect")

	// Connect() calls InitApiHelper() which acquires btsMu.Lock() internally.
	// Do NOT wrap in btsMu.Lock here — RWMutex is not reentrant → deadlock.
	if err := reconnect(bts.Connect, 30, time.Second); err != nil {
		log.TLogln("connect failed:", err)
	}

	time.Sleep(time.Millisecond * 200)
	log.TLogln("end set settings")
}

// reconnect retries Connect: the closed client's uTP socket is released lazily, and until
// then the peer port fails with "address already in use", leaving bts.client nil.
func reconnect(connect func() error, attempts int, wait time.Duration) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = connect(); err == nil {
			if i > 0 {
				log.TLogln("connect ok, attempt", i+1)
			}
			return nil
		}
		if i == 0 {
			log.TLogln("connect error, retrying:", err)
		}
		if i < attempts-1 {
			time.Sleep(wait)
		}
	}
	return err
}

func SetDefSettings() {
	if sets.ReadOnly {
		log.TLogln("API SetDefSettings: Read-only DB mode!")
		return
	}
	// V227: Serialize — prevent concurrent settings changes
	settingsMu.Lock()
	defer settingsMu.Unlock()

	sets.SetDefaultConfig()
	log.TLogln("drop all torrents")
	dropAllTorrent()
	time.Sleep(time.Millisecond * 200)
	log.TLogln("disconect")

	btsMu.Lock()
	bts.Disconnect()
	btsMu.Unlock()

	log.TLogln("connect")

	// Connect() calls InitApiHelper() which acquires btsMu.Lock() internally.
	// Do NOT wrap in btsMu.Lock here — RWMutex is not reentrant → deadlock.
	if err := reconnect(bts.Connect, 30, time.Second); err != nil {
		log.TLogln("connect failed:", err)
	}

	time.Sleep(time.Millisecond * 200)
	log.TLogln("end set default settings")
}

func dropAllTorrent() {
	btsMu.RLock()
	localBts := bts
	btsMu.RUnlock()

	for _, torr := range localBts.ListTorrents() {
		torr.drop()
		<-torr.closed
	}
}

// StopEngine stops the engine and closes the settings database (every commit was already
// fsynced; the close releases the file). The daemon's shutdown calls it last, after the
// sessions are closed and the mount is gone. The HTTP servers still run, so a late write
// is possible: it takes lifeMu first, so a save in flight finishes before the close, and a
// later write is refused and logged, not applied.
func StopEngine() {
	utils.StopTrackerLoader()

	lifeMu.Lock()
	defer lifeMu.Unlock()
	btsMu.Lock()
	if bts != nil {
		bts.Disconnect()
	}
	btsMu.Unlock()

	sets.CloseDB()
}

func Shutdown() {
	utils.StopTrackerLoader()

	btsMu.Lock()
	bts.Disconnect()
	btsMu.Unlock()

	sets.CloseDB()
	log.TLogln("Received shutdown. Quit")
	os.Exit(0)
}

func Preload(torr *Torrent, index int) {
	cache := float32(sets.BTsets.CacheSize)
	preload := float32(sets.BTsets.PreloadCache)
	size := int64((cache / 100.0) * preload)
	if size <= 0 {
		return
	}
	if size > sets.BTsets.CacheSize {
		size = sets.BTsets.CacheSize
	}
	// V239: Leak Plumbing - Use context with timeout for API preloads
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	torr.Preload(ctx, index, size)
}
