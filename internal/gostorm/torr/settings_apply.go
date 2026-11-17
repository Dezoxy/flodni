package torr

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"flodni/internal/gostorm/log"
	sets "flodni/internal/gostorm/settings"

	"github.com/anacrolix/torrent"
)

// Seams: the settings path is driven in tests without a live torrent client.
var (
	storeSettings    = sets.SetBTSets
	storeDefaults    = sets.SetDefaultConfig
	dropAllTorrentFn = dropAllTorrent
	disconnectFn     = func() {
		btsMu.Lock()
		bts.Disconnect()
		btsMu.Unlock()
	}
	// Connect() calls InitApiHelper(), which takes btsMu.Lock itself: do NOT wrap this
	// in btsMu.Lock, an RWMutex is not reentrant.
	connectFn  = func() error { return reconnect(bts.Connect, 30, time.Second) }
	settleWait = func() { time.Sleep(200 * time.Millisecond) }
)

// playbackProbe reports how many confirmed playing sessions the daemon sees. The engine
// does not know about playback; the daemon registers the probe at start.
var playbackProbe atomic.Pointer[func() int]

// SetPlaybackProbe registers the daemon's count of confirmed playing sessions.
func SetPlaybackProbe(f func() int) { playbackProbe.Store(&f) }

func playingSessions() int {
	if p := playbackProbe.Load(); p != nil {
		return (*p)()
	}
	return 0
}

// RestartRefused is returned instead of a rebuild while a session is playing.
type RestartRefused struct {
	Fields   []string // the CLIENT fields the request changes
	Sessions int      // confirmed playing sessions
}

func (e *RestartRefused) Error() string {
	return fmt.Sprintf("settings change needs an engine restart (%s) and %d playing session(s) would be interrupted; resend with force to restart anyway",
		strings.Join(e.Fields, ", "), e.Sessions)
}

// SetSettings applies set. A change to HOT fields only is stored and picked up in place:
// no torrent is dropped and the client stays. A change to a CLIENT field rebuilds the
// engine (drop every torrent, Disconnect, Connect), which a playing session would
// notice, so it is refused with *RestartRefused while the daemon counts a confirmed
// playing session, unless force is set. A refused request stores nothing.
//
// Locks: settingsMu serializes the whole call. The playback probe runs under it and takes
// only the daemon's own maps. Neither store nor hot apply nest btsMu inside bt.mu.
func SetSettings(set *sets.BTSets, force bool) error {
	if sets.ReadOnly {
		log.TLogln("API SetSettings: Read-only DB mode!")
		return nil
	}
	settingsMu.Lock()
	defer settingsMu.Unlock()
	sets.Normalize(set)
	return applyChange(*set, force, func() { storeSettings(set) })
}

// SetDefSettings resets to the defaults under the same rules as SetSettings.
func SetDefSettings(force bool) error {
	if sets.ReadOnly {
		log.TLogln("API SetDefSettings: Read-only DB mode!")
		return nil
	}
	settingsMu.Lock()
	defer settingsMu.Unlock()
	return applyChange(*sets.DefaultBTSets(), force, storeDefaults)
}

// applyChange is called with settingsMu held and next already normalized.
func applyChange(next sets.BTSets, force bool, store func()) error {
	_, client := sets.Diff(sets.Snapshot(), next)
	if len(client) == 0 {
		store()
		applyHot()
		log.TLogln("settings applied in place")
		return nil
	}
	if !force {
		if n := playingSessions(); n > 0 {
			log.TLogln("settings change refused: engine restart under", n, "playing session(s):", strings.Join(client, ","))
			return &RestartRefused{Fields: client, Sessions: n}
		}
	}
	store()
	rebuildEngine()
	return nil
}

// applyHot hands the engine what it cached from HOT fields. Only CacheSize is cached:
// the storage capacity new caches are opened with, and the reader's read-ahead ceiling.
// Caches of torrents that are already loaded keep the size they were opened with.
func applyHot() {
	btsMu.RLock()
	b := bts
	btsMu.RUnlock()
	if b == nil {
		return
	}
	size := sets.Snapshot().CacheSize
	if _, storage := b.clientAndStorage(); storage != nil {
		storage.SetCapacity(size)
	}
	torrent.SetMaxReadahead(size)
}

func rebuildEngine() {
	log.TLogln("drop all torrents")
	dropAllTorrentFn()
	settleWait()
	log.TLogln("disconect")
	disconnectFn()
	log.TLogln("connect")
	if err := connectFn(); err != nil {
		log.TLogln("connect failed:", err)
	}
	settleWait()
	log.TLogln("end set settings")
}
