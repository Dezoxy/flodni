// Package library holds the stub-file format and filename conventions used by the
// external /api/library endpoints. They must agree with the stubs already on disk: a
// stub created under a different name would be a duplicate of the same title, and a
// TV episode filed under a different registry key is treated as orphaned.
package library

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"flodni/internal/atomicfile"
)

var (
	reHDR   = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])hdr(?:$|[^A-Za-z0-9])|hdr10\+?`)
	reDV    = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])dv(?:$|[^A-Za-z0-9])|dovi|dolby.?vision`)
	reAtmos = regexp.MustCompile(`(?i)atmos`)
	re51    = regexp.MustCompile(`(?i)5\.1|dts|ddp5|ddp|dd\+|eac3|ac3`)
	// No separator required before the word: "BDRemux" and "UHDRemux" are common
	// spellings, and requiring one dropped the tag from names that announce it.
	reRemux = regexp.MustCompile(`(?i)remux(?:$|[^A-Za-z0-9])`)

	reMovieUnsafe = regexp.MustCompile(`[^a-zA-Z0-9._-]`)
	reUnderscores = regexp.MustCompile(`_+`)
	reShowUnsafe  = regexp.MustCompile(`[<>:"/\\|?*'"&]`)
	reSpaces      = regexp.MustCompile(`\s+`)
	reNonWord     = regexp.MustCompile(`[^a-z0-9]`)
	reEpSxxExx    = regexp.MustCompile(`[Ss](\d+)[ ._-]?[Ee](\d+)`) // Dezoxy fork: also s04.e01
	reEpNxN       = regexp.MustCompile(`(\d+)x(\d+)`)
)

// MovieName is everything the movie filename is built from. ReleaseTitle is the raw
// release name the quality tags are read out of; Hash is the full info hash.
type MovieName struct {
	Title        string
	ReleaseDate  string
	ReleaseTitle string
	Is4K         bool
	Hash         string
}

func SanitizeMovieName(s string) string {
	s = reMovieUnsafe.ReplaceAllString(s, "_")
	s = reUnderscores.ReplaceAllString(s, "_")
	return strings.Trim(s, "_")
}

// BuildMovieFilename returns the stub name for a movie. The trailing 8 chars are the
// tail of the info hash: that suffix is how an already-present title is recognised.
func BuildMovieFilename(n MovieName) string {
	// The year is the release date's, when the request carries one. A number in the
	// title ("Blade Runner 2049", "Dune (2021)") belongs to the title as given and is
	// not read a second time as the release year.
	year := ""
	if len(n.ReleaseDate) >= 4 {
		year = n.ReleaseDate[:4]
	}

	base := SanitizeMovieName(n.Title)
	if base == "" {
		// No ASCII letter or digit survives (a title in another script): the hash
		// prefix keeps the name non-empty, without a leading underscore, and stable.
		if base = HashPrefix(n.Hash); base == "" {
			base = "untitled"
		}
	}
	if year != "" {
		base = fmt.Sprintf("%s_%s", base, year)
	}

	if n.Is4K {
		base += "_2160p"
	} else {
		base += "_1080p"
	}

	if reDV.MatchString(n.ReleaseTitle) {
		base += "_DV"
	} else if reHDR.MatchString(n.ReleaseTitle) {
		base += "_HDR"
	}

	if reAtmos.MatchString(n.ReleaseTitle) {
		base += "_Atmos"
	} else if re51.MatchString(n.ReleaseTitle) {
		base += "_5.1"
	}

	if reRemux.MatchString(n.ReleaseTitle) {
		base += "_REMUX"
	}

	return fmt.Sprintf("%s_%s.mkv", base, HashSuffix(n.Hash))
}

// HashSuffix is the last 8 hash chars used in movie filenames.
func HashSuffix(hash string) string {
	if len(hash) <= 8 {
		return hash
	}
	return hash[len(hash)-8:]
}

// HashPrefix is the first 8 hash chars used in episode filenames.
func HashPrefix(hash string) string {
	if len(hash) <= 8 {
		return hash
	}
	return hash[:8]
}

// SanitizeShowName turns a show name into one safe directory component. Titles reach
// here from HTTP requests, so a name made of dots must not survive as "." or "..".
func SanitizeShowName(name string) string {
	clean := reShowUnsafe.ReplaceAllString(name, "")
	clean = reSpaces.ReplaceAllString(clean, "_")
	clean = reUnderscores.ReplaceAllString(clean, "_")
	clean = strings.Trim(clean, "_")
	clean = strings.TrimLeft(clean, ".")
	if clean == "" {
		return "untitled"
	}
	return clean
}

func ShowFolderName(showName, firstAirDate string) string {
	clean := SanitizeShowName(showName)
	if len(firstAirDate) >= 4 {
		return fmt.Sprintf("%s (%s)", clean, firstAirDate[:4])
	}
	return clean
}

func EpisodeFilename(show string, season, episode int, hash8 string) string {
	return fmt.Sprintf("%s_S%02dE%02d_%s.mkv", SanitizeShowName(show), season, episode, hash8)
}

// EpisodeKey is the TV registry key. The sync deletes every stub under the TV dir whose
// path is not registered, so an API-created episode must use this exact form.
//
// Latin letters with diacritics fold to their base letters (Élite gives "elite"), so an
// accented letter neither vanishes nor lets two titles share a key. Keys registered
// before this folded are still read through legacyEpisodeKey.
func EpisodeKey(show string, season, episode int) string {
	return fmt.Sprintf("%s_s%02de%02d", reNonWord.ReplaceAllString(foldLatin(strings.ToLower(show)), ""), season, episode)
}

// latinFold is a persisted key format: it decides the registry key of every accented
// show, so editing an entry silently re-keys shows already filed (the legacy alias
// covers only the form from before folding existed).
// latinFold maps lower-case Latin-1 and Latin Extended-A letters to ASCII. A table, not
// golang.org/x/text: the module is only an indirect dependency, and NFD would still
// leave ø, ł, đ, ß and the ligatures.
var latinFold = func() map[rune]string {
	m := map[rune]string{}
	for folded, letters := range map[string]string{
		"a": "àáâãäåāăą", "ae": "æ", "c": "çćĉċč", "d": "ďđð", "e": "èéêëēĕėęě",
		"g": "ĝğġģ", "h": "ĥħ", "i": "ìíîïĩīĭįı", "ij": "ĳ", "j": "ĵ", "k": "ķĸ",
		"l": "ĺļľŀł", "n": "ñńņňŉ", "ng": "ŋ", "o": "òóôõöøōŏő", "oe": "œ",
		"r": "ŕŗř", "s": "śŝşš", "ss": "ß", "t": "ţťŧ", "th": "þ",
		"u": "ùúûüũūŭůűų", "w": "ŵ", "y": "ýÿŷ", "z": "źżž",
	} {
		for _, r := range letters {
			m[r] = folded
		}
	}
	return m
}()

func foldLatin(s string) string {
	var b strings.Builder
	for _, r := range s {
		if f, ok := latinFold[r]; ok {
			b.WriteString(f)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// legacyEpisodeKey is the key form EpisodeKey produced before accents were folded:
// letters outside a-z dropped. Rows registered under it are still read as an alias.
func legacyEpisodeKey(show string, season, episode int) string {
	return fmt.Sprintf("%s_s%02de%02d", reNonWord.ReplaceAllString(strings.ToLower(show), ""), season, episode)
}

// ParseSeasonEpisode reads S01E05 (also s01.e05) or 1x05 out of a filename, returning
// 0,0 when the name carries neither.
func ParseSeasonEpisode(filename string) (int, int) {
	for _, re := range []*regexp.Regexp{reEpSxxExx, reEpNxN} {
		if m := re.FindStringSubmatch(filename); len(m) >= 3 {
			season, _ := strconv.Atoi(m[1])
			episode, _ := strconv.Atoi(m[2])
			return season, episode
		}
	}
	return 0, 0
}

// Dezoxy fork: scene packs number an episode 806 (season 8, episode 6), glue that number
// to the show's abbreviation (ttscc109), write 01e02 without the S, or keep S05E01 only in
// the episode's folder name.
var (
	reNameSeparators = regexp.MustCompile(`[._ -]+`)
	// 01e02: the season without its S (FULCRUM).
	reBareSeasonEpisode = regexp.MustCompile(`(?i)(?:^|[._ -])(\d{1,2})e(\d{1,3})(?:[._ -]|$)`)
	// ttscc109: a scene number glued to the show's abbreviation (NoScEnE).
	reGluedSceneNumber = regexp.MustCompile(`(?i)^[a-z]{2,8}(\d{3,4})$`)
	reCodecToken       = regexp.MustCompile(`(?i)^(x|h)26[45]$`)
)

// PackEpisode names a file of a season pack: from its own name, then from its folder's,
// then from a scene number whose leading digits are the pack's season. A name with two
// different such numbers is left unnamed.
func PackEpisode(path string, packSeason int) (int, int) {
	if season, episode := ParseSeasonEpisode(filepath.Base(path)); episode > 0 {
		return season, episode
	}
	if dir := filepath.Base(filepath.Dir(path)); dir != "." && dir != string(filepath.Separator) {
		if season, episode := ParseSeasonEpisode(dir); episode > 0 {
			return season, episode
		}
	}
	if packSeason <= 0 {
		return 0, 0
	}
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if m := reBareSeasonEpisode.FindStringSubmatch(name); m != nil {
		season, _ := strconv.Atoi(m[1])
		episode, _ := strconv.Atoi(m[2])
		if season == packSeason && episode > 0 {
			return season, episode
		}
	}
	found := 0
	for _, token := range reNameSeparators.Split(name, -1) {
		if m := reGluedSceneNumber.FindStringSubmatch(token); m != nil && !reCodecToken.MatchString(token) {
			token = m[1]
		}
		if len(token) < 3 || len(token) > 4 || strings.Trim(token, "0123456789") != "" {
			continue
		}
		n, _ := strconv.Atoi(token)
		if n/100 != packSeason || n%100 == 0 {
			continue
		}
		if found != 0 && found != n%100 {
			return 0, 0
		}
		found = n % 100
	}
	if found == 0 {
		return 0, 0
	}
	return packSeason, found
}

func IsVideoFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mkv", ".mp4", ".avi", ".mov", ".m4v":
		return true
	}
	return false
}

// WriteStub writes the virtual .mkv: a small JSON file the FUSE layer exposes at the
// declared size.
func WriteStub(path, streamURL string, size int64, magnet, imdbID string) error {
	data, err := json.Marshal(map[string]interface{}{
		"url":    streamURL,
		"size":   size,
		"magnet": magnet,
		"imdb":   imdbID,
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return atomicfile.WriteFile(path, data, 0644)
}
