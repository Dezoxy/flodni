package daemon

import (
	"bytes"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"flodni/internal/warmup"
)

// optInt reads a JSON number, or a numeric string, and otherwise stays unset. It never
// fails: a media server template that sends an unexpected type must not make the
// whole webhook payload unparseable.
type optInt struct {
	v  int
	ok bool
}

func (o *optInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(bytes.TrimSpace(b)), `"`)
	if n, err := strconv.Atoi(s); err == nil {
		o.v, o.ok = n, true
	}
	return nil
}

// playedKeys is what a play webhook says about the file, already normalised.
type playedKeys struct {
	basenames []string
	// mappedBasenames is asked only when no basename matched a candidate: the stub
	// names a library symlink points to (plex_path_map). It decides the same way the
	// media server's own file name does. Nil means off.
	mappedBasenames func() []string
	imdbID          string
	section         string // "movie" or "show"
	title           string // the movie title
	seriesTitle     string // the show title
	year            int
	season          optInt
	episode         optInt
	// linked settles a tie between several candidates by the one a library symlink
	// points at (linked_stubs.go). Nil means off.
	linked *linkedIndex
}

// playedCandidate is a file the mount has opened: only files with a resolved hash and
// index can be marked.
type playedCandidate struct {
	path   string
	imdbID string
	key    warmup.FileKey
}

var (
	reSeasonEpisode = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])s(\d{1,2})[ ._-]?e(\d{1,3})(?:[^0-9]|$)`)
	reNxNN          = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(\d{1,2})x(\d{2,3})(?:[^0-9]|$)`)
)

func nameTokens(s string) []string {
	return strings.Fields(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return ' '
	}, s))
}

// hasTokens reports whether want occurs in have as a contiguous run of whole tokens,
// so "alien" does not match "Aliens".
func hasTokens(have, want []string) bool {
	if len(want) == 0 {
		return false
	}
	for i := 0; i+len(want) <= len(have); i++ {
		match := true
		for j, w := range want {
			if have[i+j] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// fileSeasonEpisode reads SxxEyy (or NxNN) from a file name.
func fileSeasonEpisode(name string) (season, episode int, ok bool) {
	for _, re := range []*regexp.Regexp{reSeasonEpisode, reNxNN} {
		if m := re.FindStringSubmatch(name); m != nil {
			s, _ := strconv.Atoi(m[1])
			e, _ := strconv.Atoi(m[2])
			return s, e, true
		}
	}
	return 0, 0, false
}

// choosePlayedFile decides which single file a play webhook marks as played. It marks
// at most one: when several candidates are equally good, or the evidence is only a
// title that spans a season, it marks none. A missed play costs one file its
// protection; a wrong mark would protect a whole season. It does not affect which
// file the webhook starts playback for.
//
// Tiers, in order:
//  1. the file name the media server reports, or, when none matches, the stub name a
//     library symlink points to (plex_path_map); when any candidate matches, this tier
//     decides (one match marks it, several mark none);
//  2. the IMDB id, only when exactly one candidate carries it; an episode must also
//     agree on season and episode, read from its file name. Otherwise the name tier
//     decides;
//  3. the name: a movie by title (and year, to tell two apart), an episode by series
//     title AND season AND episode, all read from the file name.
//
// In tiers 2 and 3, several equal candidates are settled by the one a library symlink
// points at (linked_stubs.go) when exactly one is; otherwise none is marked, as before.
func choosePlayedFile(k playedKeys, cands []playedCandidate) (playedCandidate, bool) {
	var none playedCandidate
	only := func(m []playedCandidate) (playedCandidate, bool) {
		if len(m) == 1 {
			return m[0], true
		}
		return none, false
	}

	// onlyOrLinked is only, except that several candidates are settled by the one a
	// library symlink points at, when exactly one is.
	onlyOrLinked := func(m []playedCandidate) (playedCandidate, bool) {
		if len(m) > 1 {
			paths := make([]string, len(m))
			for i, c := range m {
				paths[i] = c.path
			}
			if i, ok := k.linked.pickPath(paths); ok {
				return m[i], true
			}
		}
		return only(m)
	}

	named := func(names []string) []playedCandidate {
		var m []playedCandidate
		for _, c := range cands {
			for _, b := range names {
				if filepath.Base(c.path) == b {
					m = append(m, c)
					break
				}
			}
		}
		return m
	}
	if byName := named(k.basenames); len(byName) > 0 {
		return only(byName)
	}
	if k.mappedBasenames != nil {
		if byName := named(k.mappedBasenames()); len(byName) > 0 {
			return only(byName)
		}
	}

	if k.imdbID != "" {
		var byID []playedCandidate
		for _, c := range cands {
			if c.imdbID == k.imdbID {
				byID = append(byID, c)
			}
		}
		if c, ok := onlyOrLinked(byID); ok {
			if k.section != "show" {
				return c, true
			}
			// An id can be series-wide, or cached on the wrong episode by a fuzzy
			// match: an episode needs its season and episode to agree as well.
			if s, e, found := fileSeasonEpisode(filepath.Base(c.path)); found &&
				k.season.ok && k.episode.ok && s == k.season.v && e == k.episode.v {
				return c, true
			}
		}
		// No single agreeing file: the name tier decides.
	}

	switch k.section {
	case "movie":
		want := nameTokens(k.title)
		var m []playedCandidate
		for _, c := range cands {
			base := filepath.Base(c.path)
			if _, _, isEpisode := fileSeasonEpisode(base); isEpisode {
				continue // a movie title that is also a series name
			}
			if hasTokens(nameTokens(base), want) {
				m = append(m, c)
			}
		}
		if len(m) > 1 && k.year > 0 {
			year := []string{strconv.Itoa(k.year)}
			var narrowed []playedCandidate
			for _, c := range m {
				if hasTokens(nameTokens(filepath.Base(c.path)), year) {
					narrowed = append(narrowed, c)
				}
			}
			m = narrowed
		}
		return onlyOrLinked(m)
	case "show":
		if !k.season.ok || !k.episode.ok {
			return none, false
		}
		want := nameTokens(k.seriesTitle)
		var m []playedCandidate
		for _, c := range cands {
			base := filepath.Base(c.path)
			s, e, found := fileSeasonEpisode(base)
			if found && s == k.season.v && e == k.episode.v && hasTokens(nameTokens(base), want) {
				m = append(m, c)
			}
		}
		return onlyOrLinked(m)
	}
	return none, false
}

// playedCandidates lists the registry's files that have a resolved hash and index.
func playedCandidates() []playedCandidate {
	var out []playedCandidate
	for _, e := range snapshotPlaybackEntries() {
		v, ok := tailFillTargets.Load(e.path)
		if !ok || e.state == nil {
			continue
		}
		t := v.(tailFillTarget)
		out = append(out, playedCandidate{path: e.path, imdbID: e.state.GetImdbID(),
			key: warmup.FileKey{Hash: strings.ToLower(t.hash), FileIdx: t.fileID}})
	}
	return out
}
