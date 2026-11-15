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
	linkedWalkInterval   = 5 * time.Minute  // a full rebuild
	linkedWalkDebounce   = 30 * time.Second // the least gap between two walks started early
	linkedWalkMaxEntries = 2_000_000        // directory entries one walk may visit
	linkedWalkMaxTime    = 2 * time.Minute  // wall time one walk may take
)

// stubSet is a set of stub base names.
type stubSet map[string]struct{}

// linkedIndex is the set of stub base names some library symlink points at. The zero
// set pointer means no walk has finished yet, or no root is configured: matching is
// then as it was before this existed.
type linkedIndex struct {
	set   atomic.Pointer[stubSet]
	kick  chan struct{} // capacity 1: requests for an early walk coalesce
	walks atomic.Int64
	warn  sync.Map // path -> struct{}: what a walk already complained about
}

// current is the last published set, nil before the first walk.
func (l *linkedIndex) current() stubSet {
	if p := l.set.Load(); p != nil {
		return *p
	}
	return nil
}

// request asks the walker for an early walk. It never blocks.
func (l *linkedIndex) request() {
	select {
	case l.kick <- struct{}{}:
	default: // one is already waiting (or no walker was ever built)
	}
}

// pickPath is the tie-break: among paths (more than one), the index of the only one
// whose stub name a library symlink points at. It reports false when none or several
// are linked, and while there is no set, and then the caller behaves as it always did.
// A tie also asks for an early walk, so the next webhook sees a fresh set.
func (l *linkedIndex) pickPath(paths []string) (int, bool) {
	if l == nil || len(paths) < 2 {
		return -1, false
	}
	l.request()
	set := l.current()
	if set == nil {
		return -1, false
	}
	idx := -1
	for i, p := range paths {
		if _, ok := set[filepath.Base(p)]; !ok {
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
func (l *linkedIndex) pickEntry(es []exactMatchEntry) (exactMatchEntry, bool) {
	paths := make([]string, len(es))
	for i, e := range es {
		paths[i] = e.path
	}
	if i, ok := l.pickPath(paths); ok {
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
// link still counts and a hung mount behind a link costs nothing. A missing root or an
// unreadable folder is reported to lim.Warn and skipped. It reports false, with no set,
// when it ran past a bound or was stopped: a partial set would pick the wrong stub.
func walkLinkedStubs(roots []string, stop <-chan struct{}, lim walkLimits) (stubSet, int, bool) {
	set := stubSet{}
	entries := 0
	warn := func(path string, err error) {
		if lim.Warn != nil {
			lim.Warn(path, err)
		}
	}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				warn(path, err)
				return nil // skip: a missing root or an unreadable folder
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
		if err != nil {
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
// walk; requests inside that window share one walk). It returns when stop closes. A
// walk that hits a bound keeps the previous set. With no root configured the set is
// withdrawn, so a removed root stops narrowing matches.
func runLinkedWalker(l *linkedIndex, roots func() []string, stop <-chan struct{}, interval, debounce time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	var last time.Time
	var timer *time.Timer
	var pending <-chan time.Time
	cancelPending := func() {
		if timer != nil {
			timer.Stop()
			timer, pending = nil, nil
		}
	}
	defer cancelPending()

	walk := func() {
		cancelPending() // this walk answers any request that was waiting
		last = time.Now()
		rs := roots()
		if len(rs) == 0 {
			l.set.Store(nil)
			return
		}
		lim := walkLimits{
			MaxEntries: linkedWalkMaxEntries,
			Deadline:   last.Add(linkedWalkMaxTime),
			Warn: func(path string, err error) {
				if _, seen := l.warn.LoadOrStore(path, struct{}{}); !seen {
					logger.Warn(fmt.Sprintf("[PLEX] linked-stub walk skips %q: %v", path, err), "path", path, "err", err)
				}
			},
		}
		set, n, ok := walkLinkedStubs(rs, stop, lim)
		l.walks.Add(1)
		took := time.Since(last).Round(time.Millisecond)
		if !ok {
			select {
			case <-stop:
			default:
				logger.Warn(fmt.Sprintf("[PLEX] linked-stub walk gave up after %s and %d entries; keeping the previous set", took, n), "took", took, "entries", n)
			}
			return
		}
		l.set.Store(&set)
		logger.Info(fmt.Sprintf("[PLEX] linked-stub walk: %d stub names from %d entries in %s", len(set), n, took), "stubs", len(set), "entries", n, "took", took)
	}

	walk()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			walk()
		case <-l.kick:
			if wait := debounce - time.Since(last); wait > 0 {
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
