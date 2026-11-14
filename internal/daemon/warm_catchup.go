package daemon

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"flodni/internal/library"
	"flodni/internal/promtext"
	"flodni/internal/warmer"
	"flodni/internal/warmup"
)

// Catch-up (Dezoxy fork, Readiness step 3): the wiring of warmer.Feeder to the library.
// A pass lists the stubs, keeps the files that are not warm on disk, not evicted and not
// failing, and feeds them to the warmer slowly, newest first. It never evicts: a full
// cache ends the pass. It needs warm_on_add, warmup_dir and the state database.
// With warm_catchup_scope "retries" (roadmap, step 8) it keeps only the files warm on
// add attempted and did not finish; the cold backlog is left alone.

const (
	// catchupRetryWait is how long a file or its hash is left alone after an attempt.
	catchupRetryWait = 20 * time.Hour
	// catchupMaxFailed is the failed attempts (the first plus three retries) after
	// which a file is given up on; only a read through the mount that the swarm
	// serves, or an add again, makes it eligible.
	catchupMaxFailed = 4
	// catchupInterval is the time between the end of a pass and the start of the next.
	catchupInterval = 6 * time.Hour
	// catchupQueuedAfter is how long a file may stay queued by warm on add, with no
	// outcome recorded, before the scope "retries" counts it an unfinished attempt.
	catchupQueuedAfter = time.Hour
)

// Scopes of warm_catchup_scope.
const (
	scopeAll     = "all"
	scopeRetries = "retries"
)

// States of a file, as the predicate says. stateWarm is not a backlog and has no
// gauge series; the others are the states of flodni_warm_catchup_files.
const (
	stateWarm       = "warm"
	statePending    = "pending"
	stateRetryWait  = "retry_wait"
	stateGaveUp     = "gave_up"
	stateEvicted    = "evicted"
	stateNotStored  = "not_stored"
	stateOutOfScope = "out_of_scope" // gauge and predicate: not a retry, and the scope is "retries"
	stateLeftNoRoom = "left_no_room" // gauge only: files a pass left because the cache was full
)

var catchupGaugeStates = []string{statePending, stateRetryWait, stateGaveUp, stateEvicted, stateNotStored, stateOutOfScope, stateLeftNoRoom}

// catchupFile is what the predicate reads about one file.
type catchupFile struct {
	complete           bool     // both windows complete on disk now
	warm               fileWarm // the per-file record, zero when none
	evicted            bool     // the eviction mark or the time of an eviction is set
	hashFailedRecently bool     // some file of the same hash failed under 20 h ago
	packGaveUp         bool     // another file of the hash gave up on swarm reads and no delivery since
	notStored          bool     // WakeStored refused the hash in this process
	retriesOnly        bool     // warm_catchup_scope is "retries"
}

// unfinished reports a file warm on add attempted and did not finish: a failed read, a
// cache state that says the windows were not kept, or a queued mark that no outcome
// cleared within catchupQueuedAfter (a restart mid-warm, a wake error, a full queue).
// A file never attempted, or only cached, is the cold backlog and is none of these.
func unfinished(w fileWarm, now time.Time) bool {
	switch {
	case w.readability == readFailed:
		return true
	case w.cache == cacheDroppedQuota, w.cache == cacheDroppedNoEvidence, w.cache == cacheLost, w.cache == cacheWriteFailed:
		return true
	}
	return w.queuedAt > 0 && now.Sub(time.Unix(w.queuedAt, 0)) > catchupQueuedAfter
}

// failedRecently reports a failed record whose last attempt is under 20 h old.
func failedRecently(w fileWarm, now time.Time) bool {
	return w.readability == readFailed && w.lastAttemptAt > 0 &&
		now.Sub(time.Unix(w.lastAttemptAt, 0)) < catchupRetryWait
}

// packGaveUp reports a torrent whose swarm is dead for catch-up: some file gave up
// through failed swarm reads (readFailed, 4 attempts; the cache-side outcomes lost and
// write_failed never set readFailed) and no file of the hash has had a swarm delivery
// since that file's last attempt. A delivery on any file, the given-up one included,
// lifts it. The siblings are then given up too, so a dead pack is not probed file by
// file for days. Warm on add of a new file does not read this: an add still tries once.
func packGaveUp(ws []fileWarm) bool {
	var gaveUpAt int64
	gave := false
	for _, w := range ws {
		if w.readability == readFailed && w.failedAttempts >= catchupMaxFailed {
			gave = true
			gaveUpAt = max(gaveUpAt, w.lastAttemptAt)
		}
	}
	if !gave {
		return false
	}
	for _, w := range ws {
		if w.lastDeliveryAt > gaveUpAt {
			return false
		}
	}
	return true
}

// catchupState is the predicate, in the order of the list under "Catch-up" in the README.
// It is pure: the pass reads the facts and acts on the answer.
func catchupState(f catchupFile, now time.Time) string {
	switch {
	case f.complete:
		return stateWarm // whatever the record says: the disk is the truth
	case f.evicted || f.warm.cache == cacheEvicted:
		return stateEvicted
	case f.retriesOnly && !unfinished(f.warm, now):
		return stateOutOfScope // before the waits: the cold backlog is not waiting, it is not ours
	case f.warm.readability == readFailed && f.warm.failedAttempts >= catchupMaxFailed:
		return stateGaveUp
	case f.packGaveUp:
		return stateGaveUp // a sibling gave up on the swarm: the pack is dead until a delivery
	case f.warm.lastAttemptAt > 0 && now.Sub(time.Unix(f.warm.lastAttemptAt, 0)) < catchupRetryWait:
		return stateRetryWait
	case f.hashFailedRecently:
		// A no-peers failure skips the rest of a pack without a record of its own, so
		// the siblings wait too: a dead pack is not probed again every pass.
		return stateRetryWait
	case f.notStored:
		return stateNotStored
	}
	return statePending
}

// CatchupFact is the record of a file and whether quota eviction ever removed it
// (the mark, or the time of an eviction). Memory only.
func (x *fileStateIndex) CatchupFact(k warmup.FileKey) (fileWarm, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	e := x.m[normKey(k)]
	if e == nil {
		return fileWarm{}, false
	}
	return e.warm, e.evicted || e.evictedAt > 0
}

// catchupGauge is flodni_warm_catchup_files: the backlog as of the last pass.
type catchupGauge struct {
	mu    sync.Mutex
	m     map[string]int64
	scope string // the scope of the last pass; "" before the first
}

func (g *catchupGauge) set(m map[string]int64) {
	g.mu.Lock()
	g.m = m
	g.mu.Unlock()
}

func (g *catchupGauge) setScope(s string) {
	g.mu.Lock()
	g.scope = s
	g.mu.Unlock()
}

func (g *catchupGauge) getScope() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.scope
}

func (g *catchupGauge) get() map[string]int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[string]int64{}
	for k, v := range g.m {
		out[k] = v
	}
	return out
}

var catchupFiles = &catchupGauge{}

// collectCatchupProm writes the gauge with every state present.
func collectCatchupProm(w *promtext.Writer, g *catchupGauge) {
	m := g.get()
	s := make([]promtext.Sample, 0, len(catchupGaugeStates))
	for _, st := range catchupGaugeStates {
		s = append(s, promSample(m[st], lbl("state", st)))
	}
	sc := g.getScope()
	w.Gauge("flodni_warm_catchup_scope",
		"The warm_catchup_scope the last catch-up pass ran with: 1 for that scope, 0 for the other; both 0 before the first pass. all = every library file not warm, evicted or failing; retries = only the files warm on add attempted and did not finish.",
		promSample(b2i(sc == scopeAll), lbl("scope", scopeAll)),
		promSample(b2i(sc == scopeRetries), lbl("scope", scopeRetries)))
	w.Gauge("flodni_warm_catchup_files",
		"Library files catch-up has not warmed, as of the last pass, by state: pending (candidates, also those a pass left because it was stopped, the pace was set to 0 or its disk failed), retry_wait (attempted under 20 hours ago, or a file of the same torrent failed), gave_up (4 failed attempts, or a file of the same torrent gave up on the swarm and nothing was delivered since), evicted (quota eviction removed it: left to play), not_stored (the engine refused to wake the torrent), out_of_scope (scope retries only: files warm on add never attempted, left to a play or to scope all) and left_no_room (candidates the last pass left because the cache was full). Already warm files are not counted.",
		s...)
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// catchupSwitchOff is the one requirement that is a choice, not a fault: no warning.
const catchupSwitchOff = "warm_on_add is off"

// catchupRequirement says why catch-up cannot run, or "" when it can. Without the state
// database an eviction mark would not survive a restart and catch-up could warm an
// evicted file again.
func catchupRequirement(x *fileStateIndex) string {
	c := gc()
	switch {
	case c == nil || !c.WarmOnAdd:
		return catchupSwitchOff
	case c.WarmupDir == "":
		return "warmup_dir is not set"
	case x.storeRef() == nil:
		return "the state database is not attached"
	}
	return ""
}

// catchup is the pass loop. Everything it touches is a field, so a test needs no
// engine, library or database.
type catchup struct {
	list     func() ([]library.Item, error)
	complete func(warmup.FileKey) bool
	facts    func(warmup.FileKey) (fileWarm, bool)
	record   func(warmUpdate)
	target   warmer.Target
	perHour  func() int
	scope    func() string // warm_catchup_scope; nil means "all"
	now      func() time.Time
	stop     <-chan struct{}
	poll     time.Duration // the feeder's wait for an idle warmer; 0 means 1 s
	logf     func(format string, v ...interface{})
	gauge    *catchupGauge

	passMu     sync.Mutex // held for a whole pass: the no-room flag is shared
	passScope  string     // the scope the running pass reads once at its start
	warnedOnce sync.Once
}

// newCatchup wires the daemon's catch-up: the warmer, the file index x, the cache and
// the config. list is the library's stub walk.
func newCatchup(w *warmer.Warmer, x *fileStateIndex, list func() ([]library.Item, error)) *catchup {
	return &catchup{
		list:     list,
		complete: func(k warmup.FileKey) bool { ok, _ := warmup.DiskWarmup.WindowsComplete(k); return ok },
		facts:    x.CatchupFact,
		record:   func(u warmUpdate) { x.RecordWarm(u) },
		target:   w,
		perHour: func() int {
			if c := gc(); c != nil {
				return c.WarmCatchupPerHour
			}
			return 0
		},
		scope: func() string {
			if c := gc(); c != nil {
				return c.WarmCatchupScope
			}
			return scopeAll
		},
		now:   time.Now,
		stop:  backgroundStopChan,
		logf:  func(f string, v ...interface{}) { logger.Printf(f, v...) },
		gauge: catchupFiles,
	}
}

// startCatchup lists "movies" and "tv" and runs the loop until the daemon stops.
func startCatchup(m *library.Manager) {
	c := newCatchup(addWarmer, fileStates, func() ([]library.Item, error) {
		movies, err := m.List("movies")
		if err != nil {
			return nil, err
		}
		tv, err := m.List("tv")
		if err != nil {
			return nil, err
		}
		return append(movies, tv...), nil
	})
	go c.loop(func() string { return catchupRequirement(fileStates) }, time.After)
}

// loop waits one interval at the configured pace (3 minutes at 20 an hour), then runs a
// pass, then waits 6 hours from its end, until stop closes. A pace of 0 and a missing
// requirement skip the pass; the pace and the config are read again at the next one.
func (c *catchup) loop(requirement func() string, after func(time.Duration) <-chan time.Time) {
	first := true
	for {
		d := catchupInterval
		if first {
			if ph := c.perHour(); ph > 0 {
				d = time.Hour / time.Duration(ph)
			}
		}
		first = false
		select {
		case <-after(d):
		case <-c.stop:
			return
		}
		select {
		case <-c.stop:
			return
		default:
		}
		if c.perHour() <= 0 {
			continue // off by choice: no warning
		}
		if why := requirement(); why != "" {
			if why != catchupSwitchOff { // off by choice, like a pace of 0
				c.warnedOnce.Do(func() {
					c.logf("WARNING: [WarmCatchup] catch-up needs warmup_dir and the state database, and %s: nothing is caught up", why)
				})
			}
			continue
		}
		c.runPass()
	}
}

// group is the stubs of one torrent.
type group struct {
	hash   string // lower case
	files  map[int]int64
	newest time.Time
}

// groups collects the stubs by hash: newest stub first, files by index.
func groups(items []library.Item) []*group {
	by := map[string]*group{}
	var out []*group
	for _, it := range items {
		if it.Hash == "" || it.FileIndex <= 0 || it.Size <= 0 {
			continue
		}
		h := strings.ToLower(it.Hash)
		g := by[h]
		if g == nil {
			g = &group{hash: h, files: map[int]int64{}}
			by[h] = g
			out = append(out, g)
		}
		g.files[it.FileIndex] = it.Size
		if it.ModTime.After(g.newest) {
			g.newest = it.ModTime
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].newest.Equal(out[j].newest) {
			return out[i].newest.After(out[j].newest)
		}
		return out[i].hash < out[j].hash
	})
	return out
}

// scopeNow is the scope of this pass: "retries" or, for anything else, "all".
func (c *catchup) scopeNow() string {
	if c.scope != nil && c.scope() == scopeRetries {
		return scopeRetries
	}
	return scopeAll
}

// classify applies the predicate to a group. It returns the pending files by index and
// the count of every state. A file that is warm on disk but whose record does not say
// cached gets that record, with no wake.
func (c *catchup) classify(g *group) ([]warmer.File, map[string]int64) {
	now := c.now()
	idx := make([]int, 0, len(g.files))
	for i := range g.files {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	type fact struct {
		w       fileWarm
		evicted bool
	}
	facts := make(map[int]fact, len(idx))
	hashFailed := false
	warms := make([]fileWarm, 0, len(idx))
	for _, i := range idx {
		w, ev := c.facts(warmup.FileKey{Hash: g.hash, FileIdx: i})
		facts[i] = fact{w, ev}
		warms = append(warms, w)
		hashFailed = hashFailed || failedRecently(w, now)
	}
	packDead := packGaveUp(warms)
	notStored := c.target.NotStored(g.hash)
	counts := map[string]int64{}
	var pending []warmer.File
	for _, i := range idx {
		key := warmup.FileKey{Hash: g.hash, FileIdx: i}
		f := facts[i]
		st := catchupState(catchupFile{complete: c.complete(key), warm: f.w, evicted: f.evicted,
			hashFailedRecently: hashFailed, packGaveUp: packDead, notStored: notStored, retriesOnly: c.passScope == scopeRetries}, now)
		switch st {
		case stateWarm:
			if f.w.cache != cacheCached {
				c.record(warmUpdate{key: key, cache: cacheCached})
			}
		case statePending:
			pending = append(pending, warmer.File{Index: i, Size: g.files[i]})
			counts[st]++
		default:
			counts[st]++
		}
	}
	return pending, counts
}

// plan classifies every group: the candidates in feed order and the counts.
func (c *catchup) plan(gs []*group) ([]warmer.Candidate, map[string]int64) {
	var cands []warmer.Candidate
	total := map[string]int64{}
	for _, g := range gs {
		pending, counts := c.classify(g)
		for k, v := range counts {
			total[k] += v
		}
		if len(pending) > 0 {
			cands = append(cands, warmer.Candidate{Hash: g.hash, Files: pending})
		}
	}
	return cands, total
}

// logLine writes one "[WarmCatchup] {json}" line. It carries counts only: no path, no
// title.
func (c *catchup) logLine(v map[string]interface{}) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.logf("[WarmCatchup] %s", b)
}

// runPass runs one pass and reports false when another is in flight, so two passes
// never share the warmer's no-room flag.
func (c *catchup) runPass() bool {
	if !c.passMu.TryLock() {
		return false
	}
	defer c.passMu.Unlock()
	started := time.Now()
	c.passScope = c.scopeNow() // read once: the whole pass runs under one scope
	c.gauge.setScope(c.passScope)
	items, err := c.list()
	if err != nil {
		c.logf("WARNING: [WarmCatchup] cannot list the library: %v", err)
		return true
	}
	gs := groups(items)
	byHash := make(map[string]*group, len(gs))
	for _, g := range gs {
		byHash[g.hash] = g
	}
	cands, counts := c.plan(gs)
	c.gauge.set(counts)
	files := 0
	for _, cd := range cands {
		files += len(cd.Files)
	}
	c.logLine(map[string]interface{}{"event": "pass_start", "scope": c.passScope, "candidate_hashes": len(cands), "pending": files,
		"retry_wait": counts[stateRetryWait], "gave_up": counts[stateGaveUp], "evicted": counts[stateEvicted],
		"not_stored": counts[stateNotStored], "out_of_scope": counts[stateOutOfScope]})

	fd := &warmer.Feeder{
		Target:  c.target,
		PerHour: c.perHour,
		Stop:    c.stop,
		Poll:    c.poll,
		// Fresh facts just before the feed: a file warmed, evicted or attempted since the
		// list was made is not fed.
		Still: func(hash string, f warmer.File) bool {
			g := byHash[hash]
			if g == nil {
				return false
			}
			pending, _ := c.classify(g)
			for _, p := range pending {
				if p.Index == f.Index {
					return true
				}
			}
			return false
		},
	}
	res := fd.Pass(cands)

	// The gauge after the pass: the same classification again, with the files the
	// pass left for lack of room moved out of pending. Files it left stopped, at pace 0
	// or on a failing disk stay in pending; files it fed and that failed show as
	// retry_wait.
	_, after := c.plan(gs)
	noRoom := int64(res.Left[warmer.LeftNoRoom])
	if res.NoRoom {
		// The pass ended on a full cache: the title that found it full (counted
		// no_room and skipped, with no record) is left for room like the rest.
		noRoom = after[statePending]
	}
	after[statePending] -= noRoom
	after[stateLeftNoRoom] = noRoom
	c.gauge.set(after)
	c.logLine(map[string]interface{}{"event": "pass_end", "scope": c.passScope, "fed_hashes": res.Hashes, "fed": res.Fed, "no_room": res.NoRoom,
		"left": res.Left, "pending": after[statePending], "retry_wait": after[stateRetryWait], "gave_up": after[stateGaveUp],
		"evicted": after[stateEvicted], "not_stored": after[stateNotStored], "out_of_scope": after[stateOutOfScope], "seconds": int(time.Since(started).Seconds())})
	return true
}
