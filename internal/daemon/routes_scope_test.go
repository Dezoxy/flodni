package daemon

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"flodni/internal/apiauth"
)

// stubControl registers every control route through the real registrar with a
// stub handler that records the pattern it served.
func stubControl(t *testing.T) (*apiauth.Registrar, *[]string) {
	t.Helper()
	var reached []string
	var h controlHandlers
	v := reflect.ValueOf(&h).Elem()
	stub := func(w http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}
	for i := 0; i < v.NumField(); i++ {
		v.Field(i).Set(reflect.ValueOf(stub))
	}
	routes := apiauth.NewRegistrar()
	registerControlRoutes(routes, h)
	return routes, &reached
}

// Every pattern on the real control mux has a row in the route-to-scope table, and
// every control row is a pattern the mux registers (ADR 0004).
func TestEveryControlRouteHasAScope(t *testing.T) {
	routes, _ := stubControl(t)
	seen := map[string]bool{}
	for _, p := range routes.Patterns() {
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
			t.Errorf("table row %s %s is not a route the control mux registers", row.Method, row.Pattern)
		}
	}
}

// Each table row is served by its own pattern, and the router's pattern is the
// one the table is keyed by.
func TestControlMuxServesEveryRow(t *testing.T) {
	routes, reached := stubControl(t)
	for _, row := range apiauth.Routes() {
		if row.Server != apiauth.Control {
			continue
		}
		req := httptest.NewRequest(row.Method, row.Pattern, nil)
		if got := routes.Pattern(req); got != row.Pattern {
			t.Errorf("%s %s: the router names pattern %q", row.Method, row.Pattern, got)
		}
		before := len(*reached)
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		if len(*reached) != before+1 || rec.Code != http.StatusNoContent {
			t.Errorf("%s %s: stub not reached (status %d)", row.Method, row.Pattern, rec.Code)
		}
	}
}

// The router, not a hand-written matcher, decides what a path is: a trailing
// slash, a doubled slash and another case are not the registered route.
func TestControlMuxDifferentialPaths(t *testing.T) {
	routes, reached := stubControl(t)
	for _, target := range []string{
		"/api/sessions/", "/api/sessions//", "//api/sessions", "/api//sessions",
		"/API/SESSIONS", "/Api/Sessions", "/api/sessions/.", "/api/./sessions", "/api/x/../sessions",
		"/api/health/", "/control/", "/CONTROL", "/api/live/", "/API/LIVE",
	} {
		before := len(*reached)
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
		if len(*reached) != before {
			t.Errorf("GET %s reached a handler (status %d)", target, rec.Code)
		}
		// A path the mux has to clean is answered with a redirect to the cleaned one,
		// never served; any other pattern match would have reached the stub.
		if p := routes.Pattern(httptest.NewRequest("GET", target, nil)); p != "" && rec.Code/100 != 3 {
			t.Errorf("GET %s: pattern %q but status %d", target, p, rec.Code)
		}
	}
}

func TestRegistrarPanicsOnANilHandler(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a nil handler registered silently")
		}
	}()
	registerControlRoutes(apiauth.NewRegistrar(), controlHandlers{})
}

func TestLiveAnswersOkWithoutDependencies(t *testing.T) {
	rec := httptest.NewRecorder()
	handleLive(rec, httptest.NewRequest("GET", "/api/live", nil))
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("GET /api/live = %d %q, want 200 ok", rec.Code, rec.Body.String())
	}
	for _, m := range []string{"POST", "HEAD"} {
		rec = httptest.NewRecorder()
		handleLive(rec, httptest.NewRequest(m, "/api/live", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /api/live = %d, want 405", m, rec.Code)
		}
	}
}
