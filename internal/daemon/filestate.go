package daemon

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"flodni/internal/library"
	"flodni/internal/metadb"
	"flodni/internal/warmup"
)

// defaultPlayedProtectionDays is how long a confirmed play keeps its file out of the
// first rounds of quota eviction.
const defaultPlayedProtectionDays = 90

// fileStateStore is the part of the state database the index writes to.
type fileStateStore interface {
	MarkFilePlayed(hash string, fileIdx int, at time.Time) error
	MarkFilesEvicted(keys []metadb.FileKey, at time.Time) error
	ClearFileEvicted(hash string, fileIdx int) error
	DeleteFileStates(hash string) error
	LoadFileStates() ([]metadb.FileState, error)
	SaveFileWarm(w metadb.FileWarm) error
}

type fileStateEntry struct {
	lastPlayed time.Time // zero = never played
	evicted    bool      // eviction mark, cleared by a cold play
	evictedAt  int64     // Unix time of the last eviction, kept for the warmer's EvictedSince
	warm       fileWarm  // the per-file warm record, see filewarm.go
}

// fileStateIndex is the in-memory view of what the cache remembers per media file:
// when a playback confirmed by the media server last played it, and whether quota
// eviction removed its windows since. Eviction reads it under the cache's quota lock,
// so every read here is memory only; writes to the store happen after the mutex is
// released. With no store the marks live in memory and are gone at restart.
type fileStateIndex struct {
	mu      sync.Mutex
	store   fileStateStore
	protect time.Duration // 0 = a play protects forever
	now     func() time.Time
	logf    func(format string, v ...interface{})
	m       map[warmup.FileKey]*fileStateEntry

	// warmMu orders the store writes of the warm record and of eviction marks: it is
	// held from computing a record to writing it, so two writers cannot reach the store
	// in the opposite order of the memory update. Taken before mu, never inside it.
	warmMu sync.Mutex
	// pending and wake feed the recorder (filewarm.go); both guarded by mu.
	pending     map[warmup.FileKey]int64
	wake        chan struct{}
	deliveredAt func(k warmup.FileKey, fileSize int64) time.Time
	// playLatch is set once any play is recorded, here or in the store. Warm on add
	// evicts only with that evidence: without it every file ranks as never played.
	playLatch bool
}

func newFileStateIndex(store fileStateStore, protectionDays int) *fileStateIndex {
	return &fileStateIndex{
		store:   store,
		protect: time.Duration(protectionDays) * 24 * time.Hour,
		now:     time.Now,
		logf:    func(string, ...interface{}) {},
		m:       map[warmup.FileKey]*fileStateEntry{},
		pending: map[warmup.FileKey]int64{},
		wake:    make(chan struct{}, 1),
	}
}

func normKey(k warmup.FileKey) warmup.FileKey {
	k.Hash = strings.ToLower(k.Hash)
	return k
}

func (x *fileStateIndex) storeRef() fileStateStore {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.store
}

// attach connects the state database and merges its rows into memory. The index is
// usable before it (marks then live in memory only), because the cache can evict
// before the database has opened.
func (x *fileStateIndex) attach(store fileStateStore) error {
	rows, err := store.LoadFileStates()
	if err != nil {
		return err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.store = store
	for _, r := range rows {
		k := normKey(warmup.FileKey{Hash: r.Hash, FileIdx: r.FileIdx})
		e := x.m[k]
		if e == nil {
			e = &fileStateEntry{}
			x.m[k] = e
		}
		e.evicted = e.evicted || r.EvictedAt > 0
		e.evictedAt = max(e.evictedAt, r.EvictedAt)
		e.warm.merge(r)
		if t := time.Unix(r.LastPlayedAt, 0); r.LastPlayedAt > 0 && t.After(e.lastPlayed) {
			e.lastPlayed = t
		}
		if r.LastPlayedAt > 0 {
			x.playLatch = true
		}
	}
	return nil
}

// setProtectionDays sets how long a play protects its file; 0 means forever.
func (x *fileStateIndex) setProtectionDays(days int) {
	x.mu.Lock()
	x.protect = time.Duration(days) * 24 * time.Hour
	x.mu.Unlock()
}

// IsPlayed reports whether a confirmed play protects the file now. After the
// protection time the file ranks as never played.
func (x *fileStateIndex) IsPlayed(k warmup.FileKey) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	e := x.m[normKey(k)]
	if e == nil || e.lastPlayed.IsZero() {
		return false
	}
	return x.protect == 0 || x.now().Sub(e.lastPlayed) < x.protect
}

// PlayRecorded reports whether any confirmed play is recorded, here or in the store:
// played-file protection has something to protect.
func (x *fileStateIndex) PlayRecorded() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.playLatch
}

// EverPlayed reports whether any confirmed play was ever recorded for the file, expired
// or not.
func (x *fileStateIndex) EverPlayed(k warmup.FileKey) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	e := x.m[normKey(k)]
	return e != nil && !e.lastPlayed.IsZero()
}

// HasEvictedMark reports whether quota eviction removed the file and no play has been
// counted for it since.
func (x *fileStateIndex) HasEvictedMark(k warmup.FileKey) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	e := x.m[normKey(k)]
	return e != nil && e.evicted
}

// WarmPolicy is the eviction policy warm on add may use: EvictNeverPlayed only when the
// state database is attached and some play was recorded (step 1 decision 6). Without
// that evidence "never played" is every file, so it evicts nothing: EvictNone.
func (x *fileStateIndex) WarmPolicy() warmup.EvictPolicy {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.store != nil && x.playLatch {
		return warmup.EvictNeverPlayed
	}
	return warmup.EvictNone
}

// EvictedSince reports whether quota eviction removed the file at or after since. The
// eviction mark alone can be weeks old; the time tells whether it explains a fill that
// just lost its windows. Times are whole seconds.
func (x *fileStateIndex) EvictedSince(k warmup.FileKey, since time.Time) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	e := x.m[normKey(k)]
	return e != nil && e.evictedAt > 0 && e.evictedAt >= since.Unix()
}

// MarkPlayed records a confirmed play.
func (x *fileStateIndex) MarkPlayed(k warmup.FileKey) {
	k = normKey(k)
	now := x.now()
	x.mu.Lock()
	e := x.m[k]
	if e == nil {
		e = &fileStateEntry{}
		x.m[k] = e
	}
	e.lastPlayed = now
	x.playLatch = true
	st := x.store
	x.mu.Unlock()
	if st != nil {
		if err := st.MarkFilePlayed(k.Hash, k.FileIdx, now); err != nil {
			x.logf("WARNING: file state: cannot store the play of %s/%d: %v", k.Hash, k.FileIdx, err)
		}
	}
}

// OnEvicted records a batch of quota evictions. The played time stays: the mark
// outlives the bytes.
func (x *fileStateIndex) OnEvicted(evs []warmup.Eviction) {
	now := x.now()
	keys := make([]metadb.FileKey, 0, len(evs))
	x.warmMu.Lock()
	defer x.warmMu.Unlock()
	x.mu.Lock()
	for _, ev := range evs {
		k := normKey(ev.Key)
		if k.FileIdx < 0 {
			continue // a file name that is not <hash>-<index>
		}
		e := x.m[k]
		if e == nil {
			e = &fileStateEntry{}
			x.m[k] = e
		}
		e.evicted = true
		e.evictedAt = now.Unix()
		e.warm.cache = cacheEvicted // readability stays: an eviction says nothing about the swarm
		keys = append(keys, metadb.FileKey{Hash: k.Hash, FileIdx: k.FileIdx})
	}
	st := x.store
	x.mu.Unlock()
	if st != nil && len(keys) > 0 {
		if err := st.MarkFilesEvicted(keys, now); err != nil {
			x.logf("WARNING: file state: cannot store %d evictions: %v", len(keys), err)
		}
	}
}

// ClearEvicted drops the eviction mark and reports whether there was one, so a cold
// reopen is counted once per eviction.
func (x *fileStateIndex) ClearEvicted(k warmup.FileKey) bool {
	k = normKey(k)
	x.mu.Lock()
	e := x.m[k]
	had := e != nil && e.evicted
	if had {
		e.evicted = false
	}
	st := x.store
	x.mu.Unlock()
	if had && st != nil {
		if err := st.ClearFileEvicted(k.Hash, k.FileIdx); err != nil {
			x.logf("WARNING: file state: cannot clear the eviction of %s/%d: %v", k.Hash, k.FileIdx, err)
		}
	}
	return had
}

// ForgetMemory drops the in-memory entries of a torrent.
func (x *fileStateIndex) ForgetMemory(hash string) {
	hash = strings.ToLower(hash)
	x.mu.Lock()
	for k := range x.m {
		if k.Hash == hash {
			delete(x.m, k)
		}
	}
	for k := range x.pending {
		if k.Hash == hash {
			delete(x.pending, k)
		}
	}
	x.mu.Unlock()
}

// ForgetHash drops a torrent that left the library, in memory and in the store.
func (x *fileStateIndex) ForgetHash(hash string) {
	// After any warm write in flight: its row must not land behind the delete.
	x.warmMu.Lock()
	defer x.warmMu.Unlock()
	x.ForgetMemory(hash)
	if st := x.storeRef(); st != nil {
		if err := st.DeleteFileStates(strings.ToLower(hash)); err != nil {
			x.logf("WARNING: file state: cannot delete the rows of %s: %v", hash, err)
		}
	}
}

// fileStates is the process-wide index. main sets its protection time and logger and
// attaches the state database once it is open. The cache hooks are bound to this
// instance, so nothing but a test may replace it.
var fileStates = newFileStateIndex(nil, defaultPlayedProtectionDays)

// Counters for /metrics/prometheus; they count since the process started.
var (
	playbackStarts       atomic.Int64
	coldReopensPrevPlay  atomic.Int64 // cold reopens of a file that had been played before
	coldReopensNotPlayed atomic.Int64 // cold reopens of a file never played
)

// markColdOpen latches on the path's session that it opened with no ready head on a
// file the quota had evicted. Call it from Open, after the index is resolved.
func markColdOpen(path string, k warmup.FileKey, headReady bool) {
	if headReady || !fileStates.HasEvictedMark(k) {
		return
	}
	if v, ok := sessions.Load(path); ok {
		v.(*TTFFSession).coldEvicted.Store(true)
	}
}

// countConfirmedPlay is called when a webhook moved a path from not playing to
// playing, with the single file the webhook was resolved to (ok false: none). It
// counts the start, a cold reopen when the session opened cold on an evicted file,
// and records the play.
func countConfirmedPlay(path string, k warmup.FileKey, ok bool) {
	playbackStarts.Add(1)
	if !ok {
		logger.Info(fmt.Sprintf("[PLEX] Playback start resolved to no single file: not marked, no cold reopen counted (%s)", filepath.Base(path)), "file", filepath.Base(path))
		return
	}
	if v, found := sessions.Load(path); found && v.(*TTFFSession).coldEvicted.Swap(false) {
		prev := fileStates.EverPlayed(k)
		if fileStates.ClearEvicted(k) {
			if prev {
				coldReopensPrevPlay.Add(1)
			} else {
				coldReopensNotPlayed.Add(1)
			}
		}
	}
	recordPlay(k, playSourceWebhook)
}

// Where a recorded play came from, the label of flodni_played_marks_total.
const (
	playSourceWebhook = iota
	playSourceManual
)

var playSources = [...]string{playSourceWebhook: "webhook", playSourceManual: "manual"}

// playMarks counts the plays recorded since the process started, by source.
var playMarks [len(playSources)]atomic.Int64

// recordPlay is the played mark itself: the one write a media-server webhook and a
// manual confirm share, so a file played either way is protected the same.
func recordPlay(k warmup.FileKey, source int) {
	fileStates.MarkPlayed(k)
	playMarks[source].Add(1)
}

// confirmPlayByHand is library.Config.Confirm (POST /api/library/confirm): it clears the
// file's eviction mark and records the play, so the protection applies. It touches no
// session: it does not confirm one, and a reader-budget hold waits for a webhook as
// before. It counts no playback start (that is the webhook's), only a play mark with
// source "manual".
func confirmPlayByHand(hash string, fileIdx int) {
	k := warmup.FileKey{Hash: strings.ToLower(hash), FileIdx: fileIdx}
	fileStates.ClearEvicted(k)
	recordPlay(k, playSourceManual)
}

// apiTokenSet reports whether the control server enforces any credential (an api_token
// or a tokens file): the start-up value the middleware was built with, not the live
// config, which a POST /api/config can change on an open port without any
// enforcement following.
func apiTokenSet() bool { return app.authOn() }

// libraryFileFacts is library.Config.FileState: the per-file facts GET /api/library/file
// reports, read from the process-wide index and the live sessions.
func libraryFileFacts(hash string, fileIdx int) library.FileFacts {
	return fileStates.facts(warmup.FileKey{Hash: hash, FileIdx: fileIdx})
}

// facts reads one file's record. Memory only; a file the index has never heard of
// reports nothing recorded.
func (x *fileStateIndex) facts(k warmup.FileKey) library.FileFacts {
	k = normKey(k)
	var f library.FileFacts
	x.mu.Lock()
	if e := x.m[k]; e != nil {
		f.Readability, f.Cache = e.warm.readability, e.warm.cache
		f.Queued, f.FailedAttempts = e.warm.queuedAt > 0, e.warm.failedAttempts
		f.Evicted = e.evicted
		if !e.lastPlayed.IsZero() {
			f.PlayedAt = e.lastPlayed
			f.Protected = x.protect == 0 || x.now().Sub(e.lastPlayed) < x.protect
		}
	}
	x.mu.Unlock()
	f.Confirmed = confirmedSessionOn(k)
	return f
}

// confirmedSessionOn reports whether a media-server session is open on the file: a
// webhook confirmed it, no stop came, and a handle on its path is still open. States
// restored at start stay confirmed with no handle, so the handle is what says it is open.
func confirmedSessionOn(k warmup.FileKey) bool {
	found := false
	app.playbackRegistry.Range(func(p, v any) bool {
		ps, ok := v.(*PlaybackState)
		if !ok || !ps.GetStatus() {
			return true
		}
		ps.mu.RLock()
		stopped := ps.IsStopped
		ps.mu.RUnlock()
		path := p.(string)
		t, ok := tailFillTargets.Load(path)
		if stopped || !ok {
			return true
		}
		tt := t.(tailFillTarget)
		if strings.ToLower(tt.hash) == k.Hash && tt.fileID == k.FileIdx && anyLiveHandleFor(path) {
			found = true
			return false
		}
		return true
	})
	return found
}
