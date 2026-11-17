package daemon

import (
	"flodni/internal/gostorm/web"
	"flodni/internal/metadb"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

// PlaybackState traccia lo stato di una sessione di visione reale
type PlaybackState struct {
	mu          sync.RWMutex
	Path        string
	Hash        string // InfoHash for GoStorm priority management
	ImdbID      string // IMDB ID from MKV line 4, used for webhook matching
	OpenedAt    time.Time
	ConfirmedAt time.Time // Set when Plex webhook arrives
	// SessionStart is when a media.play webhook last started a session for this file. Resume,
	// pause and stop leave it alone: it is what says which handles opened before the player.
	SessionStart time.Time
	IsHealthy    bool // Confirmed by Plex
	IsStopped    bool // Set on explicit media.stop webhook
	// V750: Inferred playback detection (self-healing when webhook is lost)
	ReadCount   int64
	LastSeekOff int64
	LastReadAt  time.Time
}

func (ps *PlaybackState) SetHealthy(healthy bool) {
	ps.mu.Lock()
	ps.IsHealthy = healthy
	ps.ConfirmedAt = time.Now()
	ps.mu.Unlock()
	// V750: Persist to SQLite
	savePlaybackStateToDB(ps)
}

// V750: Determine if this path shows evidence of active playback,
// even without a webhook confirmation.
func (ps *PlaybackState) IsInferredPlayback() bool {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	if ps.IsHealthy || ps.IsStopped {
		return false
	}
	now := time.Now()
	active := !ps.LastReadAt.IsZero() && now.Sub(ps.LastReadAt) < 10*time.Minute
	significantReads := ps.ReadCount >= 3
	streaming := ps.LastSeekOff > 2*1024*1024 // >2MB offset = real playback, not scanner
	return active && significantReads && streaming
}

func (ps *PlaybackState) GetOpenedAt() time.Time {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.OpenedAt
}

func (ps *PlaybackState) GetImdbID() string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.ImdbID
}

// LastSignOfLife is the later of the open and the webhook confirmation.
func (ps *PlaybackState) LastSignOfLife() time.Time {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	if ps.ConfirmedAt.After(ps.OpenedAt) {
		return ps.ConfirmedAt
	}
	return ps.OpenedAt
}

// sessionStartedAt is when a play webhook last started a session for the file, zero if none
// did (a restart, or a playback only inferred from reads).
func (ps *PlaybackState) sessionStartedAt() time.Time {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.SessionStart
}

func (ps *PlaybackState) GetStatus() bool {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.IsHealthy
}

// playbackOpen reports whether anyone may be watching: a read-ahead pump is running, or
// a session confirmed by the media server (or inferred from its reads) still has a live
// FUSE handle. States restored at start stay confirmed with no handle, so the handle is
// what says the stream is open. Warm on add pauses on it. The answer is cached for one
// second: every worker and every fill poll asks.
func playbackOpen() bool {
	playbackOpenCache.mu.Lock()
	defer playbackOpenCache.mu.Unlock()
	now := playbackOpenNow()
	if !playbackOpenCache.at.IsZero() && now.Sub(playbackOpenCache.at) < time.Second {
		return playbackOpenCache.v
	}
	playbackOpenCache.v = computePlaybackOpen()
	playbackOpenCache.at = now
	return playbackOpenCache.v
}

var (
	playbackOpenNow   = time.Now
	playbackOpenCache struct {
		mu sync.Mutex
		at time.Time
		v  bool
	}
)

func computePlaybackOpen() bool {
	open := false
	app.activePumps.Range(func(_, _ any) bool { open = true; return false })
	if open {
		return true
	}
	app.playbackRegistry.Range(func(k, v any) bool {
		ps, ok := v.(*PlaybackState)
		if ok && (ps.GetStatus() || ps.IsInferredPlayback()) && anyLiveHandleFor(k.(string)) {
			open = true
			return false
		}
		return true
	})
	return open
}

// confirmedPlayingSessions counts the sessions the media server confirmed (webhook, no
// stop) that still have a live FUSE handle: the sessions an engine restart would
// interrupt. States restored at start stay confirmed with no handle, so the handle says
// the stream is open. Inferred playback is not counted: it is a guess from read patterns.
func confirmedPlayingSessions() int {
	n := 0
	app.playbackRegistry.Range(func(k, v any) bool {
		ps, ok := v.(*PlaybackState)
		if !ok || !ps.GetStatus() {
			return true
		}
		ps.mu.RLock()
		stopped := ps.IsStopped
		ps.mu.RUnlock()
		if !stopped && anyLiveHandleFor(k.(string)) {
			n++
		}
		return true
	})
	return n
}

// startPumpAtOpen decides whether Open() starts the read-ahead pump (Dezoxy fork).
func startPumpAtOpen(pumpOnOpen, confirmedPlayback bool) bool {
	return pumpOnOpen || confirmedPlayback
}

// playbackConfirmed reports a webhook-confirmed (healthy) playback for path.
// isScanRead reports whether a handle is being probed, not played (Dezoxy fork): no pump
// is attached and nothing says playback, neither a webhook nor the read pattern. Scan
// reads fetch only what they need and fill the index windows. With pump_on_open every
// open gets a pump, so upstream's behaviour is unchanged.
func isScanRead(h *MkvHandle) bool {
	if gc().PumpOnOpen || h.hasSlot.Load() {
		return false
	}
	if val, ok := app.playbackRegistry.Load(h.path); ok {
		if ps, ok := val.(*PlaybackState); ok && (ps.GetStatus() || ps.IsInferredPlayback()) {
			return false
		}
	}
	return true
}

// scanFetchSize sizes a scan read's network fetch: the rest of the head window when the
// read lands in it, so one fetch fills it, otherwise only the bytes asked for. Never more
// than the normal read-ahead.
func scanFetchSize(off, target, headSize, readAhead int64) int64 {
	size := target
	if off < headSize && headSize-off > size {
		size = headSize - off
	}
	if size > readAhead {
		size = readAhead
	}
	if size < target {
		size = target
	}
	return size
}

func playbackConfirmed(path string) bool {
	if val, ok := app.playbackRegistry.Load(path); ok {
		if ps, ok := val.(*PlaybackState); ok {
			return ps.GetStatus()
		}
	}
	return false
}

// exactMatchEntry is one playbackRegistry entry: a virtual path and its state.
type exactMatchEntry struct {
	path  string
	state *PlaybackState
}

// exactMatchKeys carries the webhook-side keys used by pass-1 matching.
type exactMatchKeys struct {
	imdbID    string
	basenames []string
	// mappedBasenames is asked only when no basename matched, before the IMDB id: the
	// stub names the library's symlinks point to (pathMapStubNames). Nil means off.
	mappedBasenames func() []string
	// linked breaks a tie between several IMDb candidates (linked_stubs.go). Nil means off.
	linked linkedSnap
}

// findExactMatchRule selects the pass-1 match among the registry entries.
//
// The hash suffix is deliberately NOT a key: every episode of a season pack shares
// the same hash8 in its filename (HashPrefix = hash[:8]), so a suffix-only match can
// bind the wrong episode — e.g. the media.stop of E01 killing the pump of E02 that
// is actually playing. The filename (physical identity) wins over the IMDB ID
// (metadata identity), which also protects against an IMDB id polluted by a
// previous mis-matched media.play.
//
// It also says which rule matched: ruleBasename, rulePathMap, ruleIMDB or ruleLinked
// (ruleNone when nothing did). The path map sits between the file name and the IMDB id.
// When several entries carry the IMDB id, the only one a library symlink points at wins
// as ruleLinked; with none or several linked, the first stays the match, as before.
func findExactMatchRule(keys exactMatchKeys, entries []exactMatchEntry) (string, *PlaybackState, int) {
	byBase := func(names []string) (string, *PlaybackState) {
		for _, e := range entries {
			if e.state == nil {
				continue
			}
			base := filepath.Base(e.path)
			for _, want := range names {
				if want == base {
					return e.path, e.state
				}
			}
		}
		return "", nil
	}
	if p, s := byBase(keys.basenames); s != nil {
		return p, s, ruleBasename
	}
	if keys.mappedBasenames != nil {
		if p, s := byBase(keys.mappedBasenames()); s != nil {
			return p, s, rulePathMap
		}
	}
	if keys.imdbID != "" {
		var same []exactMatchEntry
		for _, e := range entries {
			if e.state == nil {
				continue
			}
			if imdb := e.state.GetImdbID(); imdb != "" && imdb == keys.imdbID {
				same = append(same, e)
			}
		}
		if len(same) > 1 {
			if e, ok := keys.linked.pickEntry(same); ok {
				return e.path, e.state, ruleLinked
			}
		}
		if len(same) > 0 {
			return same[0].path, same[0].state, ruleIMDB
		}
	}
	return "", nil, ruleNone
}

// pathMapStubNames resolves the media server's own file paths through plex_path_map
// (config.PlexPathMap): the longest prefix that matches a path decides, the mapped
// path is checked with one Lstat, and when it is a symlink the base name of its
// target is the stub name. A missing path or a regular file yields nothing. At most one
// Lstat and one Readlink per path, local calls on a read-only bind, no retries.
func pathMapStubNames(files []string, pathMap map[string]string) []string {
	var names []string
	logged := false
	for _, f := range files {
		if !filepath.IsAbs(f) {
			continue // the media server reports absolute paths; a relative one could climb out
		}
		f = filepath.Clean(f)
		best, mapped, root := "", "", ""
		for p, target := range pathMap { // prefixes are cleaned at load: no trailing slash
			if len(p) < len(best) || (p != "/" && f != p && !strings.HasPrefix(f, p+"/")) {
				continue
			}
			best, mapped, root = p, filepath.Join(target, strings.TrimPrefix(f, p)), target
		}
		if mapped == "" || (mapped != root && !strings.HasPrefix(mapped, strings.TrimSuffix(root, "/")+"/")) {
			continue // never look outside the mapped folder
		}
		fi, err := os.Lstat(mapped)
		var dest string
		if err == nil && fi.Mode()&os.ModeSymlink != 0 {
			dest, err = os.Readlink(mapped)
		} else if err == nil {
			continue // a regular file: not a library symlink
		}
		if err != nil {
			if !logged { // once per webhook: a wrong container path shows up here
				logged = true
				logger.Info(fmt.Sprintf("[PLEX] plex_path_map lookup of %q failed: %v", mapped, err), "mapped", mapped, "err", err)
			}
			continue
		}
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(filepath.Dir(mapped), dest)
		}
		if b := filepath.Base(dest); b != "" && b != "." && b != "/" {
			names = append(names, b)
		}
	}
	return names
}

// lazyPathMapStubNames is pathMapStubNames asked at most once, and only if needed: the
// result is shared by the match and the played mark of one webhook. It returns nil when
// plex_path_map is empty.
func lazyPathMapStubNames(files []string) func() []string {
	var pathMap map[string]string
	if c := gc(); c != nil {
		pathMap = c.PlexPathMap
	}
	if len(pathMap) == 0 || len(files) == 0 {
		return nil
	}
	// The webhook is unauthenticated when no api_token is set: bound the syscalls one
	// post can cause.
	const pathMapMaxFiles = 8
	if len(files) > pathMapMaxFiles {
		files = files[:pathMapMaxFiles]
	}
	var once sync.Once
	var names []string
	return func() []string {
		once.Do(func() { names = pathMapStubNames(files, pathMap) })
		return names
	}
}

// snapshotPlaybackEntries copies the registry into a slice: both pass-1 scans within
// one call see the same entries and order, independent of concurrent registry writes.
func snapshotPlaybackEntries() []exactMatchEntry {
	entries := make([]exactMatchEntry, 0, 16)
	app.playbackRegistry.Range(func(key, value interface{}) bool {
		entries = append(entries, exactMatchEntry{path: key.(string), state: value.(*PlaybackState)})
		return true
	})
	return entries
}

// anyLiveHandleFor reports whether a FUSE handle for path is still registered. It reads
// activeHandles (the live source of truth) instead of globalOpenTracker on purpose: the
// tracker is not decremented when media.stop deletes the pump before Release, so it can
// latch a stale count and would block stops forever.
func anyLiveHandleFor(path string) bool {
	found := false
	app.activeHandles.Range(func(key, _ interface{}) bool {
		if key.(*MkvHandle).path == path {
			found = true
			return false
		}
		return true
	})
	return found
}

// anyLiveHandleForHashExcept is anyLiveHandleForHash excluding one path: the file being
// stopped still has its own handle open at webhook time and must not count as a sibling.
func anyLiveHandleForHashExcept(hash, excludePath string) bool {
	if hash == "" {
		return false
	}
	found := false
	app.activeHandles.Range(func(key, _ interface{}) bool {
		h := key.(*MkvHandle)
		if h.path == excludePath {
			return true
		}
		h.mu.Lock()
		hashOf := h.hash
		h.mu.Unlock()
		if hashOf == hash {
			found = true
			return false
		}
		return true
	})
	return found
}

// shouldDropTorrentPriority reports whether a media.stop may deactivate priority for
// the torrent: never while a file other than the one being stopped still has an open
// handle on the same hash (season-pack episodes share it, so priority-off would hit
// the live stream).
func shouldDropTorrentPriority(hash, excludePath string) bool {
	return hash != "" && !anyLiveHandleForHashExcept(hash, excludePath)
}

// V750: Persist a PlaybackState to SQLite (best-effort)
func savePlaybackStateToDB(ps *PlaybackState) {
	if stateDB == nil {
		return
	}
	ps.mu.RLock()
	rec := &metadb.PlaybackRecord{
		Path:        ps.Path,
		Hash:        ps.Hash,
		ImdbID:      ps.ImdbID,
		OpenedAt:    ps.OpenedAt,
		ConfirmedAt: ps.ConfirmedAt,
		IsHealthy:   ps.IsHealthy,
		IsStopped:   ps.IsStopped,
		LastReadAt:  ps.LastReadAt,
		ReadCount:   ps.ReadCount,
		LastSeekOff: ps.LastSeekOff,
	}
	ps.mu.RUnlock()
	if err := stateDB.SavePlaybackState(rec); err != nil {
		logger.Info(fmt.Sprintf("[V750] Failed to persist playback state for %s: %v", filepath.Base(rec.Path), err), "file", filepath.Base(rec.Path), "err", err)
	}
}

// V750: Restore PlaybackState from SQLite at boot
func restorePlaybackStates(db *metadb.DB) {
	records, err := db.LoadPlaybackStates(4 * time.Hour)
	if err != nil {
		logger.Info(fmt.Sprintf("[V750] Failed to load playback states: %v", err), "err", err)
		return
	}
	restored := 0
	priorityApplied := 0
	for _, rec := range records {
		if rec.IsHealthy && !rec.ConfirmedAt.IsZero() {
			ps := &PlaybackState{
				Path:        rec.Path,
				Hash:        rec.Hash,
				ImdbID:      rec.ImdbID,
				OpenedAt:    rec.OpenedAt,
				ConfirmedAt: rec.ConfirmedAt,
				IsHealthy:   true,
				ReadCount:   rec.ReadCount,
				LastSeekOff: rec.LastSeekOff,
				LastReadAt:  rec.LastReadAt,
			}
			app.playbackRegistry.Store(rec.Path, ps)
			// Restore GoStorm priority
			if rec.Hash != "" {
				hHash := metainfo.NewHashFromHex(rec.Hash)
				if t := web.BTS.GetTorrent(hHash); t != nil {
					t.IsPriority.Store(true)
					t.SetAggressiveMode(true, GetEffectiveConcurrencyLimit())
					priorityApplied++
					logger.Info(fmt.Sprintf("[V750] Priority RESTORED from DB: %s", filepath.Base(rec.Path)), "file", filepath.Base(rec.Path))
				}
			}
			restored++
		}
	}
	logger.Info(fmt.Sprintf("[V750] Restored %d/%d PlaybackStates, %d priorities reapplied", restored, len(records), priorityApplied), "restored", restored, "priority_applied", priorityApplied)
}
