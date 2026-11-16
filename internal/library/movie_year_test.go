package library

import (
	"strings"
	"testing"
)

// The year in a movie stub name comes from the caller's release_date. Only four ASCII
// digits are a year: anything else is dropped, so the name stays one path component.
func TestMovieFilenameYearIsFourDigitsOrNothing(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		date, wantYear string
	}{
		{"2019-12-13", "2019"},
		{"2019", "2019"},
		{"/../", ""},
		{"a/b/", ""},
		{"..\\x", ""},
		{"20 9", ""},
		{"２０１９", ""}, // full-width digits are not ASCII
		{"201", ""},
		{"", ""},
	} {
		name := BuildMovieFilename(MovieName{Title: "Some Movie", ReleaseDate: tc.date, Hash: hash})
		if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
			t.Errorf("release_date %q: name %q is not one safe path component", tc.date, name)
		}
		hasYear := tc.wantYear != "" && strings.Contains(name, "_"+tc.wantYear+"_")
		if tc.wantYear != "" && !hasYear {
			t.Errorf("release_date %q: name %q lacks the year %s", tc.date, name, tc.wantYear)
		}
		if tc.wantYear == "" && strings.HasPrefix(name, "Some_Movie_") && !strings.HasPrefix(name, "Some_Movie_1080p") {
			t.Errorf("release_date %q: name %q carries a year it should not", tc.date, name)
		}
	}
}
