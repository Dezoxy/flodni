package apiauth

import (
	"bufio"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The golden file is written by hand. The table under test is never used to
// produce it, so a change to scopes.go that widens a route fails here until a
// reviewer sees the same change in testdata/route_scopes.
func TestRouteTableMatchesGolden(t *testing.T) {
	f, err := os.Open("testdata/route_scopes")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var want []Route
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := strings.Fields(line)
		if len(p) != 5 {
			t.Fatalf("golden line %q: want 5 fields", line)
		}
		action := p[3]
		if action == "-" {
			action = ActionNone
		}
		want = append(want, Route{Server(p[0]), p[1], p[2], action, Scope(p[4])})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	got := Routes()
	if len(got) != len(want) {
		t.Fatalf("table has %d rows, golden %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: table %+v, golden %+v", i, got[i], want[i])
		}
	}
}

func TestRouteTableHasNoDuplicateRows(t *testing.T) {
	seen := map[[4]string]bool{}
	for _, r := range Routes() {
		k := [4]string{string(r.Server), r.Method, r.Pattern, r.Action}
		if seen[k] {
			t.Errorf("duplicate row %+v", r)
		}
		seen[k] = true
	}
}

func TestScopeForUnknownIsAdmin(t *testing.T) {
	tests := []struct {
		name    string
		server  Server
		method  string
		pattern string
		action  string
		want    Scope
	}{
		{"listed", Control, "GET", "/api/sessions", "", ScopeRead},
		{"open", Control, "GET", "/api/live", "", ScopeOpen},
		{"unlisted method on a known path", Control, "POST", "/control", "", ScopeAdmin},
		{"DELETE on health", Control, "DELETE", "/api/health", "", ScopeAdmin},
		{"unknown path", Control, "GET", "/api/nope", "", ScopeAdmin},
		{"unmatched pattern", Control, "GET", "", "", ScopeAdmin},
		{"engine row on the control server", Control, "GET", "/echo", "", ScopeAdmin},
		{"engine settings get", Engine, "POST", "/settings", "get", ScopeEngine},
		{"engine settings set", Engine, "POST", "/settings", "set", ScopeAdmin},
		{"engine settings no action", Engine, "POST", "/settings", "", ScopeAdmin},
		{"engine torrents list", Engine, "POST", "/torrents", "list", ScopeEngine},
		{"engine torrents rem", Engine, "POST", "/torrents", "rem", ScopeAdmin},
		{"engine torrents unknown action", Engine, "POST", "/torrents", "anything", ScopeAdmin},
		{"engine upload", Engine, "POST", "/torrent/upload", "", ScopeUpload},
		{"engine GET on a POST route", Engine, "GET", "/torrents", "", ScopeAdmin},
		{"engine stream HEAD", Engine, "HEAD", "/stream/*fname", "", ScopeEngine},
		{"engine PUT stream", Engine, "PUT", "/stream", "", ScopeAdmin},
	}
	for _, tc := range tests {
		if got := ScopeFor(tc.server, tc.method, tc.pattern, tc.action); got != tc.want {
			t.Errorf("%s: ScopeFor = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// GET /api/live is open with a token set, like /api/health, and only for GET.
func TestLivenessRouteIsOpen(t *testing.T) {
	if got := serve(tok, "GET", "/api/live", "").Code; got != 200 {
		t.Errorf("GET /api/live with a token set and no header = %d, want 200", got)
	}
	if got := serve(tok, "POST", "/api/live", "").Code; got != 401 {
		t.Errorf("POST /api/live = %d, want 401", got)
	}
	if got := serve(tok, "HEAD", "/api/live", "").Code; got != 401 {
		t.Errorf("HEAD /api/live = %d, want 401 (GET only, like the table)", got)
	}
	if got := serve(tok, "GET", "/api/live/x", "").Code; got != 401 {
		t.Errorf("GET /api/live/x = %d, want 401", got)
	}
}

// The table's own rules. A pattern with an ActionNone row and action rows would
// let the first row win; every catch-all action row is admin; and an open row is
// open in the gate, and nothing else is.
func TestRouteTableInvariants(t *testing.T) {
	type key struct {
		s    Server
		m, p string
	}
	none, acted := map[key]bool{}, map[key]bool{}
	for _, r := range Routes() {
		k := key{r.Server, r.Method, r.Pattern}
		if r.Action == ActionNone {
			none[k] = true
		} else {
			acted[k] = true
		}
		if r.Action == ActionAny && r.Scope != ScopeAdmin {
			t.Errorf("%+v: a catch-all action must be admin", r)
		}
		open := controlOpen
		if r.Server == Engine {
			open = gostormOpen
		}
		req := httptest.NewRequest(r.Method, r.Pattern, nil)
		if got := open(req); got != (r.Scope == ScopeOpen) {
			t.Errorf("%+v: the gate's open set says %v", r, got)
		}
	}
	for k := range none {
		if acted[k] {
			t.Errorf("%+v has both an action-less row and action rows", k)
		}
	}
}

// POST /control/login is open in the gate (its credential is in the body, checked by
// the handler, which answers 404 outside scoped mode); logout and every other method
// of both still need a credential.
func TestLoginRouteIsOpenForPostOnly(t *testing.T) {
	if got := serve(tok, "POST", "/control/login", "").Code; got != 200 {
		t.Errorf("POST /control/login with a token set and no header = %d, want 200", got)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/control/login"}, {"PUT", "/control/login"}, {"POST", "/control/login/"},
		{"POST", "/control/logout"}, {"GET", "/control/logout"}, {"POST", "/control/Login"},
	} {
		if got := serve(tok, c.method, c.path, "").Code; got != 401 {
			t.Errorf("%s %s = %d, want 401", c.method, c.path, got)
		}
	}
}
