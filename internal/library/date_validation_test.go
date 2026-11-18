package library

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A show folder takes a year only when the date starts with four ASCII digits. The
// date comes from the request, so nothing else may reach the path.
func TestShowFolderNameYearIsFourDigitsOrNothing(t *testing.T) {
	for _, tc := range []struct{ date, want string }{
		{"2013-09-24", "Show (2013)"},
		{"2013", "Show (2013)"},
		{"201", "Show"},
		{"ab-cd", "Show"},
		{"abcd", "Show"},
		{"../x", "Show"},
		{"/../", "Show"},
		{"..\\x", "Show"},
		{"20 9", "Show"},
		{"２０１３", "Show"}, // full-width digits are not ASCII
		{"٢٠١٣", "Show"}, // Arabic-Indic digits
		{"é2013", "Show"},
		{"", "Show"},
	} {
		got := ShowFolderName("Show", tc.date)
		if got != tc.want {
			t.Errorf("ShowFolderName(%q) = %q, want %q", tc.date, got, tc.want)
		}
		if strings.ContainsAny(got, `/\`) || strings.Contains(got, "..") {
			t.Errorf("ShowFolderName(%q) = %q is not one safe path component", tc.date, got)
		}
	}
}

func TestAddValidatesDates(t *testing.T) {
	for _, tc := range []struct {
		field, value string
		ok           bool
	}{
		{"release_date", "", true},
		{"release_date", "2019", true},
		{"release_date", "2019-12-13", true},
		{"release_date", "201", false},
		{"release_date", "2019-12", false},
		{"release_date", "2019-12-13T00:00:00Z", false},
		{"release_date", "2019-1-3", false},
		{"release_date", " 2019", false},
		{"release_date", "2019\n", false},
		{"release_date", "/../", false},
		{"release_date", "../x", false},
		{"release_date", "２０１９", false},
		{"first_air_date", "", true},
		{"first_air_date", "2013", true},
		{"first_air_date", "2013-09-24", true},
		{"first_air_date", "../x", false},
		{"first_air_date", "/../", false},
		{"first_air_date", "abcd", false},
		{"first_air_date", "2013/09/24", false},
		{"first_air_date", "٢٠١٣", false},
	} {
		m, eng := scopedManager(t)
		req := AddRequest{Type: "movie", Title: "A Movie", Hash: hex40}
		if tc.field == "first_air_date" {
			req = AddRequest{Type: "tv", Title: "A Show", Hash: hex40, Season: 1, Episode: 1, DryRun: true}
			req.FirstAirDate = tc.value
		} else {
			req.ReleaseDate = tc.value
		}
		_, err := m.Add(context.Background(), req)
		var le *Error
		isDateErr := errors.As(err, &le) && le.Status == http.StatusBadRequest && strings.Contains(le.Message, tc.field)
		if tc.ok && isDateErr {
			t.Errorf("%s %q refused: %v", tc.field, tc.value, err)
		}
		if !tc.ok {
			if !isDateErr {
				t.Errorf("%s %q = %v, want a 400 naming the field", tc.field, tc.value, err)
			}
			if calls := eng.seen(); len(calls) != 0 {
				t.Errorf("%s %q reached the engine: %v", tc.field, tc.value, calls)
			}
		}
	}
}

func TestAddHandlerAnswers400ForABadDate(t *testing.T) {
	for _, body := range []string{
		`{"type":"movie","title":"A","hash":"` + hex40 + `","release_date":"../x"}`,
		`{"type":"tv","title":"A","hash":"` + hex40 + `","season":1,"episode":1,"first_air_date":"/../"}`,
	} {
		m, eng := scopedManager(t)
		rec := httptest.NewRecorder()
		NewHandler(m).Add(rec, httptest.NewRequest(http.MethodPost, "/api/library/add", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "YYYY") {
			t.Errorf("%s: status %d body %s, want a 400 that names the format", body, rec.Code, rec.Body)
		}
		if calls := eng.seen(); len(calls) != 0 {
			t.Errorf("%s: the engine was called: %v", body, calls)
		}
	}
}
