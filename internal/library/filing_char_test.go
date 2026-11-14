package library

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"flodni/internal/metadb"
)

// Characterization tests: they freeze how the manager files movies, single
// episodes and season packs today. The engine is replaceEngine and the registry
// memRegistry (replace_test.go).

const (
	charPackHash  = charHash                                   // head 11111111, tail 55555555
	charOtherHash = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef" // head deadbeef
	charThirdHash = "cafebabecafebabecafebabecafebabecafebabe" // head cafebabe
)

type charEnv struct {
	m      *Manager
	eng    *replaceEngine
	reg    *memRegistry
	movies string
	tv     string
}

func newCharEnv(t *testing.T, files ...FileStat) *charEnv {
	t.Helper()
	root := t.TempDir()
	e := &charEnv{
		eng:    &replaceEngine{files: files},
		reg:    &memRegistry{rows: map[string]metadb.EpisodeEntry{}},
		movies: filepath.Join(root, "movies"),
		tv:     filepath.Join(root, "tv"),
	}
	e.m = New(Config{MoviesDir: e.movies, TVDir: e.tv, GoStorm: e.eng, Registry: e.reg, GoStormURL: "http://engine/"})
	return e
}

func (e *charEnv) add(req AddRequest) (*AddResponse, error) {
	req.MetadataWait = 1
	return e.m.Add(context.Background(), req)
}

// mkvs lists the stub files under dir, relative to it, sorted.
func mkvs(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// jsonAmp spells & the way encoding/json writes it: json.Marshal escapes it as a
// backslash, u, 0026. Stub bytes on disk carry that form.
func jsonAmp(s string) string { return strings.ReplaceAll(s, "&", "\\"+"u0026") }

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func statusOf(t *testing.T, err error) int {
	t.Helper()
	apiErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %v, want a library error", err)
	}
	return apiErr.Status
}

var movieTorrent = []FileStat{
	{ID: 1, Path: "Inception/Sample/sample.mkv", Length: 50 << 20},
	{ID: 2, Path: "Inception/Inception.2010.1080p.BluRay.mkv", Length: 8 << 30},
	{ID: 3, Path: "Inception/Inception.nfo", Length: 900},
	{ID: 4, Path: "Inception/extras/making-of.mp4", Length: 700 << 20},
}

// ---- stub contents ----

func TestWriteStubWritesKeySortedJSONWithEscapedAmpersands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "x.mkv")
	if err := WriteStub(path, "http://e/stream?link=h&index=2&play", 1234, "magnet:?xt=urn:btih:h&dn=x", "tt0000001"); err != nil {
		t.Fatal(err)
	}
	want := jsonAmp(`{"imdb":"tt0000001","magnet":"magnet:?xt=urn:btih:h&dn=x","size":1234,"url":"http://e/stream?link=h&index=2&play"}`)
	if got := readFile(t, path); got != want {
		t.Fatalf("stub bytes\n got %s\nwant %s", got, want)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644", info.Mode().Perm())
	}
}

func TestReadStubParsesJSONAndLegacyForms(t *testing.T) {
	dir := t.TempDir()
	jsonStub := filepath.Join(dir, "j.mkv")
	if err := WriteStub(jsonStub, "http://e/stream?link="+strings.ToUpper(charHash)+"&index=7&play", 99, "m", "tt1234567"); err != nil {
		t.Fatal(err)
	}
	if got, want := readStub(jsonStub), (stub{Size: 99, Hash: charHash, IMDB: "tt1234567", FileIndex: 7}); got != want {
		t.Fatalf("json stub = %+v, want %+v", got, want)
	}

	legacy := filepath.Join(dir, "l.mkv")
	body := "http://e/stream?link=" + charHash + "&index=3&play\n4321\nmagnet:?xt=urn:btih:x\ntt7654321\n"
	if err := os.WriteFile(legacy, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := readStub(legacy), (stub{Size: 4321, Hash: charHash, IMDB: "tt7654321", FileIndex: 3}); got != want {
		t.Fatalf("legacy stub = %+v, want %+v", got, want)
	}
	if got := readStub(filepath.Join(dir, "missing.mkv")); got != (stub{}) {
		t.Fatalf("missing stub = %+v, want zero", got)
	}
}

// ---- movies ----

func TestMovieAddWritesOneStubForTheLargestVideoFile(t *testing.T) {
	e := newCharEnv(t, movieTorrent...)

	resp, err := e.add(AddRequest{
		Type: "movie", Title: "Inception", Hash: charPackHash, Year: 2010,
		ReleaseTitle: "Inception.2010.1080p.BluRay", IMDB: "tt1375666",
	})
	if err != nil {
		t.Fatal(err)
	}

	name := "Inception_2010_1080p_55555555.mkv"
	path := filepath.Join(e.movies, name)
	want := &AddResponse{
		Hash: charPackHash, Title: "Inception", Type: "movie",
		Files: []AddedFile{{Path: path, FusePath: "movies/" + name, Size: 8 << 30, FileIndex: 2}},
	}
	if !reflect.DeepEqual(resp, want) {
		t.Fatalf("response = %+v, want %+v", resp, want)
	}
	if got := mkvs(t, e.movies); !reflect.DeepEqual(got, []string{name}) {
		t.Fatalf("movies dir = %v", got)
	}
	// The trailing slash of GoStormURL is trimmed; the magnet is the bare hash plus the title.
	wantBytes := jsonAmp(`{"imdb":"tt1375666","magnet":"magnet:?xt=urn:btih:` + charPackHash +
		`&dn=Inception","size":8589934592,"url":"http://engine/stream?link=` + charPackHash +
		`&index=2&play"}`)
	if got := readFile(t, path); got != wantBytes {
		t.Fatalf("stub bytes\n got %s\nwant %s", got, wantBytes)
	}
	if len(e.reg.rows) != 0 {
		t.Fatalf("a movie wrote registry rows: %v", e.reg.rows)
	}
}

func TestMovieAddKeepsTheCallersMagnetVerbatim(t *testing.T) {
	e := newCharEnv(t, movieTorrent...)
	magnet := "magnet:?xt=urn:btih:" + strings.ToUpper(charPackHash) + "&dn=Own&tr=udp%3A%2F%2Ft.example%3A1%2Fannounce"

	resp, err := e.add(AddRequest{Title: "Inception", Magnet: magnet, ReleaseDate: "2010-07-16"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Hash != charPackHash || resp.Type != "movie" {
		t.Fatalf("response = %+v", resp)
	}
	got := readFile(t, resp.Files[0].Path)
	if !strings.Contains(got, `"magnet":"`+jsonAmp(magnet)+`"`) {
		t.Fatalf("stub does not carry the magnet as sent: %s", got)
	}
	if !strings.Contains(got, `"imdb":""`) {
		t.Fatalf("stub without an imdb id must carry an empty one: %s", got)
	}
}

func TestMovieAddReadsQualityFromReleaseTitleAndLetsIs4KOverrideIt(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		name    string
		release string
		is4K    *bool
		want    string
	}{
		{"2160p in the release title", "Movie.2020.2160p.UHD.BluRay.REMUX.DV.Atmos", nil, "Movie_2020_2160p_DV_Atmos_REMUX_55555555.mkv"},
		{"the flag forces 2160p", "Movie.2020.WEB-DL", &yes, "Movie_2020_2160p_55555555.mkv"},
		{"the flag forces 1080p over a 2160p title", "Movie.2020.2160p.WEB-DL", &no, "Movie_2020_1080p_55555555.mkv"},
		{"no release title falls back to the title", "", nil, "Movie_2020_1080p_55555555.mkv"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newCharEnv(t, FileStat{ID: 1, Path: "m.mkv", Length: 10})
			resp, err := e.add(AddRequest{Title: "Movie", Hash: charPackHash, ReleaseDate: "2020", ReleaseTitle: c.release, Is4K: c.is4K})
			if err != nil {
				t.Fatal(err)
			}
			if got := filepath.Base(resp.Files[0].Path); got != c.want {
				t.Fatalf("name = %q, want %q", got, c.want)
			}
		})
	}
}

func TestMovieAddFileIndexSelection(t *testing.T) {
	for _, c := range []struct {
		name       string
		index      int
		wantID     int
		wantStatus int
	}{
		{"0 picks the largest video file", 0, 2, 0},
		{"an explicit index picks that file even when smaller", 4, 4, 0},
		{"a sample can be chosen explicitly", 1, 1, 0},
		{"a non-video file is refused", 3, 0, http.StatusBadRequest},
		{"an absent file is refused", 9, 0, http.StatusBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newCharEnv(t, movieTorrent...)
			resp, err := e.add(AddRequest{Title: "Inception", Hash: charPackHash, Year: 2010, FileIndex: c.index})
			if c.wantStatus != 0 {
				if statusOf(t, err) != c.wantStatus {
					t.Fatalf("err = %v, want status %d", err, c.wantStatus)
				}
				if got := mkvs(t, e.movies); len(got) != 0 {
					t.Fatalf("a refused add left stubs: %v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			f := resp.Files[0]
			if f.FileIndex != c.wantID || !strings.Contains(readFile(t, f.Path), "index="+string(rune('0'+c.wantID))) {
				t.Fatalf("file = %+v, want id %d", f, c.wantID)
			}
		})
	}
}

func TestMovieAddOfTorrentWithoutVideoIsRefusedAndTorrentDropped(t *testing.T) {
	e := newCharEnv(t, FileStat{ID: 1, Path: "readme.txt", Length: 5})
	_, err := e.add(AddRequest{Title: "Nothing", Hash: charPackHash})
	if statusOf(t, err) != http.StatusUnprocessableEntity {
		t.Fatalf("err = %v, want 422", err)
	}
	if !reflect.DeepEqual(e.eng.removed, []string{charPackHash}) {
		t.Fatalf("removed = %v, want the new torrent", e.eng.removed)
	}
}

func TestMovieReaddIsIdempotent(t *testing.T) {
	e := newCharEnv(t, movieTorrent...)
	first, err := e.add(AddRequest{Title: "Inception", Hash: charPackHash, Year: 2010})
	if err != nil {
		t.Fatal(err)
	}
	before := readFile(t, first.Files[0].Path)

	second, err := e.add(AddRequest{Title: "Inception", Hash: charPackHash, Year: 2010, FileIndex: 4})
	if err != nil {
		t.Fatal(err)
	}
	if !second.AlreadyPresent {
		t.Fatal("second add not reported as already present")
	}
	if !reflect.DeepEqual(second.Files, first.Files) {
		t.Fatalf("second files = %+v, want %+v", second.Files, first.Files)
	}
	if got := readFile(t, first.Files[0].Path); got != before {
		t.Fatal("the stub was rewritten")
	}
	if got := mkvs(t, e.movies); len(got) != 1 {
		t.Fatalf("movies dir = %v, want one stub", got)
	}
}

func TestMovieTypeAliasesAndValidation(t *testing.T) {
	for _, typ := range []string{"", "movie", "Movies", "film"} {
		e := newCharEnv(t, FileStat{ID: 1, Path: "m.mkv", Length: 10})
		resp, err := e.add(AddRequest{Type: typ, Title: "M", Hash: charPackHash})
		if err != nil || resp.Type != "movie" {
			t.Errorf("type %q: resp = %+v, err = %v", typ, resp, err)
		}
	}
	e := newCharEnv(t)
	for name, c := range map[string]struct {
		req    AddRequest
		status int
	}{
		"no title":             {AddRequest{Hash: charPackHash}, 400},
		"no hash or magnet":    {AddRequest{Title: "M"}, 400},
		"malformed hash":       {AddRequest{Title: "M", Hash: "xyz"}, 400},
		"magnet without btih":  {AddRequest{Title: "M", Magnet: "magnet:?dn=x"}, 400},
		"unknown type":         {AddRequest{Type: "game", Title: "M", Hash: charPackHash}, 400},
		"tv without season":    {AddRequest{Type: "tv", Title: "M", Hash: charPackHash}, 400},
		"pack with file_index": {AddRequest{Type: "tv", Title: "M", Hash: charPackHash, Season: 1, FileIndex: 2}, 400},
	} {
		_, err := e.add(c.req)
		if statusOf(t, err) != c.status {
			t.Errorf("%s: err = %v, want %d", name, err, c.status)
		}
	}
}

// ---- single episodes ----

var episodeTorrent = []FileStat{
	{ID: 1, Path: "Show.S02E03/Sample/show.s02e03.sample.mkv", Length: 20 << 20},
	{ID: 2, Path: "Show.S02E03/Show.S02E03.1080p.mkv", Length: 2 << 30},
	{ID: 3, Path: "Show.S02E03/Show.S02E04.1080p.mkv", Length: 3 << 30},
	{ID: 4, Path: "Show.S02E03/Show.S02E03.srt", Length: 40},
}

func TestSingleEpisodeAddWritesOneStubAndOneRegistryRow(t *testing.T) {
	e := newCharEnv(t, episodeTorrent...)

	resp, err := e.add(AddRequest{
		Type: "tv", Title: "Marvel's Show", Hash: charPackHash, Season: 2, Episode: 3,
		FirstAirDate: "2019-05-01", IMDB: "tt9999999", QualityScore: 77,
	})
	if err != nil {
		t.Fatal(err)
	}

	rel := "Marvels_Show (2019)/Season.02/Marvels_Show_S02E03_11111111.mkv"
	path := filepath.Join(e.tv, filepath.FromSlash(rel))
	want := []AddedFile{{Path: path, FusePath: "tv/" + rel, Size: 2 << 30, FileIndex: 2, Season: 2, Episode: 3}}
	if !reflect.DeepEqual(resp.Files, want) || resp.Type != "tv" || resp.AlreadyPresent {
		t.Fatalf("response = %+v, want files %+v", resp, want)
	}
	if got := mkvs(t, e.tv); !reflect.DeepEqual(got, []string{rel}) {
		t.Fatalf("tv dir = %v", got)
	}
	// The request's imdb id is not written: an id here takes the episode out of the
	// webhook matcher.
	wantBytes := jsonAmp(`{"imdb":"","magnet":"magnet:?xt=urn:btih:` + charPackHash +
		`&dn=Marvel%27s+Show","size":2147483648,"url":"http://engine/stream?link=` + charPackHash +
		`&index=2&play"}`)
	if got := readFile(t, path); got != wantBytes {
		t.Fatalf("stub bytes\n got %s\nwant %s", got, wantBytes)
	}

	key := "marvelsshow_s02e03"
	if len(e.reg.rows) != 1 {
		t.Fatalf("registry = %+v, want one row", e.reg.rows)
	}
	row := e.reg.rows[key]
	if row.EpisodeKey != key || row.Hash != charPackHash || row.FilePath != path ||
		row.Source != "api" || row.QualityScore != 77 || row.Created == 0 || row.ShowIMDB != "" {
		t.Fatalf("row = %+v", row)
	}
}

func TestSingleEpisodeAddWithoutAirDateHasNoYearFolder(t *testing.T) {
	e := newCharEnv(t, episodeTorrent...)
	resp, err := e.add(AddRequest{Type: "series", Title: "Show", Hash: charPackHash, Season: 2, Episode: 3})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Files[0].FusePath; got != "tv/Show/Season.02/Show_S02E03_11111111.mkv" {
		t.Fatalf("fuse path = %q", got)
	}
}

func TestSingleEpisodePicksTheFileNamingItAndFallsBackToLargest(t *testing.T) {
	for _, c := range []struct {
		name            string
		files           []FileStat
		season, episode int
		fileIndex       int
		wantID          int
	}{
		{"the file naming the episode beats a larger sibling", episodeTorrent, 2, 3, 0, 2},
		{"another episode of the same torrent", episodeTorrent, 2, 4, 0, 3},
		{"a season mismatch does not count", episodeTorrent, 1, 3, 0, 3},
		{"no file names it: the largest video file", []FileStat{
			{ID: 1, Path: "a.mkv", Length: 10}, {ID: 2, Path: "b.mkv", Length: 30}}, 1, 5, 0, 2},
		{"an explicit file index wins", episodeTorrent, 2, 3, 3, 3},
		{"a file naming the episode without a season counts", []FileStat{
			{ID: 1, Path: "big.mkv", Length: 90}, {ID: 2, Path: "x.e07.mkv", Length: 10}}, 1, 7, 0, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newCharEnv(t, c.files...)
			resp, err := e.add(AddRequest{Type: "tv", Title: "Show", Hash: charPackHash, Season: c.season, Episode: c.episode, FileIndex: c.fileIndex})
			if err != nil {
				t.Fatal(err)
			}
			if got := resp.Files[0].FileIndex; got != c.wantID {
				t.Fatalf("file index = %d, want %d", got, c.wantID)
			}
		})
	}
}

func TestSingleEpisodeReaddIsIdempotent(t *testing.T) {
	e := newCharEnv(t, episodeTorrent...)
	req := AddRequest{Type: "tv", Title: "Show", Hash: charPackHash, Season: 2, Episode: 3}
	first, err := e.add(req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.add(req)
	if err != nil {
		t.Fatal(err)
	}
	if !second.AlreadyPresent || !reflect.DeepEqual(second.Files, first.Files) {
		t.Fatalf("second = %+v, first = %+v", second, first)
	}
	if got := mkvs(t, e.tv); len(got) != 1 || len(e.reg.rows) != 1 {
		t.Fatalf("stubs = %v, rows = %d, want one of each", got, len(e.reg.rows))
	}
}

func TestEpisodeAddWithoutRegistryIsRefused(t *testing.T) {
	e := newCharEnv(t, episodeTorrent...)
	e.m.cfg.Registry = nil
	_, err := e.add(AddRequest{Type: "tv", Title: "Show", Hash: charPackHash, Season: 2, Episode: 3})
	if statusOf(t, err) != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want 503", err)
	}
}

// ---- season packs ----

var seasonPack = []FileStat{
	{ID: 1, Path: "Show.S01/Show.S01E01.1080p.mkv", Length: 1000},
	{ID: 2, Path: "Show.S01/Show.S01E02.1080p.mkv", Length: 2000},
	{ID: 3, Path: "Show.S01/Show.S01E03.1080p.mp4", Length: 3000},
	{ID: 4, Path: "Show.S01/Show.S01E01.1080p.nfo", Length: 5},
	{ID: 5, Path: "Show.S01/Subs/Show.S01E01.srt", Length: 5},
	{ID: 6, Path: "Show.S01/Sample/Show.S01E01.sample.mkv", Length: 100},
	{ID: 7, Path: "Show.S01/Extras/featurette.mkv", Length: 9000},
}

func TestSeasonPackFilesOneStubPerNamedVideoFile(t *testing.T) {
	e := newCharEnv(t, seasonPack...)

	resp, err := e.add(AddRequest{Type: "tv", Title: "Show", Hash: charPackHash, Season: 1, FirstAirDate: "2020-01-01", QualityScore: 5})
	if err != nil {
		t.Fatal(err)
	}

	season := filepath.Join(e.tv, "Show (2020)", "Season.01")
	var want []AddedFile
	for _, w := range []struct {
		ep, id int
		size   int64
	}{{1, 1, 1000}, {2, 2, 2000}, {3, 3, 3000}} {
		name := EpisodeFilename("Show", 1, w.ep, "11111111")
		want = append(want, AddedFile{
			Path: filepath.Join(season, name), FusePath: "tv/Show (2020)/Season.01/" + name,
			Size: w.size, FileIndex: w.id, Season: 1, Episode: w.ep,
		})
	}
	if !reflect.DeepEqual(resp.Files, want) {
		t.Fatalf("files = %+v\nwant %+v", resp.Files, want)
	}
	// The sample lost to the full-size file of its episode; the nfo, the srt and the
	// unnumbered extra named no episode.
	if got := mkvs(t, e.tv); len(got) != 3 {
		t.Fatalf("tv dir = %v, want three stubs", got)
	}
	for _, f := range want {
		row := e.reg.rows[EpisodeKey("Show", 1, f.Episode)]
		if row.Hash != charPackHash || row.FilePath != f.Path || row.Source != "api" || row.QualityScore != 5 {
			t.Errorf("E%02d row = %+v", f.Episode, row)
		}
		if !strings.Contains(readFile(t, f.Path), jsonAmp("index="+string(rune('0'+f.FileIndex))+"&play")) {
			t.Errorf("E%02d stub points at the wrong file: %s", f.Episode, readFile(t, f.Path))
		}
	}
	if len(e.reg.rows) != 3 {
		t.Fatalf("registry rows = %d, want 3", len(e.reg.rows))
	}
}

func TestSeasonPackKeepsTheLargestFileWhenTwoNameTheSameEpisode(t *testing.T) {
	e := newCharEnv(t,
		FileStat{ID: 1, Path: "p/Show.S01E01.mkv", Length: 100},
		FileStat{ID: 2, Path: "p/Show.S01E01.PROPER.mkv", Length: 300},
		FileStat{ID: 3, Path: "p/Show.S01E01.sample.mkv", Length: 10},
	)
	resp, err := e.add(AddRequest{Type: "tv", Title: "Show", Hash: charPackHash, Season: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Files) != 1 || resp.Files[0].FileIndex != 2 {
		t.Fatalf("files = %+v, want the 300-byte file only", resp.Files)
	}
	if got := mkvs(t, e.tv); len(got) != 1 {
		t.Fatalf("tv dir = %v", got)
	}
}

func TestMultiSeasonPackFilesEachSeasonUnderItsOwnFolder(t *testing.T) {
	e := newCharEnv(t,
		FileStat{ID: 1, Path: "p/Show.S02E01.mkv", Length: 1},
		FileStat{ID: 2, Path: "p/Show.S01E02.mkv", Length: 1},
		FileStat{ID: 3, Path: "p/Show.S01E01.mkv", Length: 1},
	)
	resp, err := e.add(AddRequest{Type: "tv", Title: "Show", Hash: charPackHash, Season: 1})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range resp.Files {
		got = append(got, f.FusePath)
	}
	want := []string{
		"tv/Show/Season.01/Show_S01E01_11111111.mkv",
		"tv/Show/Season.01/Show_S01E02_11111111.mkv",
		"tv/Show/Season.02/Show_S02E01_11111111.mkv",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fuse paths = %v, want %v", got, want)
	}
}

// BEHAVIOUR THAT LOOKS LIKE A BUG, pinned as it is: a pack file named S00E01 (a
// special) parses to season 0, which addEpisodes reads as "no season in the name"
// and files under the requested season, where it collides with that season's E01
// and competes by size.
func TestSeasonPackSpecialsFallIntoTheRequestedSeason(t *testing.T) {
	e := newCharEnv(t, FileStat{ID: 1, Path: "p/Show.S00E01.Special.mkv", Length: 5})
	resp, err := e.add(AddRequest{Type: "tv", Title: "Show", Hash: charPackHash, Season: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Files) != 1 || resp.Files[0].Season != 3 || resp.Files[0].Episode != 1 ||
		resp.Files[0].FusePath != "tv/Show/Season.03/Show_S03E01_11111111.mkv" {
		t.Fatalf("files = %+v", resp.Files)
	}
}

func TestSeasonPackNamesEpisodesFromSceneNumbersAndFolders(t *testing.T) {
	e := newCharEnv(t,
		FileStat{ID: 1, Path: "p/sln-720p.ahs.806.mkv", Length: 1},
		FileStat{ID: 2, Path: "p/Show.S08E07.720p.HDTV/r4z3r-show.mkv", Length: 1},
	)
	resp, err := e.add(AddRequest{Type: "tv", Title: "Show", Hash: charPackHash, Season: 8})
	if err != nil {
		t.Fatal(err)
	}
	var got [][2]int
	for _, f := range resp.Files {
		got = append(got, [2]int{f.Season, f.Episode})
	}
	if !reflect.DeepEqual(got, [][2]int{{8, 6}, {8, 7}}) {
		t.Fatalf("episodes = %v", got)
	}
}

func TestSeasonPackWithNoNameableFileIsRefusedAndNothingIsWritten(t *testing.T) {
	e := newCharEnv(t,
		FileStat{ID: 1, Path: "p/movie-like.mkv", Length: 5},
		FileStat{ID: 2, Path: "p/Show.S01E01.nfo", Length: 5},
	)
	_, err := e.add(AddRequest{Type: "tv", Title: "Show", Hash: charPackHash, Season: 1})
	if statusOf(t, err) != http.StatusUnprocessableEntity {
		t.Fatalf("err = %v, want 422", err)
	}
	if got := mkvs(t, e.tv); len(got) != 0 || len(e.reg.rows) != 0 {
		t.Fatalf("stubs = %v, rows = %v", got, e.reg.rows)
	}
	if !reflect.DeepEqual(e.eng.removed, []string{charPackHash}) {
		t.Fatalf("removed = %v, want the new torrent dropped", e.eng.removed)
	}
}

func TestSeasonPackReaddKeepsOneStubPerEpisodeAtTheSamePaths(t *testing.T) {
	e := newCharEnv(t, seasonPack...)
	req := AddRequest{Type: "tv", Title: "Show", Hash: charPackHash, Season: 1}
	first, err := e.add(req)
	if err != nil {
		t.Fatal(err)
	}
	bytesBefore := map[string]string{}
	for _, f := range first.Files {
		bytesBefore[f.Path] = readFile(t, f.Path)
	}

	// A pack is never short-circuited as already present: it is filed again, in place.
	second, err := e.add(req)
	if err != nil {
		t.Fatal(err)
	}
	if second.AlreadyPresent || !reflect.DeepEqual(second.Files, first.Files) {
		t.Fatalf("second = %+v, first = %+v", second, first)
	}
	if got := mkvs(t, e.tv); len(got) != 3 || len(e.reg.rows) != 3 {
		t.Fatalf("stubs = %v, rows = %d, want 3 and 3", got, len(e.reg.rows))
	}
	for p, b := range bytesBefore {
		if readFile(t, p) != b {
			t.Errorf("%s changed on re-add", p)
		}
	}
	if e.eng.reads != 0 || len(e.eng.removed) != 0 {
		t.Fatalf("reads = %d, removed = %v, want none", e.eng.reads, e.eng.removed)
	}
}

// ---- replacement ----

// scatter files S01E01 from oldHash and S01E02 from charOtherHash, each its own stub.
func scatter(t *testing.T, e *charEnv) (stubs [2]string) {
	t.Helper()
	for i, h := range []string{oldHash, charOtherHash} {
		ep := i + 1
		p := filepath.Join(e.tv, "Show", "Season.01", EpisodeFilename("Show", 1, ep, h[:8]))
		if err := WriteStub(p, e.m.streamURL(h, 1), 500, "magnet:?xt=urn:btih:"+h, ""); err != nil {
			t.Fatal(err)
		}
		key := EpisodeKey("Show", 1, ep)
		e.reg.rows[key] = metadb.EpisodeEntry{EpisodeKey: key, Hash: h, FilePath: p, Source: "api", ShowIMDB: "tt0111111"}
		stubs[i] = p
	}
	return stubs
}

func TestPackReplacesScatteredEpisodesFromOtherReleases(t *testing.T) {
	e := newCharEnv(t,
		FileStat{ID: 1, Path: "Show.S01/Show.S01E01.mkv", Length: 1 << 20},
		FileStat{ID: 2, Path: "Show.S01/Show.S01E02.mkv", Length: 1 << 20},
		FileStat{ID: 3, Path: "Show.S01/Show.S01E03.mkv", Length: 1 << 20},
	)
	old := scatter(t, e)

	resp, err := e.add(AddRequest{Type: "tv", Title: "Show", Hash: charThirdHash, Season: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Files) != 3 {
		t.Fatalf("files = %d, want 3", len(resp.Files))
	}
	for _, p := range old {
		if exists(p) {
			t.Errorf("replaced stub %s still there", p)
		}
	}
	want := []string{
		"Show/Season.01/Show_S01E01_cafebabe.mkv",
		"Show/Season.01/Show_S01E02_cafebabe.mkv",
		"Show/Season.01/Show_S01E03_cafebabe.mkv",
	}
	if got := mkvs(t, e.tv); !reflect.DeepEqual(got, want) {
		t.Fatalf("tv dir = %v, want %v", got, want)
	}
	for ep := 1; ep <= 3; ep++ {
		row := e.reg.rows[EpisodeKey("Show", 1, ep)]
		wantPath := filepath.Join(e.tv, "Show", "Season.01", EpisodeFilename("Show", 1, ep, "cafebabe"))
		if row.Hash != charThirdHash || row.FilePath != wantPath {
			t.Errorf("E%02d row = %+v", ep, row)
		}
	}
	// The show id a previous sync stored survives the replacement.
	if got := e.reg.rows[EpisodeKey("Show", 1, 1)].ShowIMDB; got != "tt0111111" {
		t.Errorf("show imdb = %q, want it carried over", got)
	}
	if e.eng.reads != 2 {
		t.Errorf("reads = %d, want the replaced first episode and the last", e.eng.reads)
	}
	removed := append([]string(nil), e.eng.removed...)
	sort.Strings(removed)
	if !reflect.DeepEqual(removed, []string{oldHash, charOtherHash}) {
		t.Errorf("removed torrents = %v, want both replaced releases", removed)
	}
}

func TestSingleEpisodeReplacesOnlyItsOwnEpisode(t *testing.T) {
	e := newCharEnv(t, FileStat{ID: 1, Path: "Show.S01E02.mkv", Length: 1 << 20})
	old := scatter(t, e)

	resp, err := e.add(AddRequest{Type: "tv", Title: "Show", Hash: charThirdHash, Season: 1, Episode: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Files) != 1 || exists(old[1]) || !exists(old[0]) {
		t.Fatalf("files = %+v, E01 stub exists = %v, E02 stub exists = %v", resp.Files, exists(old[0]), exists(old[1]))
	}
	if e.reg.rows[EpisodeKey("Show", 1, 1)].Hash != oldHash || e.reg.rows[EpisodeKey("Show", 1, 2)].Hash != charThirdHash {
		t.Fatalf("rows = %+v", e.reg.rows)
	}
	if !reflect.DeepEqual(e.eng.removed, []string{charOtherHash}) {
		t.Fatalf("removed = %v, want only the replaced release", e.eng.removed)
	}
}

// ---- removal ----

func filedPack(t *testing.T) (*charEnv, []AddedFile) {
	t.Helper()
	e := newCharEnv(t,
		FileStat{ID: 1, Path: "p/Show.S01E01.mkv", Length: 10},
		FileStat{ID: 2, Path: "p/Show.S01E02.mkv", Length: 10},
	)
	resp, err := e.add(AddRequest{Type: "tv", Title: "Show", Hash: charPackHash, Season: 1})
	if err != nil {
		t.Fatal(err)
	}
	return e, resp.Files
}

func TestRemoveByPathAcceptsAbsoluteAndFusePathsAndKeepsTheSharedTorrent(t *testing.T) {
	e, files := filedPack(t)

	resp, err := e.m.Remove(context.Background(), RemoveRequest{Path: files[0].FusePath})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resp.Removed, []string{files[0].Path}) {
		t.Fatalf("removed = %v", resp.Removed)
	}
	if exists(files[0].Path) || !exists(files[1].Path) {
		t.Fatal("wrong stub removed")
	}
	if _, ok := e.reg.rows[EpisodeKey("Show", 1, 1)]; ok {
		t.Fatal("registry row of the removed episode kept")
	}
	if _, ok := e.reg.rows[EpisodeKey("Show", 1, 2)]; !ok {
		t.Fatal("registry row of the other episode gone")
	}
	if len(e.eng.removed) != 0 {
		t.Fatalf("torrent dropped while a stub still uses it: %v", e.eng.removed)
	}

	if _, err := e.m.Remove(context.Background(), RemoveRequest{Type: "tv", Path: files[1].Path}); err != nil {
		t.Fatal(err)
	}
	if len(e.reg.rows) != 0 || !reflect.DeepEqual(e.eng.removed, []string{charPackHash}) {
		t.Fatalf("rows = %v, removed = %v, want the torrent dropped with its last stub", e.reg.rows, e.eng.removed)
	}
}

func TestRemoveByHashRemovesEveryStubOfTheRelease(t *testing.T) {
	e, files := filedPack(t)
	// A movie stub of an unrelated release stays.
	other := filepath.Join(e.movies, "Other_2000_1080p_deadbeef.mkv")
	if err := WriteStub(other, e.m.streamURL(charOtherHash, 1), 1, "m", ""); err != nil {
		t.Fatal(err)
	}

	resp, err := e.m.Remove(context.Background(), RemoveRequest{Hash: strings.ToUpper(charPackHash)})
	if err != nil {
		t.Fatal(err)
	}
	got := append([]string(nil), resp.Removed...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{files[0].Path, files[1].Path}) {
		t.Fatalf("removed = %v", got)
	}
	if len(e.reg.rows) != 0 || !exists(other) {
		t.Fatalf("rows = %v, other stub exists = %v", e.reg.rows, exists(other))
	}
	if !reflect.DeepEqual(e.eng.removed, []string{charPackHash}) {
		t.Fatalf("removed torrents = %v, want exactly one drop", e.eng.removed)
	}
}

func TestRemoveMovieByHashUsesTheTailOfTheHash(t *testing.T) {
	e := newCharEnv(t, movieTorrent...)
	resp, err := e.add(AddRequest{Title: "Inception", Hash: charPackHash, Year: 2010})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Remove(context.Background(), RemoveRequest{Hash: charPackHash}); err != nil {
		t.Fatal(err)
	}
	if exists(resp.Files[0].Path) || !reflect.DeepEqual(e.eng.removed, []string{charPackHash}) {
		t.Fatalf("stub exists = %v, removed = %v", exists(resp.Files[0].Path), e.eng.removed)
	}
}

func TestRemoveRefusals(t *testing.T) {
	e, files := filedPack(t)
	outside := filepath.Join(t.TempDir(), "x.mkv")
	if err := os.WriteFile(outside, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	notAStub := filepath.Join(e.movies, "notes.txt")
	if err := os.MkdirAll(e.movies, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notAStub, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name   string
		req    RemoveRequest
		status int
	}{
		{"absolute path outside the media dirs", RemoveRequest{Path: outside}, 400},
		{"traversal out of the tv dir", RemoveRequest{Path: filepath.Join(e.tv, "..", filepath.Base(filepath.Dir(outside)), "x.mkv")}, 400},
		{"relative traversal", RemoveRequest{Path: "../../etc/passwd"}, 400},
		{"the media directory itself", RemoveRequest{Path: e.tv}, 400},
		{"a file that is not a stub", RemoveRequest{Path: notAStub}, 400},
		{"a missing stub inside the tv dir", RemoveRequest{Path: filepath.Join(e.tv, "Nope", "x.mkv")}, 404},
		{"a hash no stub carries", RemoveRequest{Hash: charThirdHash}, 404},
		{"a malformed hash", RemoveRequest{Hash: "xyz"}, 400},
		{"neither path nor hash", RemoveRequest{}, 400},
		{"an unknown type", RemoveRequest{Type: "game", Path: files[0].Path}, 400},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := e.m.Remove(context.Background(), c.req)
			if statusOf(t, err) != c.status {
				t.Fatalf("err = %v, want status %d", err, c.status)
			}
		})
	}
	if !exists(outside) || !exists(files[0].Path) || !exists(files[1].Path) || len(e.reg.rows) != 2 || len(e.eng.removed) != 0 {
		t.Fatal("a refused removal changed something")
	}
}

func TestRemoveWithBlacklistRecordsPathAndHash(t *testing.T) {
	e, files := filedPack(t)
	var gotPath, gotHash string
	e.m.cfg.Blacklist = func(path, hash string) error { gotPath, gotHash = path, hash; return nil }

	if _, err := e.m.Remove(context.Background(), RemoveRequest{Path: files[0].Path, Blacklist: true}); err != nil {
		t.Fatal(err)
	}
	if gotPath != files[0].Path || gotHash != charPackHash {
		t.Fatalf("blacklist got %q, %q", gotPath, gotHash)
	}

	// Without a configured blacklist the removal still happens and says so.
	e2, files2 := filedPack(t)
	resp, err := e2.m.Remove(context.Background(), RemoveRequest{Path: files2[0].Path, Blacklist: true})
	if err != nil || len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], "no blacklist is configured") {
		t.Fatalf("resp = %+v, err = %v", resp, err)
	}
}

func TestListReportsStubsWithTheirParsedFields(t *testing.T) {
	e, files := filedPack(t)
	items, err := e.m.List("tv")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %+v", items)
	}
	// The stub's mtime orders the catch-up feeder; it is not part of the answer.
	mt := items[0].ModTime
	if st, err := os.Stat(files[0].Path); err != nil || !mt.Equal(st.ModTime()) || mt.IsZero() {
		t.Fatalf("ModTime = %v, want the stub's mtime (stat err %v)", mt, err)
	}
	want := Item{Path: files[0].Path, FusePath: files[0].FusePath, Size: 10, Hash: charPackHash, FileIndex: 1, Season: 1, Episode: 1, ModTime: mt}
	if items[0] != want {
		t.Fatalf("item = %+v, want %+v", items[0], want)
	}
	b, err := json.Marshal(items[0])
	if err != nil || strings.Contains(string(b), "ModTime") || strings.Contains(strings.ToLower(string(b)), "mod_time") {
		t.Fatalf("the API output must not change, got %s (err %v)", b, err)
	}
}

// ---- matching by the stub's own link ----

// legacyStub writes a stub under dir with the given file name whose link points at hash,
// as the older tools did: the name carries a tag of the hash, the link the whole of it.
func (e *charEnv) legacyStub(t *testing.T, path, hash string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteStub(path, e.m.streamURL(hash, 1), 1, "m", ""); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFindByHashMatchesLegacyStubsOfBothTagsByTheirLink(t *testing.T) {
	e := newCharEnv(t)
	// charPackHash: first 8 = 11111111, last 8 = 55555555. Each kind of folder holds
	// stubs tagged either way, and a name that carries no tag at all.
	want := []string{
		e.legacyStub(t, filepath.Join(e.movies, "A_2000_1080p_55555555.mkv"), charPackHash),
		e.legacyStub(t, filepath.Join(e.movies, "B_2000_1080p_11111111.mkv"), charPackHash),
		e.legacyStub(t, filepath.Join(e.tv, "Show", "Season.01", "Show_S01E01_1080p_11111111.mkv"), charPackHash),
		e.legacyStub(t, filepath.Join(e.tv, "Show", "Season.01", "Show_S01E02_1080p_55555555.mkv"), charPackHash),
		e.legacyStub(t, filepath.Join(e.movies, "C_2000_1080p_untagged.mkv"), strings.ToUpper(charPackHash)),
	}
	for _, kind := range []string{"movie", "tv"} {
		found, err := e.m.findByHash(kind, charPackHash)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, f := range found {
			got = append(got, f.Path)
		}
		var exp []string
		for _, p := range want {
			if strings.HasPrefix(p, map[string]string{"movie": e.movies, "tv": e.tv}[kind]) {
				exp = append(exp, p)
			}
		}
		sort.Strings(got)
		sort.Strings(exp)
		if !reflect.DeepEqual(got, exp) {
			t.Errorf("%s: found %v, want %v", kind, got, exp)
		}
	}
}

func TestRemoveByHashSparesAStubOfAnotherTorrentWhoseNameSharesTheTag(t *testing.T) {
	e := newCharEnv(t)
	mine := e.legacyStub(t, filepath.Join(e.movies, "Mine_2000_1080p_55555555.mkv"), charPackHash)
	// Another torrent whose stub name carries the same eight digits.
	foreign := e.legacyStub(t, filepath.Join(e.movies, "Foreign_2000_1080p_55555555.mkv"), charOtherHash)
	foreignEp := e.legacyStub(t, filepath.Join(e.tv, "Show", "Season.01", "Show_S01E01_1080p_11111111.mkv"), charOtherHash)

	resp, err := e.m.Remove(context.Background(), RemoveRequest{Hash: charPackHash})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resp.Removed, []string{mine}) {
		t.Fatalf("removed = %v, want only %s", resp.Removed, mine)
	}
	if !exists(foreign) || !exists(foreignEp) {
		t.Fatal("a stub of another torrent was deleted on a name match")
	}
	if !reflect.DeepEqual(e.eng.removed, []string{charPackHash}) {
		t.Fatalf("removed torrents = %v, want only the one asked for", e.eng.removed)
	}
}

func TestRemoveByHashFindsAStubWhoseNameCarriesTheOtherTag(t *testing.T) {
	e := newCharEnv(t)
	movie := e.legacyStub(t, filepath.Join(e.movies, "Old_2000_1080p_11111111.mkv"), charPackHash)
	ep := e.legacyStub(t, filepath.Join(e.tv, "Show", "Season.01", "Show_S01E01_1080p_55555555.mkv"), charPackHash)

	resp, err := e.m.Remove(context.Background(), RemoveRequest{Hash: strings.ToUpper(charPackHash)})
	if err != nil {
		t.Fatal(err)
	}
	got := append([]string(nil), resp.Removed...)
	sort.Strings(got)
	want := []string{ep, movie}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("removed = %v, want %v", got, want)
	}
}

func TestAlreadyPresentUsesTheStubsLink(t *testing.T) {
	e := newCharEnv(t)
	// A movie stub of this torrent under a name that carries neither tag of it.
	mine := e.legacyStub(t, filepath.Join(e.movies, "Mine_2000_1080p_cafe0000.mkv"), charPackHash)
	// A stub of another torrent that carries this torrent's last eight digits.
	e.legacyStub(t, filepath.Join(e.movies, "Foreign_2000_1080p_55555555.mkv"), charOtherHash)

	found, err := e.m.alreadyPresent("movie", charPackHash, AddRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Path != mine {
		t.Fatalf("movie already present = %+v, want only %s", found, mine)
	}

	// A single episode: the stub at the expected path belongs to another torrent.
	req := AddRequest{Type: "tv", Title: "Show", Season: 1, Episode: 1}
	path := filepath.Join(e.tv, ShowFolderName(req.Title, req.FirstAirDate), "Season.01",
		EpisodeFilename(req.Title, 1, 1, HashPrefix(charPackHash)))
	e.legacyStub(t, path, charOtherHash)
	found, err = e.m.alreadyPresent("tv", charPackHash, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("episode already present = %+v, want none: the stub links to another torrent", found)
	}
	e.legacyStub(t, path, charPackHash)
	if found, err = e.m.alreadyPresent("tv", charPackHash, req); err != nil || len(found) != 1 {
		t.Fatalf("episode already present = %+v, %v, want the stub of this torrent", found, err)
	}
}

func TestDropTorrentIfUnusedChecksReferencesByLink(t *testing.T) {
	// A stub of another torrent that shares the name tag does not keep this one.
	e := newCharEnv(t)
	e.legacyStub(t, filepath.Join(e.movies, "Foreign_2000_1080p_55555555.mkv"), charOtherHash)
	if !e.m.dropTorrentIfUnused(context.Background(), charPackHash) {
		t.Fatal("torrent kept alive by a stub of another torrent")
	}

	// A legacy stub that links to it under any name does.
	e = newCharEnv(t)
	e.legacyStub(t, filepath.Join(e.movies, "Mine_2000_1080p_cafe0000.mkv"), charPackHash)
	if e.m.dropTorrentIfUnused(context.Background(), charPackHash) || len(e.eng.removed) != 0 {
		t.Fatalf("torrent dropped while a stub links to it: %v", e.eng.removed)
	}
}

func TestFindByHashSkipsAMalformedStubAndLogsOnce(t *testing.T) {
	e := newCharEnv(t)
	var buf bytes.Buffer
	e.m.cfg.Logger = log.New(&buf, "", 0)
	good := e.legacyStub(t, filepath.Join(e.movies, "Good_2000_1080p_55555555.mkv"), charPackHash)
	for _, name := range []string{"Bad1_2000_1080p_55555555.mkv", "Bad2_2000_1080p_55555555.mkv"} {
		if err := os.WriteFile(filepath.Join(e.movies, name), []byte("{ not json"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	found, err := e.m.findByHash("movie", charPackHash)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Path != good {
		t.Fatalf("found = %+v, want only the good stub", found)
	}
	if n := strings.Count(buf.String(), "\n"); n != 1 || !strings.Contains(buf.String(), "skipped 2 stub(s)") {
		t.Fatalf("log = %q, want one line naming 2 skipped stubs", buf.String())
	}
}
