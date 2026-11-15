package torr

import (
	"errors"
	"flodni/internal/gostorm/torrshash"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	utils2 "flodni/internal/gostorm/utils"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"flodni/internal/gostorm/log"
	"flodni/internal/gostorm/settings"
	"flodni/internal/gostorm/torr/state"
	cacheSt "flodni/internal/gostorm/torr/storage/state"
	"flodni/internal/gostorm/torr/storage/torrstor"
	"flodni/internal/gostorm/torr/utils"
)

type Torrent struct {
	Title    string
	Category string
	Poster   string
	Data     string
	*torrent.TorrentSpec

	Stat      state.TorrentStat
	Timestamp int64
	Size      int64

	*torrent.Torrent
	muTorrent sync.Mutex

	bt    *BTServer
	cache *torrstor.Cache

	lastTimeSpeed       time.Time
	DownloadSpeed       float64
	UploadSpeed         float64
	BytesReadUsefulData int64
	BytesWrittenData    int64

	PreloadSize    int64
	PreloadedBytes int64

	DurationSeconds float64
	BitRate         string

	expiredTime time.Time
	IsPriority  atomic.Bool // V185: aggressive mode for the active stream (tuner); no longer holds the idle timeout, see HoldExpiry

	closed <-chan struct{}

	cachedFileStats []*state.TorrentFileStat
}

// infoPublic reports metadata that is known and not flagged private (BEP 27).
// Unknown metadata is not public: a private torrent must not be given public
// trackers while it could still turn out to be one.
func infoPublic(info *metainfo.Info) bool {
	return info != nil && (info.Private == nil || !*info.Private)
}

// infoBytesPublic is infoPublic for raw metainfo: absent or unreadable bytes are not public.
func infoBytesPublic(b []byte) bool {
	var info metainfo.Info
	return len(b) > 0 && bencode.Unmarshal(b, &info) == nil && infoPublic(&info)
}

// dropPublicTrackers removes from a private torrent's trackers every one of the public
// lists we add ourselves (hard rule 3), keeping the torrent's own. A stored spec carries
// whatever trackers the torrent was saved with, which for a torrent added before the
// retracker gate includes them; a public torrent is left alone.
func dropPublicTrackers(spec *torrent.TorrentSpec, public func() []string) {
	var info metainfo.Info
	if len(spec.InfoBytes) == 0 || bencode.Unmarshal(spec.InfoBytes, &info) != nil || infoPublic(&info) {
		return
	}
	drop := map[string]bool{}
	for _, tr := range public() {
		drop[tr] = true
	}
	var tiers [][]string
	for _, tier := range spec.Trackers {
		var kept []string
		for _, tr := range tier {
			if !drop[tr] {
				kept = append(kept, tr)
			}
		}
		if len(kept) > 0 {
			tiers = append(tiers, kept)
		}
	}
	spec.Trackers = tiers
}

// publicTrackers is every tracker we add to torrents on our own account.
func publicTrackers() []string {
	return append(append(utils.BuiltinTrackers(), utils.GetDefTrackers()...), utils.GetTrackerFromFile()...)
}

// retrackSpec applies the retracker mode and trackers.txt to spec, but only when
// spec carries metadata that is not private. A private torrent keeps exactly the
// trackers of its own metainfo (hard rule 3). Without metadata the choice waits
// for it: see retrackWhenPublic.
func retrackSpec(spec *torrent.TorrentSpec, mode int, defTrackers, fileTrackers func() []string) {
	var info metainfo.Info
	if len(spec.InfoBytes) == 0 || bencode.Unmarshal(spec.InfoBytes, &info) != nil || !infoPublic(&info) {
		return
	}
	applyRetrackers(spec, mode, defTrackers, fileTrackers)
}

func applyRetrackers(spec *torrent.TorrentSpec, mode int, defTrackers, fileTrackers func() []string) {
	switch mode {
	case 1:
		spec.Trackers = append(spec.Trackers, [][]string{defTrackers()}...)
	case 2:
		spec.Trackers = nil
	case 3:
		spec.Trackers = [][]string{defTrackers()}
	}

	trackers := fileTrackers()
	if len(trackers) > 0 {
		spec.Trackers = append(spec.Trackers, [][]string{trackers}...)
	}
}

// retrackWhenPublic gives a torrent added without metadata the public trackers once
// the metadata shows it is not private, which is when a public torrent added by magnet
// gets what retrackSpec would have given it at the add.
func retrackWhenPublic(tor *torrent.Torrent, mode int, defTrackers, fileTrackers func() []string) {
	select {
	case <-tor.GotInfo():
	case <-tor.Closed():
		return
	}
	if !infoPublic(tor.Info()) {
		return
	}
	spec := &torrent.TorrentSpec{}
	applyRetrackers(spec, mode, defTrackers, fileTrackers)
	if len(spec.Trackers) > 0 {
		tor.AddTrackers(spec.Trackers)
	}
}

// heldTakesTrackers reports whether an add may give its trackers to the torrent the
// engine already holds: only when the held torrent is known public. Without metainfo
// it may be private, so it takes none (a .torrent for it is added by remove, then add).
func heldTakesTrackers(held *Torrent) bool {
	if held.Torrent == nil {
		return false
	}
	info := held.Torrent.Info()
	return info != nil && infoPublic(info)
}

func NewTorrent(spec *torrent.TorrentSpec, bt *BTServer) (*Torrent, error) {
	// https://github.com/anacrolix/torrent/issues/747
	if bt == nil {
		return nil, errors.New("BT client not connected")
	}
	dropPublicTrackers(spec, publicTrackers)
	retrackSpec(spec, settings.BTsets.RetrackersMode, utils.GetDefTrackers, utils.GetTrackerFromFile)

	// Disconnect nils bt.client under bt.mu during a settings reconnect: read it under the lock.
	client, _ := bt.clientAndStorage()
	if client == nil {
		return nil, errors.New("BT client not connected")
	}

	// AddTorrentSpec merges a spec's trackers into a torrent the engine already holds.
	// A held torrent that is not known public takes none from an add (hard rule 3):
	// the live torrent, and the record the next save writes, keep their own.
	addSpec := spec
	if held := bt.GetTorrent(spec.InfoHash); held != nil && !heldTakesTrackers(held) {
		if n := countNewTrackers(spec.Trackers, nil); n > 0 {
			log.TLogln("add: ignored", n, "request trackers for a held torrent that is not public")
		}
		noTrackers := *spec
		noTrackers.Trackers = nil
		addSpec = &noTrackers
	}
	goTorrent, _, err := client.AddTorrentSpec(addSpec)
	if err != nil {
		return nil, err
	}

	bt.mu.Lock()
	defer bt.mu.Unlock()
	if tor, ok := bt.torrents[spec.InfoHash]; ok {
		return tor, nil
	}

	timeout := time.Second * time.Duration(settings.BTsets.TorrentDisconnectTimeout)
	if timeout > time.Minute {
		timeout = time.Minute
	}

	torr := new(Torrent)
	torr.Torrent = goTorrent
	torr.Stat = state.TorrentAdded
	torr.lastTimeSpeed = time.Now()
	torr.bt = bt
	torr.closed = goTorrent.Closed()
	torr.TorrentSpec = spec
	torr.AddExpiredTime(timeout)
	torr.Timestamp = time.Now().Unix()

	bt.torrents[spec.InfoHash] = torr
	if len(spec.InfoBytes) == 0 {
		go retrackWhenPublic(goTorrent, settings.BTsets.RetrackersMode, utils.GetDefTrackers, utils.GetTrackerFromFile)
	}
	return torr, nil
}

func (t *Torrent) WaitInfo() bool {
	if t == nil {
		return false
	}
	// Snapshot the anacrolix torrent once: drop() nils t.Torrent under muTorrent, so
	// rereading the field inside the select below can dereference nil.
	tor := t.UnderlyingTorrent()
	if tor == nil {
		return false
	}

	// Close torrent if no info in 1 minute + TorrentDisconnectTimeout config option
	tm := time.NewTimer(time.Minute + time.Second*time.Duration(settings.BTsets.TorrentDisconnectTimeout))
	defer tm.Stop()

	select {
	case <-tor.GotInfo():
		return t.attachCache(tor)
	case <-t.closed:
		return false
	case <-tm.C:
		return false
	}
}

// attachCache binds the storage cache to the anacrolix torrent. A nil cache means the
// torrent was dropped while WaitInfo was blocked: CloseHash removed it from storage, so
// the torrent is not usable and the caller must treat it as unavailable.
func (t *Torrent) attachCache(tor *torrent.Torrent) bool {
	if t == nil {
		return false
	}
	if t.bt == nil {
		return true
	}
	_, storage := t.bt.clientAndStorage()
	if storage == nil {
		return true
	}
	cache := storage.GetCache(tor.InfoHash())
	if cache == nil {
		return false
	}
	t.cache = cache
	cache.SetTorrent(tor)
	return true
}

// UnderlyingTorrent returns the anacrolix torrent this wrapper drives, or nil once the
// wrapper has been closed. It takes muTorrent internally and must not be called with
// muTorrent already held (the mutex is not reentrant).
func (t *Torrent) UnderlyingTorrent() *torrent.Torrent {
	if t == nil {
		return nil
	}
	t.muTorrent.Lock()
	defer t.muTorrent.Unlock()
	return t.Torrent
}

func (t *Torrent) GotInfo() bool {
	if t == nil {
		return false
	}
	t.muTorrent.Lock()
	if t.Stat == state.TorrentClosed {
		t.muTorrent.Unlock()
		return false
	}
	if t.Stat == state.TorrentPreload {
		t.muTorrent.Unlock()
		return true
	}
	t.Stat = state.TorrentGettingInfo
	t.muTorrent.Unlock()
	if t.WaitInfo() {
		t.muTorrent.Lock()
		t.Stat = state.TorrentWorking
		t.muTorrent.Unlock()
		t.AddExpiredTime(time.Second * time.Duration(settings.BTsets.TorrentDisconnectTimeout))
		return true
	} else {
		t.Close()
		return false
	}
}

func (t *Torrent) AddExpiredTime(duration time.Duration) {
	newExpiredTime := time.Now().Add(duration)
	t.muTorrent.Lock()
	if t.expiredTime.Before(newExpiredTime) {
		t.expiredTime = newExpiredTime
	}
	t.muTorrent.Unlock()
}

// OnIdleClose, when set, is told the hash of every torrent the ticker closed for
// idleness. It only counts: it must not call back into the engine. It runs after
// muTorrent is released. Set it once at start, before the ticker runs.
var OnIdleClose func(hash string)

// UpdateStats is called periodically by the central BTServer ticker
func (t *Torrent) UpdateStats() {
	t.muTorrent.Lock()
	if t.expired() {
		t.muTorrent.Unlock()
		if t.TorrentSpec != nil {
			log.TLogln("Torrent close by timeout", t.TorrentSpec.InfoHash.HexString())
			// V255: Snapshot peers to DB before drop. At expiry the swarm is fullest
			// (tracker/DHT have responded). Next Wake() injects these as Trusted peers,
			// skipping discovery delay. Force=true: bypass debounce, this is the final save.
			ForceSaveTorrentToDB(t)
		}
		hash := t.Hash()
		if t.bt.RemoveTorrent(hash) && OnIdleClose != nil {
			OnIdleClose(hash.HexString())
		}
		return
	}

	if t.Torrent != nil && t.Torrent.Info() != nil {
		st := t.Torrent.Stats()
		deltaDlBytes := st.BytesRead.Int64() - t.BytesReadUsefulData
		deltaUpBytes := st.BytesWritten.Int64() - t.BytesWrittenData
		deltaTime := time.Since(t.lastTimeSpeed).Seconds()

		t.DownloadSpeed = float64(deltaDlBytes) / deltaTime
		t.UploadSpeed = float64(deltaUpBytes) / deltaTime

		t.BytesReadUsefulData = st.BytesRead.Int64()
		t.BytesWrittenData = st.BytesWritten.Int64()

		if t.cache != nil {
			t.PreloadedBytes = t.cache.GetState().Filled
		}
	} else {
		t.DownloadSpeed = 0
		t.UploadSpeed = 0
	}
	t.lastTimeSpeed = time.Now()
	t.muTorrent.Unlock()

	t.updateRA()
}

func (t *Torrent) updateRA() {
	if t.cache == nil {
		return
	}
	adj := torrstor.CacheReadahead()
	go t.cache.AdjustRA(adj)
}

// IsStreaming returns true if the torrent has active FUSE readers or non-zero download speed.
// Used by listActiveTorrents to avoid serializing idle bt.torrents entries.
func (t *Torrent) IsStreaming() bool {
	if t.cache != nil && t.cache.Readers() > 0 {
		return true
	}
	t.muTorrent.Lock()
	s := t.DownloadSpeed
	t.muTorrent.Unlock()
	return s > 0
}

func (t *Torrent) expired() bool {
	readers := 0
	if t.cache != nil {
		readers = t.cache.Readers()
	}
	return expiryVerdict(t.Stat, t.expiredTime.Before(time.Now()), t.cache != nil, readers,
		func() bool { return holdExpiry(t.Hash().HexString()) })
}

// HoldExpiry, when set, is asked for every torrent that would otherwise be closed for
// idleness: true keeps it open for this tick. nil means no hold. It runs under
// muTorrent on the ticker, so it must not call into the engine (Status, Stats and the
// like take the same non-reentrant lock). The keep-alive (Reliability step 8, phase b)
// sets it from the playback state; the engine knows nothing about playback.
var HoldExpiry func(hash string) bool

// holdExpiry asks HoldExpiry and treats a panic as no hold: a panic here would leave
// muTorrent locked and end the ticker.
func holdExpiry(hash string) (held bool) {
	f := HoldExpiry
	if f == nil {
		return false
	}
	defer func() {
		if r := recover(); r != nil {
			log.TLogln("[KeepAlive] hold predicate panicked, not holding:", r)
			held = false
		}
	}()
	return f(hash)
}

// expiryVerdict is the pure expiry decision. A torrent still getting its info or
// preloading never expires; neither does one whose timer has not passed or that has a
// reader. One with no cache (GotInfo failed or never ran) expires, since a zombie
// holds nothing worth keeping. Only then is the hold asked, so a scan or a busy
// torrent never costs a predicate call.
func expiryVerdict(stat state.TorrentStat, timerPassed, hasCache bool, readers int, held func() bool) bool {
	if stat == state.TorrentGettingInfo || stat == state.TorrentPreload {
		return false
	}
	if !timerPassed {
		return false
	}
	if !hasCache {
		return true
	}
	if readers != 0 {
		return false
	}
	return held == nil || !held()
}

// SetAggressiveMode enables or disables aggressive download priority in the cache
func (t *Torrent) SetAggressiveMode(enabled bool, masterLimit int) {
	if t.cache != nil {
		t.cache.SetAggressive(enabled, masterLimit)
	}
}

func (t *Torrent) Files() []*torrent.File {
	if t.Torrent != nil && t.Torrent.Info() != nil {
		files := t.Torrent.Files()
		return files
	}
	return nil
}

func (t *Torrent) Hash() metainfo.Hash {
	if t.Torrent != nil {
		return t.Torrent.InfoHash()
	}
	if t.TorrentSpec != nil {
		return t.TorrentSpec.InfoHash
	}
	return [20]byte{}
}

func (t *Torrent) Length() int64 {
	if t.Info() == nil {
		return 0
	}
	return t.Torrent.Length()
}

func (t *Torrent) NewReader(file *torrent.File) *torrstor.Reader {
	// V244-Fix: Capture cache locally to avoid race where t.cache becomes nil after check
	cache := t.cache
	if t.Stat == state.TorrentClosed || cache == nil {
		return nil
	}
	reader := cache.NewReader(file)
	return reader
}

func (t *Torrent) CloseReader(reader *torrstor.Reader) {
	if t.cache != nil {
		t.cache.CloseReader(reader)
	}
	t.AddExpiredTime(time.Second * time.Duration(settings.BTsets.TorrentDisconnectTimeout))
}

func (t *Torrent) GetCache() *torrstor.Cache {
	return t.cache
}

// dropAllowed reports whether anacrolix still holds this exact torrent. Its Drop
// panics with "no such torrent" when the object was already dropped from the client:
// Client.AddTorrentSpec drops a new torrent itself when MergeSpec fails, and a double
// close reaches the same object through a stale pointer.
func dropAllowed(current *torrent.Torrent, held bool, ours *torrent.Torrent) bool {
	return ours != nil && held && current == ours
}

func (t *Torrent) drop() {
	// The check and the Drop must be one critical section across every wrapper of the
	// same hash: two *Torrent have different muTorrent, so per-wrapper locking alone
	// lets both pass the check before either drops (TOCTOU).
	var client *torrent.Client
	if t.bt != nil {
		client, _ = t.bt.clientAndStorage()
	}
	if client != nil {
		t.bt.dropMu.Lock()
		defer t.bt.dropMu.Unlock()
	}
	t.muTorrent.Lock()
	defer t.muTorrent.Unlock()
	if t.Torrent == nil {
		return
	}
	if client == nil {
		// No client to verify against: "cannot verify" is "do not drop", because
		// Drop panics on an object the client no longer holds.
		log.TLogln("[Drop] no client, skipping", t.Hash().HexString())
		t.Torrent = nil
		return
	}
	if current, held := client.Torrent(t.Torrent.InfoHash()); !dropAllowed(current, held, t.Torrent) {
		t.Torrent = nil
		return
	}
	t.Torrent.Drop()
	t.Torrent = nil
}

func (t *Torrent) Close() bool {
	if t == nil {
		return false
	}
	if settings.ReadOnly && t.cache != nil && t.cache.GetUseReaders() > 0 {
		return false
	}
	t.muTorrent.Lock()
	t.Stat = state.TorrentClosed
	t.muTorrent.Unlock()

	if t.bt != nil {
		t.bt.mu.Lock()
		delete(t.bt.torrents, t.Hash())
		t.bt.mu.Unlock()
	}

	t.drop()

	// CloseHash was previously dead code: Storage.caches[hash] was never evicted, so every
	// torrent ever touched during the process lifetime kept its *Cache — and every piece
	// buffer received from peers (torrstor.MemPiece.buffer) — permanently reachable, even
	// after the torrent itself expired and was dropped. Cache.Close() is idempotent (guarded
	// by isClosed), so this is safe even if anacrolix's storage.TorrentImpl.Close already fired.
	if t.bt != nil {
		if _, storage := t.bt.clientAndStorage(); storage != nil {
			storage.CloseHash(t.Hash())
		}
	}
	return true
}

func (t *Torrent) Status() *state.TorrentStatus {
	st := new(state.TorrentStatus)

	t.muTorrent.Lock()
	st.Stat = t.Stat
	st.StatString = t.Stat.String()
	st.Title = t.Title
	st.Category = t.Category
	st.Poster = t.Poster
	st.Data = t.Data
	st.Timestamp = t.Timestamp
	st.TorrentSize = t.Size
	st.BitRate = t.BitRate
	st.DurationSeconds = t.DurationSeconds
	st.IsPriority = t.IsPriority.Load() // V186
	st.PreloadedBytes = t.PreloadedBytes
	st.PreloadSize = t.PreloadSize
	st.DownloadSpeed = t.DownloadSpeed
	st.UploadSpeed = t.UploadSpeed

	if t.TorrentSpec != nil {
		st.Hash = t.TorrentSpec.InfoHash.HexString()
	}

	if t.Torrent != nil && t.Torrent.Info() != nil {
		if t.cachedFileStats == nil {
			files := t.Files()
			sort.Slice(files, func(i, j int) bool {
				return utils2.CompareStrings(files[i].Path(), files[j].Path())
			})
			for i, f := range files {
				t.cachedFileStats = append(t.cachedFileStats, &state.TorrentFileStat{
					Id:     i + 1, // in web id 0 is undefined
					Path:   f.Path(),
					Length: f.Length(),
				})
			}
		}
		st.FileStats = t.cachedFileStats
	}

	torr := t.Torrent
	t.muTorrent.Unlock() // V227: Release early to prevent bottleneck during heavy stats/sort

	if torr != nil {
		st.Name = torr.Name()
		st.Hash = torr.InfoHash().HexString()
		st.LoadedSize = torr.BytesCompleted()

		tst := torr.Stats()
		st.BytesWritten = tst.BytesWritten.Int64()
		st.BytesWrittenData = tst.BytesWrittenData.Int64()
		st.BytesRead = tst.BytesRead.Int64()
		st.BytesReadData = tst.BytesReadData.Int64()
		st.BytesReadUsefulData = tst.BytesReadUsefulData.Int64()
		st.ChunksWritten = tst.ChunksWritten.Int64()
		st.ChunksRead = tst.ChunksRead.Int64()
		st.ChunksReadUseful = tst.ChunksReadUseful.Int64()
		st.ChunksReadWasted = tst.ChunksReadWasted.Int64()
		st.PiecesDirtiedGood = tst.PiecesDirtiedGood.Int64()
		st.PiecesDirtiedBad = tst.PiecesDirtiedBad.Int64()
		st.TotalPeers = tst.TotalPeers
		st.PendingPeers = tst.PendingPeers
		st.ActivePeers = tst.ActivePeers
		st.ConnectedSeeders = tst.ConnectedSeeders
		st.HalfOpenPeers = tst.HalfOpenPeers

		if torr.Info() != nil {
			st.TorrentSize = torr.Length()

			th := torrshash.New(st.Hash)
			th.AddField(torrshash.TagTitle, st.Title)
			th.AddField(torrshash.TagPoster, st.Poster)
			th.AddField(torrshash.TagCategory, st.Category)
			th.AddField(torrshash.TagSize, strconv.FormatInt(st.TorrentSize, 10))

			if t.TorrentSpec != nil {
				if len(t.TorrentSpec.Trackers) > 0 && len(t.TorrentSpec.Trackers[0]) > 0 {
					for _, tr := range t.TorrentSpec.Trackers[0] {
						th.AddField(torrshash.TagTracker, tr)
					}
				}
			}
			token, err := torrshash.Pack(th)
			if err == nil {
				st.TorrsHash = token
			}
		}
	}

	return st
}

func (t *Torrent) CacheState() *cacheSt.CacheState {
	if t.Torrent != nil && t.cache != nil {
		st := t.cache.GetState()
		st.Torrent = t.Status()
		return st
	}
	return nil
}

// V162-Optimization: Lightweight status for high-frequency polling (PeerPreloader)
// Avoids the massive overhead of Status() which locks for too long causing starvation
func (t *Torrent) StatHighFreq() *state.TorrentStatus {
	t.muTorrent.Lock()
	defer t.muTorrent.Unlock()

	st := new(state.TorrentStatus)
	st.Hash = t.TorrentSpec.InfoHash.HexString()

	// Only copy essential fields for PeerPreloader
	st.DownloadSpeed = t.DownloadSpeed
	st.IsPriority = t.IsPriority.Load() // V186

	if t.Torrent != nil {
		tst := t.Torrent.Stats()
		st.TotalPeers = tst.TotalPeers
		st.ActivePeers = tst.ActivePeers
		st.ConnectedSeeders = tst.ConnectedSeeders
		st.LoadedSize = t.Torrent.BytesCompleted()
	}

	return st
}
