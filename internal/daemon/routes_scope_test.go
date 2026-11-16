package daemon

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"flodni/internal/apiauth"
)

// registeredControlPatterns reads Run in main.go, the code that builds the real
// control mux, and returns every pattern passed to routes.handle. It fails on a
// registration that bypasses the registrar or whose pattern is not a literal, so
// no route can reach the mux unseen.
func registeredControlPatterns(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var patterns []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		recv, _ := sel.X.(*ast.Ident)
		if recv == nil {
			return true
		}
		switch {
		case recv.Name == "routes" && sel.Sel.Name == "handle":
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("%s: routes.handle needs a string literal pattern", fset.Position(call.Pos()))
				return true
			}
			p, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			patterns = append(patterns, p)
		case recv.Name == "mux" && (sel.Sel.Name == "Handle" || sel.Sel.Name == "HandleFunc"):
			t.Errorf("%s: register through routes.handle, not mux.%s", fset.Position(call.Pos()), sel.Sel.Name)
		}
		return true
	})
	return patterns
}

// Every pattern on the control mux has a row in the route-to-scope table, and
// every control row is a pattern the mux registers (ADR 0004).
func TestEveryControlRouteHasAScope(t *testing.T) {
	patterns := registeredControlPatterns(t)
	if len(patterns) == 0 {
		t.Fatal("found no routes.handle calls in main.go")
	}
	seen := map[string]bool{}
	for _, p := range patterns {
		if seen[p] {
			t.Errorf("pattern %s registered twice", p)
		}
		seen[p] = true
		if !apiauth.HasPattern(apiauth.Control, p) {
			t.Errorf("control route %s is registered with no row in internal/apiauth/scopes.go", p)
		}
	}
	for _, row := range apiauth.Routes() {
		if row.Server == apiauth.Control && !seen[row.Pattern] {
			t.Errorf("table row %s %s is not a route Run registers", row.Method, row.Pattern)
		}
	}
}

func TestControlRoutesRecordsPatterns(t *testing.T) {
	c := newControlRoutes()
	h := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }
	c.handle("/b", h)
	c.handle("/a", h)
	got := c.Patterns()
	if len(got) != 2 || got[0] != "/b" || got[1] != "/a" {
		t.Fatalf("Patterns = %v, want registration order [/b /a]", got)
	}
	rec := httptest.NewRecorder()
	c.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/a", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("handler not reached through the registrar: %d", rec.Code)
	}
}

func TestLiveAnswersOkWithoutDependencies(t *testing.T) {
	rec := httptest.NewRecorder()
	handleLive(rec, httptest.NewRequest("GET", "/api/live", nil))
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("GET /api/live = %d %q, want 200 ok", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handleLive(rec, httptest.NewRequest("POST", "/api/live", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/live = %d, want 405", rec.Code)
	}
}
