// Package authtest holds what the gate's tests on the two servers share: a tokens
// file with one token per scope, loaded through the real loader, and the
// hand-written golden copy of the route-to-scope table.
package authtest

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"flodni/internal/apiauth"
)

// Fixture is a scoped configuration loaded from a real tokens file.
type Fixture struct {
	Tokens *apiauth.Tokens
	// Token maps a fixture token name to its secret. The names are the eight scopes
	// (each token holds that scope alone) and "readlib", which holds read and library.
	Token map[string]string
	// Scopes maps a fixture token name to the scopes it holds as written.
	Scopes map[string][]string
	// Path is the tokens file.
	Path string
}

// New writes the file and loads it. The secrets are random per call.
func New(t testing.TB) *Fixture {
	t.Helper()
	f := &Fixture{Token: map[string]string{}, Scopes: map[string][]string{}}
	var entries []string
	add := func(name string, scopes ...string) {
		tok, hash, err := apiauth.GenerateToken()
		if err != nil {
			t.Fatal(err)
		}
		f.Token[name] = tok
		f.Scopes[name] = scopes
		quoted := make([]string, len(scopes))
		for i, s := range scopes {
			quoted[i] = `"` + s + `"`
		}
		entries = append(entries, `{"name":"`+name+`","scopes":[`+strings.Join(quoted, ",")+`],"sha256":"`+hash+`"}`)
	}
	for _, s := range apiauth.AllScopes {
		add(string(s), string(s))
	}
	add("readlib", "read", "library")
	f.Path = filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(f.Path, []byte(`{"tokens":[`+strings.Join(entries, ",")+`]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tk, err := apiauth.Load(apiauth.LoadOptions{TokensFile: f.Path, GoStormURL: "http://127.0.0.1:8090"})
	if err != nil {
		t.Fatal(err)
	}
	f.Tokens = tk
	return f
}

// Unknown is a token of the right shape that is in no file.
func (f *Fixture) Unknown() string { return "fl_" + strings.Repeat("Z", 43) }

// Allows is the rule written out by hand: a token reaches a route when it holds the
// scope the route needs or admin, and every token reaches an open route.
func (f *Fixture) Allows(token, required string) bool {
	if required == "open" {
		return true
	}
	for _, s := range f.Scopes[token] {
		if s == required || s == "admin" {
			return true
		}
	}
	return false
}

// Row is one line of the golden copy.
type Row struct {
	Server, Method, Pattern, Action, Scope string
}

// Golden reads internal/apiauth/testdata/route_scopes, the hand-written copy of
// the table. The expectations of the matrices come from here, never from the table
// under test.
func Golden(t testing.TB) []Row {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	fh, err := os.Open(filepath.Join(filepath.Dir(file), "..", "testdata", "route_scopes"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }() // read-only
	var rows []Row
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := strings.Fields(line)
		if len(p) != 5 {
			t.Fatalf("golden line %q: want 5 columns", line)
		}
		if p[3] == "-" {
			p[3] = ""
		}
		rows = append(rows, Row{p[0], p[1], p[2], p[3], p[4]})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return rows
}

// Path turns a route pattern into a request path the router matches to it.
func Path(pattern string) string {
	r := strings.NewReplacer(":hash", "0123456789abcdef0123456789abcdef01234567", ":id", "1", ":size", "1", "*fname", "x", "*reason", "x")
	return r.Replace(pattern)
}
