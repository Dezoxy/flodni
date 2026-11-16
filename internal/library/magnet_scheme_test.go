package library

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The in-process engine identity is admin, and the engine fetches an http(s) link and
// opens a file: link by scheme. So add and inspect take only a magnet whose first
// xt is exactly a 40-hex or 32-base32 btih, and refuse everything else before any
// engine call (ADR 0004). The old checks were unanchored regexps: a URL that merely
// contained "xt=urn:btih:<hash>" passed them.
const (
	hex40 = "0123456789abcdef0123456789abcdef01234567"
	b32   = "abcdefghijklmnopqrstuvwxyz234567"
)

var badMagnets = []string{
	"http://host/x?xt=urn:btih:" + hex40,
	"https://host/x?xt=urn:btih:" + hex40,
	"file:///etc/passwd?xt=urn:btih:" + hex40,
	"ftp://host/x?xt=urn:btih:" + hex40,
	" http://host/x?xt=urn:btih:" + hex40,
	"/etc/passwd?xt=urn:btih:" + hex40,
	"magnet:?dn=a&x=xt=urn:btih:" + hex40,
	"magnet:?dn=xt=urn:btih:" + hex40 + "&xt=urn:btih:" + strings.Repeat("a", 40),
	"magnet:?xt=urn:btih:" + hex40 + "ab",                 // 42 characters
	"magnet:?xt=urn:btih:" + hex40[:39],                   // 39
	"magnet:?xt=urn:btih:" + hex40[:31],                   // 31
	"magnet:?xt=urn:btih:" + strings.Repeat("g", 40),      // not hex
	"magnet:?xt=urn:btih:" + b32 + "a",                    // 33
	"magnet:?xt=urn:btmh:" + hex40,                        // not btih
	"magnet:?xt=urn:sha1:" + hex40,                        // not btih
	"magnet:?dn=only-a-name",                              // no xt
	"magnet:",                                             // nothing
	"magnet:?xt=urn:btih:" + hex40 + "%26xs=http://h/x.t", // the hash runs on (no anchor)
	"magnet:?x.pe=" + hex40 + "&xt=urn:btih:" + hex40 + "x",
	"magnet:?x=1&amp;xt=urn:btih:" + hex40 + "&xt=urn:btih:" + strings.Repeat("a", 40), // the engine unescapes &amp; and reads another first xt
	"magnet://host/?xt=urn:btih:" + hex40,
	"magnet:\n?xt=urn:btih:" + hex40,
	"torrs://" + hex40,
	"magnet:/etc/passwd?xt=urn:btih:" + hex40, // a path is not a magnet
	// The engine rewrites &amp; to &, which makes the percent-encoded xt (H2) the first
	// one it reads, where the library read H1: only the re-check refuses it.
	"magnet:?a=1&amp;xt=urn%3Abtih%3A" + strings.Repeat("b", 40) + "&xt=urn:btih:" + hex40,
	hex40, // a bare hash is the hash field, not a magnet
}

var goodMagnets = []string{
	"magnet:?xt=urn:btih:" + hex40,
	"magnet:?xt=urn:btih:" + strings.ToUpper(hex40),
	"magnet:?xt=urn:btih:" + hex40 + "&dn=Name&tr=udp%3A%2F%2Ft.example%3A80%2Fannounce",
	"magnet:?dn=Name&xt=urn:btih:" + hex40,
	"magnet:?xt=urn:btih:" + b32,
	"magnet:?xt=urn:btih:" + strings.ToUpper(b32) + "&tr=http://t.example/announce",
	"MAGNET:?xt=urn:btih:" + hex40,
	"magnet:?xt=urn:btih:" + hex40 + "&amp;dn=Name",
}

func TestCheckMagnet(t *testing.T) {
	for _, m := range badMagnets {
		if err := CheckMagnet(m); err == nil {
			t.Errorf("CheckMagnet(%q) accepted it", m)
		}
	}
	for _, m := range goodMagnets {
		if err := CheckMagnet(m); err != nil {
			t.Errorf("CheckMagnet(%q) = %v", m, err)
		}
	}
	if err := CheckMagnet(""); err != nil {
		t.Errorf("an absent magnet is the hash path: %v", err)
	}
}

func scopedManager(t *testing.T) (*Manager, *recEngine) {
	t.Helper()
	eng := &recEngine{}
	eng.files = []FileStat{{ID: 1, Path: "A.Movie.2020.mkv", Length: 1 << 20}}
	root := t.TempDir()
	return New(Config{MoviesDir: root + "/movies", TVDir: root + "/tv",
		GoStorm: eng, GoStormURL: "http://engine"}), eng
}

func TestAddRefusesALinkThatIsNotAMagnetBeforeAnyEngineCall(t *testing.T) {
	for _, magnet := range badMagnets {
		m, eng := scopedManager(t)
		_, err := m.Add(context.Background(), AddRequest{Type: "movie", Title: "A Movie", Magnet: magnet, Year: 2020})
		var le *Error
		if err == nil || !errors.As(err, &le) || le.Status != http.StatusBadRequest {
			t.Errorf("Add(%q) = %v, want a 400", magnet, err)
		}
		if calls := eng.seen(); len(calls) != 0 {
			t.Errorf("Add(%q) reached the engine: %v", magnet, calls)
		}
	}
}

func TestInspectRefusesALinkThatIsNotAMagnetBeforeAnyEngineCall(t *testing.T) {
	for _, magnet := range badMagnets {
		m, eng := scopedManager(t)
		_, err := m.Inspect(context.Background(), InspectRequest{Title: "A Movie", Magnet: magnet, MetadataWait: 1})
		var le *Error
		if err == nil || !errors.As(err, &le) || le.Status != http.StatusBadRequest {
			t.Errorf("Inspect(%q) = %v, want a 400", magnet, err)
		}
		if calls := eng.seen(); len(calls) != 0 {
			t.Errorf("Inspect(%q) reached the engine: %v", magnet, calls)
		}
	}
}

// Through the handlers, with the exact request ADR 0004 names.
func TestHandlersAnswer400ForAnHTTPLinkWithABtihInIt(t *testing.T) {
	for _, tc := range []struct{ route, body string }{
		{"add", `{"type":"movie","title":"A","year":2020,"magnet":"http://host/x?xt=urn:btih:` + hex40 + `"}`},
		{"inspect", `{"title":"A","magnet":"http://host/x?xt=urn:btih:` + hex40 + `"}`},
	} {
		m, eng := scopedManager(t)
		h := NewHandler(m)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/library/"+tc.route, strings.NewReader(tc.body))
		if tc.route == "add" {
			h.Add(rec, req)
		} else {
			h.Inspect(rec, req)
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", tc.route, rec.Code, rec.Body)
		}
		if calls := eng.seen(); len(calls) != 0 {
			t.Errorf("%s: the engine was called: %v", tc.route, calls)
		}
	}
}

// A valid magnet still reaches the engine, with the link unchanged.
func TestAValidMagnetStillReachesTheEngine(t *testing.T) {
	m, eng := scopedManager(t)
	_, err := m.Inspect(context.Background(), InspectRequest{Title: "A Movie", Magnet: goodMagnets[2], MetadataWait: 1})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got := eng.seen(); len(got) == 0 || got[0] != "ListTorrents" {
		t.Errorf("engine calls = %v, want the inspect to run", got)
	}
}
