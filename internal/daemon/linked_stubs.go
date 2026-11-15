package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// A play webhook from Plex names no file, so it is matched by IMDb id, the library
// type or a fuzzy title, and several stubs can fit: a release an upgrade replaced stays
// as its own stub, and no library symlink points at it any more. The set below is what
// breaks such a tie. A background walker builds it from the library roots (the targets
// of plex_path_map) with lstat and readlink only; the webhook handler only reads the
// published pointer and never touches the library.
const (
	linkedWalkInterval   = 5 * time.Minute        // a full rebuild
	linkedWalkDebounce   = 30 * time.Second       // the least gap between two walks started early
	linkedWalkMaxEntries = 2_000_000              // directory entries one walk may visit
	linkedWalkMaxTime    = 2 * time.Minute        // wall time one walk may take
	linkedSetMaxAge      = 3 * linkedWalkInterval // older than this, the set is not used
)

// stubSet is a set of stub base names.
type stubSet map[string]struct{}

// builtSet is a published set and when its walk finished.
type builtSet struct {
	names stubSet
	at    time.Time
}

// linkedIndex is the set of stub base names some library symlink points at. No set
// means no walk has finished yet, no root is configured, or the set is too old:
// matching is then as it was before this existed.
type linkedIndex struct {
	set   atomic.Pointer[builtSet]
	kick  chan struct{} // capacity 1: requests for an early walk coalesce
	walks atomic.Int64
	warn  sync.Map // path -> struct{}: what a walk already complained about
}

// current is the last published set, nil before the first walk and once it is older
// than linkedSetMaxAge (walks keep failing or hang): matching is then as it was.
func (l *linkedIndex) current() stubSet {
	if p := l.set.Load(); p != nil && time.Since(p.at) <= linkedSetMaxAge {
		return p.names
	}
	return nil
}

// publish makes set the current one, built at at.
func (l *linkedIndex) publish(set stubSet, at time.Time) {
	l.set.Store(&builtSet{names: set, at: at})
}

// request asks the walker for an early walk. It never blocks.
func (l *linkedIndex) request() {
	select {
	case l.kick <- struct{}{}:
	default: // one is already waiting (or no walker was ever built)
	}
}

// snapshot fixes the set one webhook decides with: the match and the played mark
// share it, so they cannot see different sets.
func (l *linkedIndex) snapshot() linkedSnap { return linkedSnap{l: l, set: l.current()} }

// linkedSnap is a linkedIndex read once. The zero value never picks.
type linkedSnap struct {
	l   *linkedIndex
	set stubSet
}

// pickPath is the tie-break: among paths (more than one), the index of the only one
// whose stub name a library symlink points at. It reports false when none or several
// are linked, and while there is no set, and then the caller behaves as it always did.
// A tie also asks for an early walk, so the next webhook sees a fresh set.
func (v linkedSnap) pickPath(paths []string) (int, bool) {
	if v.l == nil || len(paths) < 2 {
		return -1, false
	}
	v.l.request()
	if v.set == nil {
		return -1, false
	}
	idx := -1
	for i, p := range paths {
		if _, ok := v.set[filepath.Base(p)]; !ok {
			continue
		}
		if idx >= 0 {
			return -1, false
		}
		idx = i
	}
	return idx, idx >= 0
}

// pickEntry is pickPath over registry entries.
func (v linkedSnap) pickEntry(es []exactMatchEntry) (exactMatchEntry, bool) {
	paths := make([]string, len(es))
	for i, e := range es {
		paths[i] = e.path
	}
	if i, ok := v.pickPath(paths); ok {
		return es[i], true
	}
	return exactMatchEntry{}, false
}

// linkedRoots are the library roots: the targets plex_path_map declares, once each.
func linkedRoots() []string {
	c := gc()
	if c == nil || len(c.PlexPathMap) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(c.PlexPathMap))
	roots := make([]string, 0, len(c.PlexPathMap))
	for _, target := range c.PlexPathMap {
		if _, dup := seen[target]; !dup {
			seen[target] = struct{}{}
			roots = append(roots, target)
		}
	}
	sort.Strings(roots)
	return roots
}

// walkLimits bound one walk. A zero Deadline means none.
type walkLimits struct {
	MaxEntries int
	Deadline   time.Time
	// Warn is told of a path the walk could not read, which it skips.
	Warn func(path string, err error)
}

var errWalkBound = errors.New("walk bound reached")

// walkLinkedStubs collects the base names the symlinks under roots point at. It uses
// the directory listing, lstat (through the entry type) and readlink only: it never
// follows a link, never opens a file and never touches a link's target, so a dangling
// link still counts and a hung mount behind a link costs nothing. Each root is resolved
// once with EvalSymlinks (the root only, never an entry), so a root that is itself a
// symlink is walked. An unreadable folder inside a root is reported to lim.Warn and
// skipped. It reports false, with no set, when a root itself cannot be resolved or
// read, when it ran past a bound, or when it was stopped: a set missing a root, or cut
// short, would pick the wrong stub.
func walkLinkedStubs(roots []string, stop <-chan struct{}, lim walkLimits) (stubSet, int, bool) {
	set := stubSet{}
	entries := 0
	warn := func(path string, err error) {
		if lim.Warn != nil {
			lim.Warn(path, err)
		}
	}
	for _, root := range roots {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			warn(root, err)
			return nil, entries, false
		}
		rootFailed := false
		err = filepath.WalkDir(resolved, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				warn(path, err)
				if path == resolved {
					rootFailed = true
				}
				return nil // skip an unreadable folder
			}
			entries++
			if entries > lim.MaxEntries {
				return errWalkBound
			}
			if entries%256 == 0 {
				select {
				case <-stop:
					return errWalkBound
				default:
				}
				if !lim.Deadline.IsZero() && time.Now().After(lim.Deadline) {
					return errWalkBound
				}
			}
			if d.Type()&fs.ModeSymlink == 0 {
				return nil
			}
			dest, err := os.Readlink(path)
			if err != nil {
				warn(path, err)
				return nil
			}
			if !filepath.IsAbs(dest) { // against the link's own folder, as plex_path_map does
				dest = filepath.Join(filepath.Dir(path), dest)
			}
			if b := filepath.Base(dest); b != "" && b != "." && b != "/" {
				set[b] = struct{}{}
			}
			return nil
		})
		if err != nil || rootFailed {
			return nil, entries, false
		}
	}
	// stop and the deadline are only sampled every 256 entries: a small tree that
	// finished after either fired is still a whole set, so only a bound is an error.
	select {
	case <-stop:
		return nil, entries, false
	default:
	}
	if !lim.Deadline.IsZero() && time.Now().After(lim.Deadline) {
		return nil, entries, false
	}
	return set, entries, true
}

// runLinkedWalker rebuilds the set every interval and, once, soon after a request from
// a webhook that found several candidates (never sooner than debounce after the last
// walk ended; requests inside that window share one walk). It returns when stop closes.
// A walk that fails (a root missing, a bound hit) keeps the previous set, which the age
// cap in current bounds. With no root configured the set is withdrawn, so a removed
// root stops narrowing matches.
func runLinkedWalker(l *linkedIndex, roots func() []string, stop <-chan struct{}, interval, debounce time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	var lastEnd time.Time
	var timer *time.Timer
	var pending <-chan time.Time
	cancelPending := func() {
		if timer != nil {
			timer.Stop()
			timer, pending = nil, nil
		}
	}
	defer cancelPending()
	loggedStubs, loggedEntries := -1, -1 // what the last Info line said

	walk := func() {
		cancelPending() // this walk answers any request that was waiting
		defer func() { lastEnd = time.Now() }()
		rs := roots()
		if len(rs) == 0 {
			l.set.Store(nil)
			return
		}
		start := time.Now()
		warned := 0
		lim := walkLimits{
			MaxEntries: linkedWalkMaxEntries,
			Deadline:   start.Add(linkedWalkMaxTime),
			Warn: func(path string, err error) {
				warned++
				if _, seen := l.warn.LoadOrStore(path, struct{}{}); !seen {
					logger.Warn(fmt.Sprintf("[PLEX] linked-stub walk skips %q: %v", path, err), "path", path, "err", err)
				}
			},
		}
		set, n, ok := walkLinkedStubs(rs, stop, lim)
		l.walks.Add(1)
		took := time.Since(start).Round(time.Millisecond)
		if !ok {
			select {
			case <-stop:
			default:
				logger.Warn(fmt.Sprintf("[PLEX] linked-stub walk failed after %s and %d entries; keeping the previous set", took, n), "took", took, "entries", n)
			}
			return
		}
		if warned == 0 { // a clean walk: the next problem is news again
			l.warn.Range(func(k, _ any) bool { l.warn.Delete(k); return true })
		}
		l.publish(set, time.Now())
		msg := fmt.Sprintf("[PLEX] linked-stub walk: %d stub names from %d entries in %s", len(set), n, took)
		if len(set) != loggedStubs || n != loggedEntries {
			loggedStubs, loggedEntries = len(set), n
			logger.Info(msg, "stubs", len(set), "entries", n, "took", took)
		} else {
			logger.Debug(msg, "stubs", len(set), "entries", n, "took", took)
		}
	}

	walk()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			walk()
		case <-l.kick:
			if wait := debounce - time.Since(lastEnd); wait > 0 {
				if timer == nil {
					timer = time.NewTimer(wait)
					pending = timer.C
				}
			} else {
				walk()
			}
		case <-pending:
			walk()
		}
	}
}

// episodeAgrees reports whether the file can be what the webhook played. A show's
// fuzzy match is series-wide, so a linked pick needs the file's own season and episode
// (read as the played mark reads them) to equal the webhook's, and both to be set; a
// movie has nothing more to check.
func episodeAgrees(path, section string, season, episode optInt) bool {
	if section != "show" {
		return true
	}
	if !season.ok || !episode.ok {
		return false
	}
	s, e, found := fileSeasonEpisode(filepath.Base(path))
	return found && s == season.v && e == episode.v
}

// sameEpisode keeps the entries episodeAgrees accepts.
func sameEpisode(es []exactMatchEntry, section string, season, episode optInt) []exactMatchEntry {
	var out []exactMatchEntry
	for _, e := range es {
		if episodeAgrees(e.path, section, season, episode) {
			out = append(out, e)
		}
	}
	return out
}
