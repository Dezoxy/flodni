package web

import (
	"testing"

	"flodni/internal/apiauth"
)

// Every route the real router registers has a row in the route-to-scope table
// (ADR 0004), and every engine row is a route the router registers. A route
// added without a scope fails here, not in production as an admin-only 403.
func TestEveryEngineRouteHasAScope(t *testing.T) {
	registered := map[[2]string]bool{}
	for _, r := range newRoute().Routes() {
		registered[[2]string{r.Method, r.Path}] = true
		if !apiauth.Known(apiauth.Engine, r.Method, r.Path) {
			t.Errorf("route %s %s is registered with no row in internal/apiauth/scopes.go", r.Method, r.Path)
		}
	}
	for _, row := range apiauth.Routes() {
		if row.Server == apiauth.Engine && !registered[[2]string{row.Method, row.Pattern}] {
			t.Errorf("table row %s %s is not a route newRoute registers", row.Method, row.Pattern)
		}
	}
}
