package torr

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/anacrolix/torrent"

	"flodni/internal/gostorm/log"
)

// lifeMu serialises the three things that decide whether a torrent exists: loading
// it from its record (LoadStored, AddTorrent), saving it (saveTorrentToDB,
// SetTorrent) and removing it (RemTorrent). Without it a removal can run between a
// wake's read of the record and its add, or between an idle close's save and the
// removal, and the torrent comes back from the dead with a record of its own.
//
// Lock order: lifeMu is taken before btsMu, and never while holding btsMu,
// BTServer.mu or a Torrent's muTorrent. Nothing under it waits for metadata,
// deletes files or calls warmup.RemoveHash.
var lifeMu sync.Mutex

// newTorrent is how the lifecycle adds a torrent to the engine; tests replace it.
var newTorrent = NewTorrent

// ErrNotStored means the engine keeps no record of the hash that holds metainfo, so
// a wake refused it. A bare magnet for such a hash would be added with the public
// trackers and the DHT, and a private torrent (BEP 27) must never get those
// (hard rule 3).
var ErrNotStored = errors.New("the engine does not store this torrent")

// ErrEngineNotReady means the engine is not up yet, so nothing can be said about
// whether the hash is stored. It is transient and deliberately not an ErrNotStored:
// no caller may record it as "not stored", latch it, or ask an operator to re-add
// anything because of it.
var ErrEngineNotReady = errors.New("the engine is not ready yet")

var (
	engineReady     = make(chan struct{})
	engineReadyOnce sync.Once
)

// markEngineReady closes the ready signal, once: the engine's client is up.
func markEngineReady() { engineReadyOnce.Do(func() { close(engineReady) }) }

// WaitEngineReady blocks until the engine is up or the bound passes, and reports
// which. It returns at once when the engine already is.
func WaitEngineReady(bound time.Duration) bool { return waitSignal(engineReady, bound) }

func waitSignal(ready <-chan struct{}, bound time.Duration) bool {
	select {
	case <-ready:
		return true
	default:
	}
	t := time.NewTimer(bound)
	defer t.Stop()
	select {
	case <-ready:
		return true
	case <-t.C:
		return false
	}
}

// LoadStored wakes a torrent the engine closed, from its stored record only. It
// returns the wrapper the engine already holds, else adds one from the record's own
// spec (its metainfo, trackers and peers; no magnet is parsed) and restores what
// the record kept besides. A hash without a record, or whose record has no
// metainfo, is ErrNotStored: nothing is added. A wake before the engine is up is
// ErrEngineNotReady, which is not a verdict on the hash.
func LoadStored(hashHex string) (*Torrent, error) {
	hash, ok := parseHash(hashHex)
	if !ok {
		return nil, fmt.Errorf("%w: malformed hash", ErrNotStored)
	}
	lifeMu.Lock()
	defer lifeMu.Unlock()

	btsMu.RLock()
	localBts := bts
	btsMu.RUnlock()
	if localBts == nil {
		// No engine yet: nothing may be added, but this says nothing about the hash.
		return nil, ErrEngineNotReady
	}
	if held := localBts.GetTorrent(hash); held != nil {
		return held, nil
	}
	rec := GetTorrentDB(hash)
	if rec == nil || rec.TorrentSpec == nil {
		return nil, ErrNotStored
	}
	// A record without metainfo would be added as a bare magnet: its private flag
	// unknown, so the DHT and PEX stay on until the metainfo arrives.
	if len(rec.TorrentSpec.InfoBytes) == 0 {
		return nil, fmt.Errorf("%w: its record holds no metainfo", ErrNotStored)
	}
	t, err := newTorrent(rec.TorrentSpec, localBts)
	if err != nil {
		log.TLogln("error load stored torrent:", err)
		return nil, err
	}
	t.Title = rec.Title
	t.Poster = rec.Poster
	t.Data = rec.Data
	t.Size = rec.Size
	t.Timestamp = rec.Timestamp
	t.Category = rec.Category
	return t, nil
}

// addUnderLock is AddTorrent's first half: it fills the spec from the record and
// adds the torrent, as one step against a removal.
func addUnderLock(spec *torrent.TorrentSpec) (*Torrent, error) {
	lifeMu.Lock()
	defer lifeMu.Unlock()

	// V255: Inject cached InfoBytes + PeerAddrs from DB to skip metadata re-fetch
	// and peer discovery. Magnet links always have InfoBytes=nil and PeerAddrs=nil.
	// If this torrent was previously saved, the DB has both — inject them so
	// GotInfo() fires immediately and peers connect without waiting for tracker/DHT.
	if cached := GetTorrentDB(spec.InfoHash); cached != nil && cached.TorrentSpec != nil {
		if len(spec.InfoBytes) == 0 && len(cached.TorrentSpec.InfoBytes) > 0 {
			spec.InfoBytes = cached.TorrentSpec.InfoBytes
		}
		if len(spec.PeerAddrs) == 0 && len(cached.TorrentSpec.PeerAddrs) > 0 {
			spec.PeerAddrs = cached.TorrentSpec.PeerAddrs
		}
		// The trackers the torrent was first added with (its indexer's, possibly with the
		// user's passkey) come back too.
		spec.Trackers = mergeTrackerTiers(spec.Trackers, cached.TorrentSpec.Trackers)
	}

	btsMu.RLock()
	localBts := bts
	btsMu.RUnlock()

	return newTorrent(spec, localBts)
}
