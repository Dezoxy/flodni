package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"flodni/internal/config"
	"flodni/internal/warmup"
)

const (
	lkA = "/mnt/flodni/tv/Show (2020)/Season.01/Show_S01E02_aaaaaaaa.mkv"
	lkB = "/mnt/flodni/tv/Show (2020)/Season.01/Show_S01E02_bbbbbbbb.mkv"
	lkI = "tt0000009"
)

var (
	lkKeyA = warmup.FileKey{Hash: "aaaaaaaa00", FileIdx: 2}
	lkKeyB = warmup.FileKey{Hash: "bbbbbbbb00", FileIdx: 2}
)

// linkedEnv registers two stubs of one episode, the way an upgrade leaves them. imdb is
// the id both carry ("" for none).
func linkedEnv(t *testing.T, imdb string) (a, b *PlaybackState) {
	t.Helper()
	newTestApp(t)
	oldStates, prevCfg := fileStates, globalConfig.Load()
	fileStates = newFileStateIndex(nil, 90)
	globalConfig.Store(&config.Config{})
	a = &PlaybackState{Path: lkA, OpenedAt: time.Now(), ImdbID: imdb}
	b = &PlaybackState{Path: lkB, OpenedAt: time.Now(), ImdbID: imdb}
	app.playbackRegistry.Store(lkA, a)
	app.playbackRegistry.Store(lkB, b)
	tailFillTargets.Store(lkA, tailFillTarget{hash: lkKeyA.Hash, fileID: lkKeyA.FileIdx, size: 1 << 30})
	tailFillTargets.Store(lkB, tailFillTarget{hash: lkKeyB.Hash, fileID: lkKeyB.FileIdx, size: 1 << 30})
	t.Cleanup(func() {
		globalConfig.Store(prevCfg)
		fileStates = oldStates
		for _, p := range []string{lkA, lkB} {
			app.playbackRegistry.Delete(p)
			tailFillTargets.Delete(p)
		}
	})
	return a, b
}

// linkedNames publishes the set a finished walk would, for these stub paths.
func linkedNames(paths ...string) {
	s := stubSet{}
	for _, p := range paths {
		s[filepath.Base(p)] = struct{}{}
	}
	app.linked.publish(s, time.Now())
}

// lkPayload is what Plex sends: no file, an IMDb id (when given), a title.
func lkPayload(event, imdb string) string {
	guid := ""
	if imdb != "" {
		guid = `,"Guid":[{"id":"imdb://` + imdb + `"}]`
	}
	return `{"event":"` + event + `","Metadata":{"title":"Pilot","grandparentTitle":"Show","librarySectionType":"show","parentIndex":1,"index":2` + guid + `}}`
}

func TestLinkedStubWinsAnImdbTie(t *testing.T) {
	for _, linked := range []string{lkA, lkB} {
		a, b := linkedEnv(t, lkI)
		linkedNames(linked)
		if got := playRule(t, lkPayload("media.play", lkI)); got != ruleLinked {
			t.Fatalf("linked %s: rule = %d, want linked (%d)", filepath.Base(linked), got, ruleLinked)
		}
		want, other, wantKey, otherKey := a, b, lkKeyA, lkKeyB
		if linked == lkB {
			want, other, wantKey, otherKey = b, a, lkKeyB, lkKeyA
		}
		if !want.GetStatus() || !fileStates.IsPlayed(wantKey) {
			t.Fatal("the linked stub was not confirmed and marked played")
		}
		if other.GetStatus() || fileStates.EverPlayed(otherKey) {
			t.Fatal("the unlinked stub was touched")
		}
	}
}

// The bootstrap's candidates share only the library type: no linked pick there.
func TestBootstrapTieIsNotBrokenByLinks(t *testing.T) {
	a, b := linkedEnv(t, "") // neither carries an id yet
	linkedNames(lkB)
	payload := strings.Replace(lkPayload("media.play", lkI), `"grandparentTitle":"Show"`, `"grandparentTitle":"Unrelated Programme"`, 1)
	payload = strings.Replace(payload, `"title":"Pilot"`, `"title":"Zzz"`, 1)
	if got := playRule(t, payload); got != ruleNone {
		t.Fatalf("rule = %d, want none (%d)", got, ruleNone)
	}
	if a.GetStatus() || b.GetStatus() {
		t.Fatal("a library-type tie confirmed a stub")
	}
}

func TestLinkedStubWinsAFuzzyTie(t *testing.T) {
	a, b := linkedEnv(t, "")
	linkedNames(lkA)
	if got := playRule(t, lkPayload("media.play", "")); got != ruleLinked {
		t.Fatalf("rule = %d, want linked (%d)", got, ruleLinked)
	}
	if !a.GetStatus() || b.GetStatus() || !fileStates.IsPlayed(lkKeyA) || fileStates.EverPlayed(lkKeyB) {
		t.Fatal("the fuzzy tie did not pick the linked stub alone")
	}
}

// openHandle holds a FUSE handle on path, the way a player does.
func openHandle(t *testing.T, path string) {
	t.Helper()
	h := &MkvHandle{path: path}
	app.activeHandles.Store(h, true)
	t.Cleanup(func() { app.activeHandles.Delete(h) })
}

func TestLinkedStubStopIsHonouredWithAHandleOpen(t *testing.T) {
	for _, imdb := range []string{lkI, ""} { // the id tie, then the fuzzy tie
		a, b := linkedEnv(t, imdb)
		linkedNames(lkA)
		a.SetHealthy(true)
		b.SetHealthy(true)
		openHandle(t, lkA)
		openHandle(t, lkB)
		postWebhook(t, lkPayload("media.stop", imdb))
		if a.GetStatus() {
			t.Fatalf("imdb %q: the stop did not reach the linked stub", imdb)
		}
		if !b.GetStatus() {
			t.Fatalf("imdb %q: the stop reached the unlinked stub", imdb)
		}
	}
}

// Two stubs of E03 (one linked, both open) and a late stop for E05, whose entry is gone:
// the series title ties both E03 stubs, and neither may be released.
func TestLinkedStopForAnotherEpisodeReleasesNothing(t *testing.T) {
	const e3new = "/mnt/flodni/tv/Show (2020)/Season.01/Show_S01E03_aaaaaaaa.mkv"
	const e3old = "/mnt/flodni/tv/Show (2020)/Season.01/Show_S01E03_bbbbbbbb.mkv"
	linkedEnv(t, "")
	app.playbackRegistry.Delete(lkA)
	app.playbackRegistry.Delete(lkB)
	newS, oldS := &PlaybackState{Path: e3new, OpenedAt: time.Now()}, &PlaybackState{Path: e3old, OpenedAt: time.Now()}
	app.playbackRegistry.Store(e3new, newS)
	app.playbackRegistry.Store(e3old, oldS)
	t.Cleanup(func() { app.playbackRegistry.Delete(e3new); app.playbackRegistry.Delete(e3old) })
	linkedNames(e3new)
	newS.SetHealthy(true)
	oldS.SetHealthy(true)
	openHandle(t, e3new)
	openHandle(t, e3old)
	payload := strings.Replace(lkPayload("media.stop", ""), `"index":2`, `"index":5`, 1)
	postWebhook(t, payload)
	if !newS.GetStatus() || !oldS.GetStatus() {
		t.Fatal("a stop for another episode released a stream being watched")
	}
	// Without a season and episode in the payload there is no linked pick either.
	payload = strings.Replace(lkPayload("media.stop", ""), `,"parentIndex":1,"index":2`, ``, 1)
	postWebhook(t, payload)
	if !newS.GetStatus() || !oldS.GetStatus() {
		t.Fatal("a stop with no episode released a stream being watched")
	}
}

func TestLinkedPlayFuzzyTieNeedsTheEpisodeToAgree(t *testing.T) {
	const e3 = "/mnt/flodni/tv/Show (2020)/Season.01/Show_S01E03_cccccccc.mkv"
	a, b := linkedEnv(t, "") // both S01E02
	s3 := &PlaybackState{Path: e3, OpenedAt: time.Now()}
	app.playbackRegistry.Store(e3, s3)
	t.Cleanup(func() { app.playbackRegistry.Delete(e3) })
	linkedNames(lkA, lkB, e3) // both E02 stubs linked: no pick
	if got := playRule(t, lkPayload("media.play", "")); got == ruleLinked {
		t.Fatal("a tie across episodes was settled by a link")
	}
	_, _, _ = a, b, s3
	// Only E03 linked: the payload says E02, so E03 must not be picked.
	linkedNames(e3)
	if got := playRule(t, lkPayload("media.play", "")); got == ruleLinked {
		t.Fatal("a linked stub of another episode was picked")
	}
}

const (
	mvAliens = "/mnt/flodni/movies/Aliens.1986.1080p.BluRay.mkv"
	mvAlien  = "/mnt/flodni/movies/Alien.1979.1080p.BluRay.mkv"
	mvSameA  = "/mnt/flodni/movies/Heat.1995.1080p.WEB.aaaaaaaa.mkv"
	mvSameB  = "/mnt/flodni/movies/Heat.1995.1080p.WEB.bbbbbbbb.mkv"
)

func mvPayload(event, title, year string) string {
	y := ""
	if year != "" {
		y = `,"year":` + year
	}
	return `{"event":"` + event + `","Metadata":{"title":"` + title + `","librarySectionType":"movie"` + y + `}}`
}

func mvEnv(t *testing.T, paths ...string) map[string]*PlaybackState {
	t.Helper()
	linkedEnv(t, "")
	out := map[string]*PlaybackState{}
	for _, p := range paths {
		st := &PlaybackState{Path: p, OpenedAt: time.Now()}
		app.playbackRegistry.Store(p, st)
		out[p] = st
		p := p
		t.Cleanup(func() { app.playbackRegistry.Delete(p) })
	}
	return out
}

// "Alien" is a substring of "Aliens": a late stop for a movie whose entry is gone ties
// both, and the linked, open one must not be released.
func TestLinkedStopForAnotherMovieReleasesNothing(t *testing.T) {
	for _, year := range []string{"", "1979"} {
		m := mvEnv(t, mvAliens, mvAlien)
		linkedNames(mvAliens)
		for p, st := range m {
			st.SetHealthy(true)
			openHandle(t, p)
		}
		postWebhook(t, mvPayload("media.stop", "Alien", year))
		if !m[mvAliens].GetStatus() || !m[mvAlien].GetStatus() {
			t.Fatalf("year %q: a stop for another movie released a stream being watched", year)
		}
	}
}

func TestLinkedPlayOfAnotherMovieIsNotPicked(t *testing.T) {
	for _, year := range []string{"", "1979"} {
		m := mvEnv(t, mvAliens, mvAlien)
		linkedNames(mvAliens)
		if got := playRule(t, mvPayload("media.play", "Alien", year)); got == ruleLinked {
			t.Fatalf("year %q: a title substring was settled by a link", year)
		}
		// With no year nothing settles the tie: plain fuzzy picks either one in Range
		// order, as before the tie-break. With the year, Alien ranks above Aliens.
		if year != "" && m[mvAliens].GetStatus() {
			t.Fatalf("year %q: the linked stub of another movie was confirmed", year)
		}
	}
}

func TestLinkedStubOfTheSameMovieStillWins(t *testing.T) {
	m := mvEnv(t, mvSameA, mvSameB)
	linkedNames(mvSameB)
	if got := playRule(t, mvPayload("media.play", "Heat", "1995")); got != ruleLinked {
		t.Fatalf("rule = %d, want linked (%d)", got, ruleLinked)
	}
	if !m[mvSameB].GetStatus() || m[mvSameA].GetStatus() {
		t.Fatal("the linked copy of the movie was not the one confirmed")
	}
	for p, st := range m {
		st.SetHealthy(true)
		openHandle(t, p)
	}
	postWebhook(t, mvPayload("media.stop", "Heat", "1995"))
	if m[mvSameB].GetStatus() || !m[mvSameA].GetStatus() {
		t.Fatal("the stop did not release the linked copy alone")
	}
}

func TestNoLinkedStubKeepsTodaysBehaviour(t *testing.T) {
	cases := map[string]func(){
		"no walk yet": func() {},
		"neither":     func() { linkedNames("/x/other.mkv") },
		"both":        func() { linkedNames(lkA, lkB) },
		"empty set":   func() { linkedNames() },
	}
	for name, setup := range cases {
		a, b := linkedEnv(t, lkI)
		setup()
		if got := playRule(t, lkPayload("media.play", lkI)); got != ruleIMDB {
			t.Fatalf("%s: rule = %d, want imdb (%d)", name, got, ruleIMDB)
		}
		if a.GetStatus() == b.GetStatus() {
			t.Fatalf("%s: want exactly one stub confirmed, as before", name)
		}
		// A stop with both handles open stays ambiguous: ignored, nothing released.
		openHandle(t, lkA)
		openHandle(t, lkB)
		a.SetHealthy(true)
		b.SetHealthy(true)
		postWebhook(t, lkPayload("media.stop", lkI))
		if !a.GetStatus() || !b.GetStatus() {
			t.Fatalf("%s: an ambiguous stop with handles open released a stub", name)
		}
	}
}

func TestOneCandidateIsNotNarrowed(t *testing.T) {
	a, b := linkedEnv(t, lkI)
	app.playbackRegistry.Delete(lkB) // the replaced release is gone: one candidate
	linkedNames("/x/other.mkv")      // and it is not linked either
	if got := playRule(t, lkPayload("media.play", lkI)); got != ruleIMDB {
		t.Fatalf("rule = %d, want imdb (%d)", got, ruleIMDB)
	}
	if !a.GetStatus() || b.GetStatus() {
		t.Fatal("a single candidate was not confirmed as before")
	}
}

func TestAnAmbiguousWebhookAsksForAWalkAndNeverWalks(t *testing.T) {
	root := t.TempDir()
	pmLink(t, root, "/lib/a.mkv", lkA)
	linkedEnv(t, lkI)
	globalConfig.Store(&config.Config{PlexPathMap: map[string]string{"/series": root}})
	postWebhook(t, lkPayload("media.play", lkI))
	if len(app.linked.kick) != 1 {
		t.Fatal("two candidates did not ask for a walk")
	}
	if app.linked.set.Load() != nil {
		t.Fatal("the handler walked the library inline")
	}
	for i := 0; i < 5; i++ {
		app.linked.request()
	}
	if len(app.linked.kick) != 1 {
		t.Fatal("requests are not coalesced")
	}

	<-app.linked.kick
	app.playbackRegistry.Delete(lkB)
	postWebhook(t, lkPayload("media.play", lkI))
	if len(app.linked.kick) != 0 {
		t.Fatal("a single candidate asked for a walk")
	}
}

// ---- the walker ----

func TestWalkLinkedStubsReadsLinksOnly(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	pmLink(t, root, "/movies/a.mkv", "/mnt/flodni/movies/Stub_A.mkv")        // absolute
	pmLink(t, root, "/series/Show/Season 1/b.mkv", "../../up/Stub_B.mkv")    // relative to the link's folder
	pmLink(t, root, "/series/dangling.mkv", "/nonexistent/Stub_C.mkv")       // target missing: still a name
	pmLink(t, root, "/dir", outside)                                         // a link to a folder: never entered
	pmLink(t, outside, "/inside.mkv", "/mnt/flodni/movies/Stub_Outside.mkv") // behind that link
	plain := filepath.Join(root, "series", "plain.mkv")
	if err := os.WriteFile(plain, []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "series", "pipe.mkv") // opening a FIFO for reading blocks forever
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	type result struct {
		set stubSet
		ok  bool
	}
	done := make(chan result, 1)
	go func() {
		s, _, ok := walkLinkedStubs([]string{root}, nil, walkLimits{MaxEntries: 1000})
		done <- result{s, ok}
	}()
	var got result
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the walk hung: it opened a file")
	}
	if !got.ok {
		t.Fatal("the walk failed")
	}
	want := []string{"Stub_A.mkv", "Stub_B.mkv", "Stub_C.mkv"}
	for _, w := range want {
		if _, ok := got.set[w]; !ok {
			t.Errorf("%s is missing from %v", w, got.set)
		}
	}
	if _, ok := got.set["Stub_Outside.mkv"]; ok {
		t.Error("a link to a folder was followed")
	}
	if _, ok := got.set["plain.mkv"]; ok || len(got.set) != len(want)+1 { // +1: the folder link's own target name
		t.Errorf("unexpected entries: %v", got.set)
	}
}

func TestWalkLinkedStubsIsBounded(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a", "b", "c", "d"} {
		pmLink(t, root, n+".mkv", "/x/"+n)
	}
	if _, _, ok := walkLinkedStubs([]string{root}, nil, walkLimits{MaxEntries: 2}); ok {
		t.Fatal("a walk past its entry bound published a partial set")
	}
	if _, _, ok := walkLinkedStubs([]string{root}, nil, walkLimits{MaxEntries: 100, Deadline: time.Now().Add(-time.Second)}); ok {
		t.Fatal("a walk past its deadline published a partial set")
	}
	stop := make(chan struct{})
	close(stop)
	if _, _, ok := walkLinkedStubs([]string{root}, stop, walkLimits{MaxEntries: 100}); ok {
		t.Fatal("a stopped walk published a set")
	}
	if s, n, ok := walkLinkedStubs([]string{root}, nil, walkLimits{MaxEntries: 100}); !ok || len(s) != 4 || n < 4 {
		t.Fatalf("within bounds: ok=%v set=%v entries=%d", ok, s, n)
	}
}

func TestLinkedWalkerPublishesAndDebouncesEarlyWalks(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	pmLink(t, root, "a.mkv", "/x/Stub_A.mkv")
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		runLinkedWalker(&a.linked, func() []string { return []string{root} }, stop, time.Hour, 200*time.Millisecond)
		close(finished)
	}()
	t.Cleanup(func() { close(stop); <-finished })

	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for i := 0; i < 500; i++ {
			if ok() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	has := func(name string) func() bool {
		return func() bool { s := a.linked.current(); _, ok := s[name]; return ok }
	}
	waitFor("the first walk", has("Stub_A.mkv"))

	pmLink(t, root, "b.mkv", "/x/Stub_B.mkv")
	for i := 0; i < 50; i++ {
		a.linked.request()
	}
	waitFor("the early walk", has("Stub_B.mkv"))
	time.Sleep(300 * time.Millisecond)
	if n := a.linked.walks.Load(); n != 2 {
		t.Fatalf("%d walks for a first walk and one burst of requests, want 2", n)
	}
}

func TestLinkedWalkerWithoutRootsPublishesNothing(t *testing.T) {
	a := newTestApp(t)
	s := stubSet{"Stub_Old.mkv": {}}
	a.linked.publish(s, time.Now()) // a root removed from the config since the last walk
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		runLinkedWalker(&a.linked, func() []string { return nil }, stop, time.Hour, time.Millisecond)
		close(finished)
	}()
	for i := 0; i < 500 && a.linked.set.Load() != nil; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	close(stop)
	<-finished
	if a.linked.set.Load() != nil {
		t.Fatal("a set from a removed root still narrows matches")
	}
}

func TestLinkedRootsAreThePathMapTargets(t *testing.T) {
	prev := globalConfig.Load()
	t.Cleanup(func() { globalConfig.Store(prev) })
	globalConfig.Store(&config.Config{PlexPathMap: map[string]string{
		"/movies": "/media/movies", "/series": "/media/series", "/more": "/media/movies", // one root twice
	}})
	got := linkedRoots()
	if len(got) != 2 || got[0] != "/media/movies" || got[1] != "/media/series" {
		t.Fatalf("roots = %v", got)
	}
	globalConfig.Store(&config.Config{})
	if got := linkedRoots(); len(got) != 0 {
		t.Fatalf("roots with no plex_path_map = %v", got)
	}
}

func TestWalkLinkedStubsRootFailureDoesNotPublish(t *testing.T) {
	root := t.TempDir()
	pmLink(t, root, "a.mkv", "/x/Stub_A.mkv")
	var warned []string
	lim := walkLimits{MaxEntries: 100, Warn: func(p string, _ error) { warned = append(warned, p) }}
	if _, _, ok := walkLinkedStubs([]string{root, filepath.Join(root, "missing")}, nil, lim); ok {
		t.Fatal("a missing root still published a set")
	}
	if len(warned) == 0 {
		t.Fatal("a missing root was not reported")
	}
}

func TestWalkLinkedStubsResolvesASymlinkedRootOnce(t *testing.T) {
	real, holder := t.TempDir(), t.TempDir()
	pmLink(t, real, "a.mkv", "/x/Stub_A.mkv")
	pmLink(t, real, "sub/b.mkv", "../Stub_B.mkv")
	rootLink := filepath.Join(holder, "lib")
	if err := os.Symlink(real, rootLink); err != nil {
		t.Fatal(err)
	}
	set, _, ok := walkLinkedStubs([]string{rootLink}, nil, walkLimits{MaxEntries: 100})
	if !ok || len(set) != 2 {
		t.Fatalf("ok=%v set=%v, want Stub_A and Stub_B only", ok, set)
	}
	if _, in := set[filepath.Base(real)]; in {
		t.Fatal("the root's own target name was recorded")
	}
}

func TestLinkedSetOlderThanThreeWalksIsIgnored(t *testing.T) {
	a := newTestApp(t)
	a.linked.publish(stubSet{"x.mkv": {}}, time.Now().Add(-2*linkedWalkInterval))
	if a.linked.current() == nil {
		t.Fatal("a set two walks old was dropped")
	}
	a.linked.publish(stubSet{"x.mkv": {}}, time.Now().Add(-3*linkedWalkInterval-time.Second))
	if a.linked.current() != nil {
		t.Fatal("a stale set still narrows matches")
	}
}

func TestLinkedWalkerKeepsTheSetWhenARootFails(t *testing.T) {
	a := newTestApp(t)
	s := stubSet{"Stub_Old.mkv": {}}
	a.linked.publish(s, time.Now())
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		runLinkedWalker(&a.linked, func() []string { return []string{filepath.Join(t.TempDir(), "gone")} }, stop, time.Hour, time.Millisecond)
		close(finished)
	}()
	for i := 0; i < 500 && a.linked.walks.Load() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	close(stop)
	<-finished
	if _, ok := a.linked.current()["Stub_Old.mkv"]; !ok {
		t.Fatal("a failed root dropped the previous set")
	}
}

func TestLinkedWalkerForgetsWarningsAfterACleanWalk(t *testing.T) {
	a := newTestApp(t)
	a.linked.warn.Store("/old/path", struct{}{})
	root := t.TempDir()
	pmLink(t, root, "a.mkv", "/x/Stub_A.mkv")
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		runLinkedWalker(&a.linked, func() []string { return []string{root} }, stop, time.Hour, time.Millisecond)
		close(finished)
	}()
	for i := 0; i < 500 && a.linked.current() == nil; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	close(stop)
	<-finished
	if _, ok := a.linked.warn.Load("/old/path"); ok {
		t.Fatal("a clean walk kept its old warnings")
	}
}

// Without any link, a fuzzy title tie with a known year keeps the files carrying that
// year: "Alien" is a substring of "Aliens", and a play of Alien (1979) must confirm it,
// whatever order the registry is ranged in. A stop narrows the same way.
func TestFuzzyTieWithAYearKeepsTheFilesOfThatYear(t *testing.T) {
	for i := 0; i < 20; i++ {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			m := mvEnv(t, mvAliens, mvAlien)
			playRule(t, mvPayload("media.play", "Alien", "1979"))
			if !m[mvAlien].GetStatus() || m[mvAliens].GetStatus() {
				t.Fatalf("a play of Alien (1979) confirmed the wrong file (alien %v, aliens %v)", m[mvAlien].GetStatus(), m[mvAliens].GetStatus())
			}
		})
	}
}

func TestFuzzyStopTieWithAYearStopsTheFileOfThatYear(t *testing.T) {
	for i := 0; i < 20; i++ {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			m := mvEnv(t, mvAliens, mvAlien)
			for _, st := range m {
				st.SetHealthy(true)
			}
			postWebhook(t, mvPayload("media.stop", "Alien", "1979"))
			if m[mvAlien].GetStatus() || !m[mvAliens].GetStatus() {
				t.Fatalf("a stop of Alien (1979) hit the wrong file (alien playing %v, aliens playing %v)", m[mvAlien].GetStatus(), m[mvAliens].GetStatus())
			}
		})
	}
}
