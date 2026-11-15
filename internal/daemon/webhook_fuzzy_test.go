package daemon

import (
	"bytes"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"flodni/internal/flog"
)

// The fuzzy pass of a play and of a stop is one matcher. These tests drive both webhooks
// through the same tie shapes and assert only what the match decides. A tie that nothing
// narrows is a map-order pick among its members, so such a case names the set the pick
// comes from, never one member.

const (
	fzTv = "/mnt/flodni/tv/Show (2020)/Season.01/"
	fzMv = "/mnt/flodni/movies/"

	fzE2a = fzTv + "Show_S01E02_aaaaaaaa.mkv"
	fzE2b = fzTv + "Show_S01E02_bbbbbbbb.mkv"
	fzE3  = fzTv + "Show_S01E03_cccccccc.mkv"
)

type fzCase struct {
	name     string
	reg      []string
	linked   []string // stub names a library symlink points at
	section  string
	title    string
	series   string
	year     string // "" for none
	season   string // "" for none
	episode  string
	wantRule int      // the rule a play counts
	want     []string // the match is one of these; empty = no match
}

func (c fzCase) payload(event string) string {
	var meta []string
	meta = append(meta, fmt.Sprintf(`"title":%q`, c.title), fmt.Sprintf(`"librarySectionType":%q`, c.section))
	if c.series != "" {
		meta = append(meta, fmt.Sprintf(`"grandparentTitle":%q`, c.series))
	}
	if c.year != "" {
		meta = append(meta, `"year":`+c.year)
	}
	if c.season != "" {
		meta = append(meta, `"parentIndex":`+c.season)
	}
	if c.episode != "" {
		meta = append(meta, `"index":`+c.episode)
	}
	return `{"event":"` + event + `","Metadata":{` + strings.Join(meta, ",") + `}}`
}

func fzEnv(t *testing.T, c fzCase) map[string]*PlaybackState {
	t.Helper()
	linkedEnv(t, "")
	app.playbackRegistry.Delete(lkA)
	app.playbackRegistry.Delete(lkB)
	out := map[string]*PlaybackState{}
	for _, p := range c.reg {
		st := &PlaybackState{Path: p, OpenedAt: time.Now()}
		app.playbackRegistry.Store(p, st)
		out[p] = st
		p := p
		t.Cleanup(func() { app.playbackRegistry.Delete(p) })
	}
	if len(c.linked) > 0 {
		linkedNames(c.linked...)
	}
	return out
}

func fzTable() []fzCase {
	const (
		heat95a = fzMv + "Heat.1995.1080p.WEB.aaaaaaaa.mkv"
		heat95b = fzMv + "Heat.1995.1080p.WEB.bbbbbbbb.mkv"
		alien   = fzMv + "Alien.1979.1080p.BluRay.mkv"
		aliens  = fzMv + "Aliens.1986.1080p.BluRay.mkv"
		casa    = fzMv + "Casablanca.1942.720p.mkv"
		bb1     = fzTv + "Breaking.Bad.S01E01.mkv"
		bbOther = fzTv + "Breaking.Other.S01E01.mkv"
		pilot   = fzTv + "Other.S01E01.Pilot.mkv"
	)
	return []fzCase{
		{name: "empty registry", section: "movie", title: "Heat", year: "1995", wantRule: ruleNone},
		{name: "nothing fits", reg: []string{heat95a}, section: "movie", title: "Zzz", wantRule: ruleNone},
		{name: "one candidate, level 3 title", reg: []string{heat95a}, section: "movie", title: "Heat",
			wantRule: ruleFuzzy, want: []string{heat95a}},
		{name: "one candidate, level 2 series", reg: []string{fzE2a}, section: "show", title: "Zzz", series: "Show",
			wantRule: ruleFuzzy, want: []string{fzE2a}},
		{name: "one candidate, level 1 first word of the series", reg: []string{bb1}, section: "show", title: "Zzz", series: "Breaking Wonder",
			wantRule: ruleFuzzy, want: []string{bb1}},
		{name: "one candidate, level 1 first word and year", reg: []string{casa}, section: "movie", title: "Casablanca Special", year: "1942",
			wantRule: ruleFuzzy, want: []string{casa}},
		{name: "first word without the year is no match", reg: []string{casa}, section: "movie", title: "Casablanca Special",
			wantRule: ruleNone},
		{name: "first word with another year is no match", reg: []string{casa}, section: "movie", title: "Casablanca Special", year: "1999",
			wantRule: ruleNone},
		{name: "short first word is no match", reg: []string{fzMv + "The.Thing.1982.mkv"}, section: "movie", title: "The Other", year: "1982",
			wantRule: ruleNone},
		{name: "level 3 beats level 2", reg: []string{pilot, fzE2a}, section: "show", title: "Pilot", series: "Show",
			wantRule: ruleFuzzy, want: []string{pilot}},
		{name: "level 2 beats level 1", reg: []string{bb1, bbOther}, section: "show", title: "Zzz", series: "Breaking Bad",
			wantRule: ruleFuzzy, want: []string{bb1}},
		{name: "tie at level 3", reg: []string{heat95a, heat95b}, section: "movie", title: "Heat",
			wantRule: ruleFuzzy, want: []string{heat95a, heat95b}},
		{name: "tie at level 2", reg: []string{fzE2a, fzE3}, section: "show", title: "Zzz", series: "Show",
			wantRule: ruleFuzzy, want: []string{fzE2a, fzE3}},
		{name: "tie at level 1", reg: []string{bb1, bbOther}, section: "show", title: "Zzz", series: "Breaking Wonder",
			wantRule: ruleFuzzy, want: []string{bb1, bbOther}},
		{name: "year narrows a title tie", reg: []string{alien, aliens}, section: "movie", title: "Alien", year: "1979",
			wantRule: ruleFuzzy, want: []string{alien}},
		{name: "a year no tied file carries narrows nothing", reg: []string{alien, aliens}, section: "movie", title: "Alien", year: "2000",
			wantRule: ruleFuzzy, want: []string{alien, aliens}},
		{name: "no year narrows nothing", reg: []string{alien, aliens}, section: "movie", title: "Alien",
			wantRule: ruleFuzzy, want: []string{alien, aliens}},
		{name: "year never adds a candidate", reg: []string{alien, fzMv + "Other.1979.mkv"}, section: "movie", title: "Alien", year: "1979",
			wantRule: ruleFuzzy, want: []string{alien}},
		{name: "show: the linked stub of the episode wins", reg: []string{fzE2a, fzE2b}, linked: []string{filepath.Base(fzE2b)},
			section: "show", title: "Zzz", series: "Show", season: "1", episode: "2",
			wantRule: ruleLinked, want: []string{fzE2b}},
		{name: "show: a link of another episode is ignored", reg: []string{fzE2a, fzE2b, fzE3},
			linked:  []string{filepath.Base(fzE2a), filepath.Base(fzE3)},
			section: "show", title: "Zzz", series: "Show", season: "1", episode: "2",
			wantRule: ruleLinked, want: []string{fzE2a}},
		{name: "show: the only link is another episode", reg: []string{fzE2a, fzE3}, linked: []string{filepath.Base(fzE3)},
			section: "show", title: "Zzz", series: "Show", season: "1", episode: "2",
			wantRule: ruleFuzzy, want: []string{fzE2a, fzE3}},
		{name: "show: two linked stubs of the episode pick nothing", reg: []string{fzE2a, fzE2b},
			linked:  []string{filepath.Base(fzE2a), filepath.Base(fzE2b)},
			section: "show", title: "Zzz", series: "Show", season: "1", episode: "2",
			wantRule: ruleFuzzy, want: []string{fzE2a, fzE2b}},
		{name: "show: no season and episode in the webhook pick nothing", reg: []string{fzE2a, fzE2b}, linked: []string{filepath.Base(fzE2b)},
			section: "show", title: "Zzz", series: "Show",
			wantRule: ruleFuzzy, want: []string{fzE2a, fzE2b}},
		{name: "show: another episode in the webhook picks nothing", reg: []string{fzE2a, fzE2b}, linked: []string{filepath.Base(fzE2b)},
			section: "show", title: "Zzz", series: "Show", season: "1", episode: "5",
			wantRule: ruleFuzzy, want: []string{fzE2a, fzE2b}},
		{name: "movie: the linked copy of the year wins", reg: []string{heat95a, heat95b}, linked: []string{filepath.Base(heat95b)},
			section: "movie", title: "Heat", year: "1995",
			wantRule: ruleLinked, want: []string{heat95b}},
		{name: "movie: no year in the webhook picks nothing", reg: []string{heat95a, heat95b}, linked: []string{filepath.Base(heat95b)},
			section: "movie", title: "Heat",
			wantRule: ruleFuzzy, want: []string{heat95a, heat95b}},
		{name: "movie: another year in the webhook picks nothing", reg: []string{heat95a, heat95b}, linked: []string{filepath.Base(heat95b)},
			section: "movie", title: "Heat", year: "1996",
			wantRule: ruleFuzzy, want: []string{heat95a, heat95b}},
		{name: "movie: the link of a title substring is not picked", reg: []string{alien, aliens}, linked: []string{filepath.Base(aliens)},
			section: "movie", title: "Alien", year: "1979",
			wantRule: ruleFuzzy, want: []string{alien}},
		{name: "one candidate is not narrowed by a link elsewhere", reg: []string{heat95a}, linked: []string{"x.mkv"},
			section: "movie", title: "Heat", year: "1995",
			wantRule: ruleFuzzy, want: []string{heat95a}},
	}
}

func fzIn(set []string, p string) bool {
	for _, s := range set {
		if s == p {
			return true
		}
	}
	return false
}

func TestFuzzyPlayTies(t *testing.T) {
	for _, c := range fzTable() {
		t.Run(c.name, func(t *testing.T) {
			m := fzEnv(t, c)
			if got := playRule(t, c.payload("media.play")); got != c.wantRule {
				t.Fatalf("rule = %d, want %d", got, c.wantRule)
			}
			confirmed := 0
			for p, st := range m {
				if !st.GetStatus() {
					continue
				}
				confirmed++
				if !fzIn(c.want, p) {
					t.Fatalf("confirmed %s, want one of %v", filepath.Base(p), c.want)
				}
			}
			if wantN := min(len(c.want), 1); confirmed != wantN {
				t.Fatalf("%d files confirmed, want %d", confirmed, wantN)
			}
		})
	}
}

// A stop with no handle open applies to any fuzzy or linked match. With a handle open on
// every file only a linked match, whose file agrees with the webhook, is honoured: a
// fuzzy match is a guess and must not kill a live stream.
func TestFuzzyStopTies(t *testing.T) {
	for _, handles := range []bool{false, true} {
		for _, c := range fzTable() {
			t.Run(fmt.Sprintf("handles=%v/%s", handles, c.name), func(t *testing.T) {
				m := fzEnv(t, c)
				for p, st := range m {
					st.SetHealthy(true)
					if handles {
						openHandle(t, p)
					}
				}
				postWebhook(t, c.payload("media.stop"))
				want := c.want
				if handles && c.wantRule != ruleLinked {
					want = nil
				}
				var released []string
				for p, st := range m {
					if !st.GetStatus() {
						released = append(released, filepath.Base(p))
					}
				}
				sort.Strings(released)
				if len(want) == 0 {
					if len(released) != 0 {
						t.Fatalf("released %v, want nothing", released)
					}
					return
				}
				if len(released) != 1 {
					t.Fatalf("released %v, want exactly one of %v", released, want)
				}
				for p, st := range m {
					if !st.GetStatus() && !fzIn(want, p) {
						t.Fatalf("released %s, want one of %v", filepath.Base(p), want)
					}
				}
			})
		}
	}
}

// The play and the stop of one payload settle on the same candidates: a case with a
// single expected file names that file for both.
func TestFuzzyPlayAndStopAgreeOnASingleWinner(t *testing.T) {
	for _, c := range fzTable() {
		if len(c.want) != 1 {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			m := fzEnv(t, c)
			postWebhook(t, c.payload("media.play"))
			if !m[c.want[0]].GetStatus() {
				t.Fatal("the play did not confirm the winner")
			}
			postWebhook(t, c.payload("media.stop"))
			if len(c.reg) > 1 && c.wantRule != ruleLinked {
				return // handles are not open: the stop applies; nothing further to pin
			}
			if m[c.want[0]].GetStatus() {
				t.Fatal("the stop did not release the winner")
			}
		})
	}
}

// The webhook's IMDb id is cached onto the file only when the match is one the id may be
// trusted on: a file name, a path map, the id itself or the bootstrap, or a linked pick.
// Never after a fuzzy guess: that would pin the id to a file that may be the wrong one.
func TestFuzzyMatchDoesNotCacheTheImdbId(t *testing.T) {
	const id = "tt0000042"
	payload := lkPayload("media.play", id)
	// Two files of the library type with no id: the bootstrap needs exactly one, so the
	// play falls through to the fuzzy pass, which ties them.
	a, b := linkedEnv(t, "")
	if got := playRule(t, payload); got != ruleFuzzy {
		t.Fatalf("rule = %d, want fuzzy (%d)", got, ruleFuzzy)
	}
	if !a.GetStatus() && !b.GetStatus() {
		t.Fatal("the fuzzy match confirmed nothing")
	}
	if a.GetImdbID() != "" || b.GetImdbID() != "" {
		t.Fatalf("a fuzzy match cached the id (a %q, b %q)", a.GetImdbID(), b.GetImdbID())
	}
}

func TestExactMatchesStillCacheTheImdbId(t *testing.T) {
	const id = "tt0000042"
	t.Run("bootstrap", func(t *testing.T) {
		a, b := linkedEnv(t, "")
		app.playbackRegistry.Delete(lkB) // exactly one file of the type with no id
		if got := playRule(t, lkPayload("media.play", id)); got != ruleIMDB {
			t.Fatalf("rule = %d, want imdb (%d)", got, ruleIMDB)
		}
		if a.GetImdbID() != id || b.GetImdbID() != "" {
			t.Fatalf("the bootstrap did not cache the id on its file (a %q)", a.GetImdbID())
		}
	})
	t.Run("linked pick", func(t *testing.T) {
		a, b := linkedEnv(t, "")
		linkedNames(lkB)
		if got := playRule(t, lkPayload("media.play", id)); got != ruleLinked {
			t.Fatalf("rule = %d, want linked (%d)", got, ruleLinked)
		}
		if a.GetImdbID() != "" || b.GetImdbID() != id {
			t.Fatalf("a linked pick did not cache the id on its file (a %q, b %q)", a.GetImdbID(), b.GetImdbID())
		}
	})
	t.Run("basename", func(t *testing.T) {
		a, _ := linkedEnv(t, "")
		payload := `{"event":"media.play","Metadata":{"title":"Zzz","librarySectionType":"show","Guid":[{"id":"imdb://` + id + `"}],"Media":[{"Part":[{"file":"/lib/` + filepath.Base(lkA) + `"}]}]}}`
		if got := playRule(t, payload); got != ruleBasename {
			t.Fatalf("rule = %d, want basename (%d)", got, ruleBasename)
		}
		if a.GetImdbID() != id {
			t.Fatalf("a basename match did not cache the id (%q)", a.GetImdbID())
		}
	})
}

// The registry dump is a DEBUG line: it lists every registered path on each play.
func TestRegistryDumpIsDebugOnly(t *testing.T) {
	prevLevel := flog.Level.Level()
	t.Cleanup(func() { flog.Level.Set(prevLevel) })
	prev := logger
	t.Cleanup(func() { logger = prev })
	for _, debug := range []bool{false, true} {
		flog.SetLevel(map[bool]string{false: "INFO", true: "DEBUG"}[debug])
		var buf bytes.Buffer
		logger = flog.NewText(&buf)
		fzEnv(t, fzCase{reg: []string{fzE2a}})
		postWebhook(t, lkPayload("media.play", ""))
		if got := strings.Contains(buf.String(), "Registered: "+fzE2a); got != debug {
			t.Fatalf("debug=%v: registry line logged = %v\n%s", debug, got, buf.String())
		}
	}
}
