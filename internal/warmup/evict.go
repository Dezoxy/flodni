package warmup

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// FileKey names one media file of a torrent: the unit eviction removes, with its head
// and its tail window together.
type FileKey struct {
	Hash    string
	FileIdx int
}

// EvictPolicy says which files one quota check may evict. EvictAny is the zero value
// and what every head creation uses. The other two are for background warming (see
// docs/roadmap.md, Readiness steps 2 and 3): FillWindows (fill.go) passes them, and
// the warmer (internal/warmer) is its only caller: warm on add with EvictNeverPlayed,
// catch-up with EvictNone (its RoomOnly pre-flight evicts nothing).
type EvictPolicy int

const (
	// EvictAny takes files never played first, then played files, oldest first.
	EvictAny EvictPolicy = iota
	// EvictNeverPlayed takes only files never played, and nothing at all when they
	// cannot make enough room.
	EvictNeverPlayed
	// EvictNone never evicts: over the quota it only reports that there is no room.
	EvictNone
)

// TriggerHeadCreate is the trigger of an eviction made to make room for a new head file.
const TriggerHeadCreate = "head_create"

// TriggerRequest is the trigger of an eviction made on request (EvictTo).
const TriggerRequest = "request"

// IsPlayed, if set, reports whether a playback confirmed by the media server protects
// the file now (a play inside the configured protection time). It runs under the
// cache's quota lock and tailMu, so it must read memory only, never block and never call
// a cache writer (it would deadlock). Nil: nothing is played, and eviction degenerates
// to oldest first.
var IsPlayed func(FileKey) bool

// OnEvicted, if set, receives the files one quota check evicted. It runs after the
// cache's locks are released, so it may write to a database.
var OnEvicted func([]Eviction)

// Eviction describes one evicted media file.
type Eviction struct {
	Key       FileKey
	HeadBytes int64 // allocated size of the head window removed, 0 when it had none
	TailBytes int64
	Age       time.Duration // since the newest write of the two windows
	Played    bool          // the file was inside its played protection
	Trigger   string
}

// evictOpts is what one quota check needs to know about its caller.
type evictOpts struct {
	policy  EvictPolicy
	exclude FileKey            // the file being created; never a victim
	protect func(FileKey) bool // files that are never victims, such as those a fill has in flight
	trigger string
	// A request (EvictTo) evicts down to its own size instead of the configured quota,
	// and walks the disk even when the running total looks low.
	request bool
	limit   int64
}

// evictLog writes one JSON line per evicted file. The prefix is stable so a log
// search can find them.
var evictLog = log.New(os.Stdout, "[WarmupEvict] ", log.LstdFlags)

type evictLine struct {
	Hash      string `json:"hash"`
	File      int    `json:"file"`
	HeadBytes int64  `json:"head_bytes"`
	TailBytes int64  `json:"tail_bytes"`
	AgeSec    int64  `json:"age_s"`
	Played    bool   `json:"played"`
	Trigger   string `json:"trigger"`
}

// reportEvictions logs each eviction and hands the batch to OnEvicted. Callers hold
// no cache lock.
func reportEvictions(evs []Eviction) {
	if len(evs) == 0 {
		return
	}
	for _, e := range evs {
		b, err := json.Marshal(evictLine{e.Key.Hash, e.Key.FileIdx, e.HeadBytes, e.TailBytes,
			int64(e.Age.Seconds()), e.Played, e.Trigger})
		if err != nil {
			continue
		}
		evictLog.Print(string(b))
	}
	if OnEvicted != nil {
		OnEvicted(evs)
	}
}

// evictUnit is one media file's windows on disk.
type evictUnit struct {
	key       FileKey
	headPath  string
	tailPath  string
	headBytes int64
	tailBytes int64
	newest    time.Time
	played    bool
}

func (u *evictUnit) size() int64 { return u.headBytes + u.tailBytes }

// splitCacheName parses "<hash>-<idx>" from a cache file name without its suffix.
func splitCacheName(stem string) FileKey {
	i := strings.LastIndexByte(stem, '-')
	if i < 0 {
		return FileKey{Hash: stem, FileIdx: -1}
	}
	idx, err := strconv.Atoi(stem[i+1:])
	if err != nil {
		return FileKey{Hash: stem, FileIdx: -1}
	}
	return FileKey{Hash: stem[:i], FileIdx: idx}
}

// enforceQuotaLocked makes room for needed bytes, and reports the evictions it made and
// whether the room exists afterwards. The caller holds d.mu, and reports the evictions
// with reportEvictions once it has released it.
//
// A victim is a whole media file, head and tail together. Files never played go first,
// then played files, each class oldest first by the newer of its two windows. The file
// in opts.exclude is never a victim. The verdict follows the disk walk, which also
// resyncs totalSize.
// maxEvictFailures is how many units one quota check may fail to remove before it gives
// up: a read-only or broken directory fails them all, and a failed removal no longer
// lowers the total that ends the loop.
const maxEvictFailures = 8

func (d *DiskWarmupCache) enforceQuotaLocked(needed int64, opts evictOpts) (evicted []Eviction, room bool) {
	quota := quotaBytes()
	if opts.request {
		quota = opts.limit
	}

	totalSize := atomic.LoadInt64(&d.totalSize)
	if !opts.request && totalSize+needed <= quota {
		return nil, true
	}

	// The walk stores an absolute total, so no write or removal may change totalSize
	// between the walk and the store: tailMu is held from before the walk to the end,
	// after d.mu as everywhere.
	d.tailMu.Lock()
	defer d.tailMu.Unlock()

	walkStart := time.Now()
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		// A vanished or unreadable directory proves nothing about the total: keep the
		// old one rather than store 0, which would undercount until the next restart.
		logf.Printf("[DiskWarmup] quota walk of %s failed: %v: total kept, nothing evicted", d.dir, err)
		return nil, false
	}
	units := map[string]*evictUnit{}
	var diskTotal int64
	ledger := map[string]int64{}
	for _, e := range entries {
		name := e.Name()
		isTail := strings.HasSuffix(name, tailSuffix)
		if !isTail && !strings.HasSuffix(name, warmupSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		stem := strings.TrimSuffix(strings.TrimSuffix(name, tailSuffix), warmupSuffix)
		u := units[stem]
		if u == nil {
			u = &evictUnit{key: splitCacheName(stem)}
			units[stem] = u
		}
		full := filepath.Join(d.dir, name)
		if isTail {
			u.tailPath, u.tailBytes = full, allocatedBytes(info)
		} else {
			u.headPath, u.headBytes = full, allocatedBytes(info)
		}
		if info.ModTime().After(u.newest) {
			u.newest = info.ModTime()
		}
		diskTotal += allocatedBytes(info)
		ledger[full] = allocatedBytes(info)
	}

	atomic.StoreInt64(&d.totalSize, diskTotal)
	d.ledger = ledger
	// tailMu is held for the whole walk, and a head write waits on it: say when it is slow.
	if dur := time.Since(walkStart); dur > 50*time.Millisecond {
		logf.Printf("[DiskWarmup] quota walk held tailMu for %v over %d files", dur.Round(time.Millisecond), len(ledger))
	}
	if diskTotal+needed <= quota {
		return nil, true
	}
	if opts.policy == EvictNone {
		return nil, false
	}

	var victims []*evictUnit
	var neverPlayedBytes int64
	for _, u := range units {
		if u.key == opts.exclude || (opts.protect != nil && u.key.FileIdx >= 0 && opts.protect(u.key)) {
			continue
		}
		u.played = IsPlayed != nil && u.key.FileIdx >= 0 && IsPlayed(u.key)
		if u.played && opts.policy == EvictNeverPlayed {
			continue
		}
		if !u.played {
			neverPlayedBytes += u.size()
		}
		victims = append(victims, u)
	}
	over := diskTotal + needed - quota
	if opts.policy == EvictNeverPlayed && neverPlayedBytes < over {
		return nil, false
	}
	sort.Slice(victims, func(i, j int) bool {
		a, b := victims[i], victims[j]
		if a.played != b.played {
			return !a.played
		}
		if !a.newest.Equal(b.newest) {
			return a.newest.Before(b.newest)
		}
		return a.key.Hash+"-"+strconv.Itoa(a.key.FileIdx) < b.key.Hash+"-"+strconv.Itoa(b.key.FileIdx)
	})

	now := time.Now()
	d.removals.Add(1)
	failed := 0
	for _, u := range victims {
		if diskTotal+needed <= quota {
			break
		}
		// A failed unlink is not an eviction: its bytes stay counted, and nothing is
		// reported, so the state database does not mark a file that is still there.
		allGone := true
		var headGone, tailGone int64
		for i, p := range []string{u.headPath, u.tailPath} {
			if p == "" {
				continue
			}
			d.closeHandle(p)
			if !removed(p) {
				allGone = false
				continue
			}
			removeFile(p + coverSuffix)
			d.sizeCache.Delete(p)
			d.tailCoverage.Delete(p)
			delete(d.ledger, p)
			if i == 0 {
				headGone = u.headBytes
				evictedHeads.Add(1)
				evictedHeadBytes.Add(u.headBytes)
			} else {
				tailGone = u.tailBytes
				evictedTails.Add(1)
				evictedTailBytes.Add(u.tailBytes)
			}
		}
		diskTotal -= headGone + tailGone
		if headGone+tailGone > 0 {
			// Reported with the bytes actually removed: a head gone with its tail stuck
			// is still an eviction of that head.
			evicted = append(evicted, Eviction{Key: u.key, HeadBytes: headGone, TailBytes: tailGone,
				Age: now.Sub(u.newest), Played: u.played, Trigger: opts.trigger})
		}
		if !allGone {
			if failed++; failed >= maxEvictFailures {
				logf.Printf("[DiskWarmup] eviction stopped: %d units could not be removed", failed)
				break
			}
		}
	}
	atomic.StoreInt64(&d.totalSize, diskTotal)
	return evicted, diskTotal+needed <= quota
}

// EvictResult is what one EvictTo call removed.
type EvictResult struct {
	Files      int
	Bytes      int64
	TotalAfter int64
}

// EvictTo evicts down to target bytes now, by the same victim selection as the quota
// check (never-played files first, then played ones, each oldest first), and reports
// each eviction as a quota eviction with the trigger TriggerRequest. It deletes cache
// files only, inside the cache directory. A target at or above the current total
// evicts nothing. Safe on a nil receiver, where it does nothing.
func (d *DiskWarmupCache) EvictTo(target int64) EvictResult {
	if d == nil {
		return EvictResult{}
	}
	d.mu.Lock()
	evicted, _ := d.enforceQuotaLocked(0, evictOpts{trigger: TriggerRequest, request: true, limit: target})
	d.mu.Unlock()
	reportEvictions(evicted) // after the locks: it may write to a database
	res := EvictResult{Files: len(evicted), TotalAfter: d.TrackedBytes()}
	for _, e := range evicted {
		res.Bytes += e.HeadBytes + e.TailBytes
	}
	return res
}
