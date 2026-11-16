package apiauth

import (
	"sort"
	"sync"
)

// The reasons of flodni_auth_refusals_total (ADR 0004, "Metrics and logs"). The
// proxy-assertion reasons (jwt_*) are in internal/apiauth/jwt.
const (
	ReasonMissing         = "missing"           // no credential on a route that needs one
	ReasonInvalid         = "invalid"           // a credential was presented and is wrong
	ReasonScope           = "scope"             // a valid token without the scope the route needs
	ReasonQueryNotAllowed = "query_not_allowed" // a token query parameter where it is not allowed
	ReasonBody            = "body"              // the engine action peek refused the body
	ReasonCSRF            = "csrf"              // the ambient-credential rules
	ReasonSession         = "session"           // a cookie that is expired, tampered or revoked
	ReasonHost            = "host"
	ReasonOrigin          = "origin"
	ReasonCrossSite       = "cross_site"
	ReasonClosed          = "closed" // the gate was built with no credential store: it answers 503
)

// Fixed route labels besides the table's own patterns.
const (
	RouteUnmatched = "unmatched" // a path no row names
	RoutePreRoute  = "pre_route" // the engine's outer gate: it runs before routing
)

// Observer is told of every refusal. route is a pattern from the table,
// RouteUnmatched or RoutePreRoute, never a raw path; scope is the scope the route
// requires (empty for pre_route), never the presented token's.
type Observer func(route string, scope Scope, reason string)

// Refusals counts refusals by route, scope and reason, with bounded labels: a value
// outside the route table, the scopes and the known reasons is folded into a fixed
// one, so no path, token or name can ever become a label.
type Refusals struct {
	mu sync.Mutex
	n  map[[3]string]uint64
}

// RefusalCount is one series.
type RefusalCount struct {
	Route, Scope, Reason string
	Count                uint64
}

var knownReasons = func() map[string]bool {
	m := map[string]bool{}
	for _, r := range []string{ReasonMissing, ReasonInvalid, ReasonScope, ReasonQueryNotAllowed, ReasonBody, ReasonCSRF,
		ReasonSession, ReasonHost, ReasonOrigin, ReasonCrossSite, ReasonClosed,
		"jwt_malformed", "jwt_alg", "jwt_unknown_kid", "jwt_keys_unavailable", "jwt_signature", "jwt_issuer",
		"jwt_audience", "jwt_expired", "jwt_not_yet_valid", "jwt_claims"} {
		m[r] = true
	}
	return m
}()

// Observe counts one refusal; it is an Observer.
func (r *Refusals) Observe(route string, scope Scope, reason string) {
	if route != RouteUnmatched && route != RoutePreRoute && !HasPattern(Control, route) && !HasPattern(Engine, route) {
		route = RouteUnmatched
	}
	if scope != "" && scope != ScopeOpen && scopeBit(scope) == 0 {
		scope = ""
	}
	if !knownReasons[reason] {
		reason = ReasonInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == nil {
		r.n = map[[3]string]uint64{}
	}
	r.n[[3]string{route, string(scope), reason}]++
}

// Snapshot returns every series seen, sorted by route, scope, reason.
func (r *Refusals) Snapshot() []RefusalCount {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RefusalCount, 0, len(r.n))
	for k, v := range r.n {
		out = append(out, RefusalCount{k[0], k[1], k[2], v})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Route != b.Route {
			return a.Route < b.Route
		}
		if a.Scope != b.Scope {
			return a.Scope < b.Scope
		}
		return a.Reason < b.Reason
	})
	return out
}
