package library

import "testing"

// Characterization tests: they freeze what the naming helpers do today, so a
// later restructuring cannot change a stub name unnoticed. A stub renamed by a
// refactor would be filed a second time and the old one orphaned.

// charHash has distinct first and last 8 chars: movies use the tail, episodes the head.
const charHash = "1111111122222222333333334444444455555555"

func TestMovieFilenameFromTitleYearAndHash(t *testing.T) {
	for _, c := range []struct {
		name string
		in   MovieName
		want string
	}{
		{"plain title and release date",
			MovieName{Title: "Inception", ReleaseDate: "2010-07-16", ReleaseTitle: "Inception.2010.1080p.BluRay", Hash: charHash},
			"Inception_2010_1080p_55555555.mkv"},
		{"year-only release date",
			MovieName{Title: "Inception", ReleaseDate: "2010", Hash: charHash},
			"Inception_2010_1080p_55555555.mkv"},
		{"colon and spaces collapse to one underscore",
			MovieName{Title: "Spider-Man: No Way Home", ReleaseDate: "2021-12-15", Hash: charHash},
			"Spider-Man_No_Way_Home_2021_1080p_55555555.mkv"},
		{"slash is replaced",
			MovieName{Title: "AC/DC: Live at River Plate", ReleaseDate: "2014", Hash: charHash},
			"AC_DC_Live_at_River_Plate_2014_1080p_55555555.mkv"},
		{"dots are kept, other punctuation becomes an underscore",
			MovieName{Title: "...And Justice for All!", ReleaseDate: "1979", Hash: charHash},
			"...And_Justice_for_All_1979_1080p_55555555.mkv"},
		{"non-ASCII letters become underscores",
			MovieName{Title: "Amélie", ReleaseDate: "2001", Hash: charHash},
			"Am_lie_2001_1080p_55555555.mkv"},
		{"only non-ASCII letters leave an empty base: the hash prefix names it, with no leading underscore",
			MovieName{Title: "千と千尋の神隠し", ReleaseDate: "2001", Hash: charHash},
			"11111111_2001_1080p_55555555.mkv"},
		{"no year anywhere",
			MovieName{Title: "Untitled", Hash: charHash},
			"Untitled_1080p_55555555.mkv"},
		{"date shorter than four chars gives no year",
			MovieName{Title: "Untitled", ReleaseDate: "202", Hash: charHash},
			"Untitled_1080p_55555555.mkv"},
		{"a parenthesised year in the title stays part of the title and is not read twice",
			MovieName{Title: "Dune (2021)", Hash: charHash},
			"Dune_2021_1080p_55555555.mkv"},
		{"a year-like number in the title is part of the title, not the release year",
			MovieName{Title: "Blade Runner 2049", Hash: charHash},
			"Blade_Runner_2049_1080p_55555555.mkv"},
		{"an explicit release date still adds its year after such a title",
			MovieName{Title: "Blade Runner 2049", ReleaseDate: "2017-10-04", Hash: charHash},
			"Blade_Runner_2049_2017_1080p_55555555.mkv"},
		{"release date wins over a year in the title",
			MovieName{Title: "Dune (2021)", ReleaseDate: "1984-12-14", Hash: charHash},
			"Dune_2021_1984_1080p_55555555.mkv"},
		{"short hash is used whole",
			MovieName{Title: "Short", ReleaseDate: "2000", Hash: "abc"},
			"Short_2000_1080p_abc.mkv"},
		{"eight-char hash is used whole",
			MovieName{Title: "Short", ReleaseDate: "2000", Hash: "abcdefgh"},
			"Short_2000_1080p_abcdefgh.mkv"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := BuildMovieFilename(c.in); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestMovieFilenameLongTitleIsNotTruncated(t *testing.T) {
	long := ""
	for i := 0; i < 30; i++ {
		long += "Long Title "
	}
	got := BuildMovieFilename(MovieName{Title: long, ReleaseDate: "2000", Hash: charHash})
	// 30 * "Long_Title_" less the trailing underscore, then _2000_1080p_55555555.mkv.
	if want := 30*len("Long_Title_") - 1 + len("_2000_1080p_55555555.mkv"); len(got) != want {
		t.Fatalf("len = %d, want %d: %q", len(got), want, got)
	}
}

func TestMovieFilenameQualityTagsFromReleaseTitle(t *testing.T) {
	for _, c := range []struct {
		name    string
		release string
		is4K    bool
		want    string
	}{
		{"1080p by default", "Movie.2020.1080p.WEB-DL", false, "Movie_2020_1080p_55555555.mkv"},
		{"2160p from the flag", "Movie.2020.WEB-DL", true, "Movie_2020_2160p_55555555.mkv"},
		{"the flag alone decides resolution, not the release title", "Movie.2020.2160p.WEB-DL", false, "Movie_2020_1080p_55555555.mkv"},
		{"HDR", "Movie.2020.2160p.HDR.WEB-DL", true, "Movie_2020_2160p_HDR_55555555.mkv"},
		{"HDR10+", "Movie.2020.2160p.HDR10+.WEB-DL", true, "Movie_2020_2160p_HDR_55555555.mkv"},
		{"Dolby Vision beats HDR", "Movie.2020.2160p.DV.HDR.WEB-DL", true, "Movie_2020_2160p_DV_55555555.mkv"},
		{"Dolby Vision spelled out", "Movie 2020 2160p Dolby Vision", true, "Movie_2020_2160p_DV_55555555.mkv"},
		{"DoVi", "Movie.2020.2160p.DoVi", true, "Movie_2020_2160p_DV_55555555.mkv"},
		{"Atmos", "Movie.2020.1080p.TrueHD.Atmos", false, "Movie_2020_1080p_Atmos_55555555.mkv"},
		{"Atmos beats 5.1", "Movie.2020.1080p.DDP5.1.Atmos", false, "Movie_2020_1080p_Atmos_55555555.mkv"},
		{"5.1 from DTS", "Movie.2020.1080p.DTS-HD", false, "Movie_2020_1080p_5.1_55555555.mkv"},
		{"5.1 from DD+", "Movie.2020.1080p.DD+.x264", false, "Movie_2020_1080p_5.1_55555555.mkv"},
		{"REMUX", "Movie.2020.1080p.BluRay.REMUX.AVC", false, "Movie_2020_1080p_REMUX_55555555.mkv"},
		{"BDRemux without a separator", "Movie.2020.1080p.BDRemux", false, "Movie_2020_1080p_REMUX_55555555.mkv"},
		{"all tags in order", "Movie.2020.2160p.UHD.BluRay.REMUX.DV.HDR10.TrueHD.Atmos", true,
			"Movie_2020_2160p_DV_Atmos_REMUX_55555555.mkv"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := BuildMovieFilename(MovieName{Title: "Movie", ReleaseDate: "2020", ReleaseTitle: c.release, Is4K: c.is4K, Hash: charHash})
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestResolutionDetectionFromReleaseTitle(t *testing.T) {
	for release, want := range map[string]bool{
		"Movie.2020.2160p.WEB-DL": true,
		"Movie.2020.UHD.BluRay":   true,
		"Movie 2020 4K HDR":       true,
		"Movie.2020.4k.WEB":       true,
		"Movie.2020.1080p.WEB-DL": false,
		"Movie.2020.720p":         false,
		"Movie.2020.UHDRip":       false,
		"Movie.2020.24K.Gold":     false,
		"":                        false,
	} {
		if got := reIs4K.MatchString(release); got != want {
			t.Errorf("%q: 4K = %v, want %v", release, got, want)
		}
	}
}

func TestHashAffixes(t *testing.T) {
	if got := HashSuffix(charHash); got != "55555555" {
		t.Errorf("suffix = %q", got)
	}
	if got := HashPrefix(charHash); got != "11111111" {
		t.Errorf("prefix = %q", got)
	}
	if HashSuffix("abc") != "abc" || HashPrefix("abc") != "abc" {
		t.Error("a short hash must come back whole")
	}
}

func TestEpisodeNaming(t *testing.T) {
	for _, c := range []struct {
		name               string
		show, airDate      string
		season, episode    int
		wantFolder, wantEp string
		wantKey            string
	}{
		{"plain show with air date", "Severance", "2022-02-18", 1, 2,
			"Severance (2022)", "Severance_S01E02_11111111.mkv", "severance_s01e02"},
		{"no air date, no year in the folder", "Severance", "", 1, 2,
			"Severance", "Severance_S01E02_11111111.mkv", "severance_s01e02"},
		{"air date shorter than four chars is ignored", "Severance", "202", 1, 2,
			"Severance", "Severance_S01E02_11111111.mkv", "severance_s01e02"},
		{"spaces become underscores", "The Last of Us", "2023-01-15", 2, 10,
			"The_Last_of_Us (2023)", "The_Last_of_Us_S02E10_11111111.mkv", "thelastofus_s02e10"},
		{"apostrophe, dots and colon", "Marvel's Agents of S.H.I.E.L.D.", "2013-09-24", 3, 4,
			"Marvels_Agents_of_S.H.I.E.L.D. (2013)", "Marvels_Agents_of_S.H.I.E.L.D._S03E04_11111111.mkv", "marvelsagentsofshield_s03e04"},
		{"colon and slash are removed, not replaced", "Show: Name/Part", "", 1, 1,
			"Show_NamePart", "Show_NamePart_S01E01_11111111.mkv", "shownamepart_s01e01"},
		{"ampersand is removed", "Tom & Jerry", "", 1, 1,
			"Tom_Jerry", "Tom_Jerry_S01E01_11111111.mkv", "tomjerry_s01e01"},
		{"non-ASCII letters survive in the path and fold to their base letter in the key", "Élite", "2018-10-05", 1, 1,
			"Élite (2018)", "Élite_S01E01_11111111.mkv", "elite_s01e01"},
		{"specials: season 0 and episode 0 format as two digits", "Doctor Who", "", 0, 0,
			"Doctor_Who", "Doctor_Who_S00E00_11111111.mkv", "doctorwho_s00e00"},
		{"three-digit episodes keep all digits", "One Piece", "1999-10-20", 21, 1015,
			"One_Piece (1999)", "One_Piece_S21E1015_11111111.mkv", "onepiece_s21e1015"},
		{"a name of dots alone cannot escape the directory", "..", "", 1, 1,
			"untitled", "untitled_S01E01_11111111.mkv", "_s01e01"},
		{"an empty name becomes untitled", "", "", 1, 1,
			"untitled", "untitled_S01E01_11111111.mkv", "_s01e01"},
		{"leading dots are stripped", "..Hidden", "", 1, 1,
			"Hidden", "Hidden_S01E01_11111111.mkv", "hidden_s01e01"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ShowFolderName(c.show, c.airDate); got != c.wantFolder {
				t.Errorf("folder = %q, want %q", got, c.wantFolder)
			}
			if got := EpisodeFilename(c.show, c.season, c.episode, HashPrefix(charHash)); got != c.wantEp {
				t.Errorf("filename = %q, want %q", got, c.wantEp)
			}
			if got := EpisodeKey(c.show, c.season, c.episode); got != c.wantKey {
				t.Errorf("key = %q, want %q", got, c.wantKey)
			}
		})
	}
}

func TestShowNameRoundTripsFromEpisodePath(t *testing.T) {
	for path, want := range map[string]string{
		"/tv/The_Last_of_Us (2023)/Season.02/The_Last_of_Us_S02E10_11111111.mkv": "The Last of Us",
		"/tv/Severance/Season.01/Severance_S01E02_11111111.mkv":                  "Severance",

		"/tv/Show/Season 1/x.mkv": "Show",
		"/tv/Show/Specials/x.mkv": "",
		"/Season.01/x.mkv":        "",
	} {
		if got := ShowNameFromEpisodePath(path); got != want {
			t.Errorf("%s: got %q, want %q", path, got, want)
		}
	}
}

func TestVideoFileExtensions(t *testing.T) {
	for name, want := range map[string]bool{
		"a.mkv": true, "a.MP4": true, "a.avi": true, "a.mov": true, "a.m4v": true,
		"a.srt": false, "a.nfo": false, "a.ts": false, "a.mkv.part": false, "mkv": false,
	} {
		if got := IsVideoFile(name); got != want {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
}

func TestMagnetHelpers(t *testing.T) {
	got := BuildMagnet(charHash, "Some Title & More", []string{"udp://t.example:80/announce"})
	want := "magnet:?xt=urn:btih:" + charHash + "&dn=Some+Title+%26+More&tr=udp%3A%2F%2Ft.example%3A80%2Fannounce"
	if got != want {
		t.Fatalf("magnet = %q, want %q", got, want)
	}
	if got := BuildMagnet(charHash, "", nil); got != "magnet:?xt=urn:btih:"+charHash {
		t.Fatalf("bare magnet = %q", got)
	}
	if got := HashFromMagnet("magnet:?xt=urn:btih:ABCDEF0123456789ABCDEF0123456789ABCDEF01&dn=x"); got != "abcdef0123456789abcdef0123456789abcdef01" {
		t.Fatalf("hash from magnet = %q", got)
	}
	if got := HashFromMagnet("magnet:?dn=nothing"); got != "" {
		t.Fatalf("hash from a magnet without one = %q", got)
	}
}

// The key folds Latin diacritics to their base letters so an accented title
// neither loses a letter nor shares a key with a different title. The old form
// stays available as legacyEpisodeKey: rows already registered carry it.
func TestEpisodeKeyFoldsLatinLettersAndKeepsTheLegacyForm(t *testing.T) {
	for _, c := range []struct {
		show, key, legacy string
	}{
		{"Élite", "elite_s01e01", "lite_s01e01"},
		{"Lite", "lite_s01e01", "lite_s01e01"},
		{"Ångström", "angstrom_s01e01", "ngstrm_s01e01"},
		{"Łódź Straße", "lodzstrasse_s01e01", "dstrae_s01e01"},
		{"Œuvre Æon", "oeuvreaeon_s01e01", "uvreon_s01e01"},
		{"Plain Show", "plainshow_s01e01", "plainshow_s01e01"},
		{"千と千尋", "_s01e01", "_s01e01"},
	} {
		if got := EpisodeKey(c.show, 1, 1); got != c.key {
			t.Errorf("%q: key = %q, want %q", c.show, got, c.key)
		}
		if got := legacyEpisodeKey(c.show, 1, 1); got != c.legacy {
			t.Errorf("%q: legacy key = %q, want %q", c.show, got, c.legacy)
		}
	}
}
