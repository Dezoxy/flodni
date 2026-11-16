package apiauth

// The route-to-scope table of ADR 0004: the one place that says what each route
// on the two servers needs. Nothing enforces it yet (the gate still takes one
// token); later steps read it on both ports and for the refusal metric.
//
// Unknown means admin: a route, a method or an engine action absent from the
// table is authorized as admin, so a forgotten row costs the caller a refusal,
// never exposure.

// Scope names what a caller may reach.
type Scope string

const (
	ScopeOpen    Scope = "open" // no credential
	ScopeWebhook Scope = "webhook"
	ScopeMetrics Scope = "metrics"
	ScopeRead    Scope = "read"
	ScopeLibrary Scope = "library"
	ScopeScrape  Scope = "scrape"
	ScopeEngine  Scope = "engine"
	ScopeUpload  Scope = "upload"
	ScopeAdmin   Scope = "admin"
)

// Server names one of the two listeners.
type Server string

const (
	Control Server = "control" // metrics_port, default 9080
	Engine  Server = "engine"  // GoStorm, 8090 and its TLS port
)

// Action values of a Route. ActionNone is a route with no body action, or one
// whose actions all need the same scope; the engine's POST /settings and POST
// /torrents carry an action in the JSON body that decides the scope.
const (
	ActionNone = ""
	ActionAny  = "*" // every action no other row of the route names
)

// Route is one row: the pattern as the router registers it (ServeMux path on
// the control server, gin FullPath on the engine), never a raw request path.
type Route struct {
	Server  Server
	Method  string
	Pattern string
	Action  string
	Scope   Scope
}

var routeTable = []Route{
	// Control server. http.ServeMux patterns carry no method, so a pattern
	// registered there needs at least one row, and a method no row lists is admin.
	{Control, "POST", "/plex/webhook", ActionNone, ScopeWebhook},
	{Control, "POST", "/webhook", ActionNone, ScopeWebhook},
	{Control, "GET", "/metrics", ActionNone, ScopeMetrics},
	{Control, "GET", "/metrics/profiling", ActionNone, ScopeMetrics},
	{Control, "GET", "/metrics/prometheus", ActionNone, ScopeMetrics},
	{Control, "GET", "/metrics/ttff", ActionNone, ScopeMetrics},
	{Control, "GET", "/api/sessions", ActionNone, ScopeRead},
	{Control, "GET", "/api/torrents/health", ActionNone, ScopeRead},
	{Control, "GET", "/api/startup", ActionNone, ScopeRead},
	{Control, "GET", "/api/library/list", ActionNone, ScopeRead},
	{Control, "GET", "/api/library/file", ActionNone, ScopeRead},
	{Control, "GET", "/api/library/audit", ActionNone, ScopeRead},
	{Control, "POST", "/api/library/add", ActionNone, ScopeLibrary},
	{Control, "POST", "/api/library/remove", ActionNone, ScopeLibrary},
	{Control, "POST", "/api/library/confirm", ActionNone, ScopeLibrary},
	{Control, "POST", "/api/library/inspect", ActionNone, ScopeLibrary}, // contacts the engine, so the swarm
	{Control, "POST", "/api/torrents/scrape", ActionNone, ScopeScrape},
	{Control, "GET", "/api/torrents/scrape/status", ActionNone, ScopeScrape},
	{Control, "POST", "/api/warmup/evict", ActionNone, ScopeAdmin}, // drops cached data
	{Control, "GET", "/api/config", ActionNone, ScopeAdmin},        // paths, URLs, host list
	{Control, "POST", "/api/config", ActionNone, ScopeAdmin},
	{Control, "POST", "/api/gostorm/settings", ActionNone, ScopeAdmin}, // the engine's settings set, proxied
	{Control, "POST", "/api/restart", ActionNone, ScopeAdmin},
	{Control, "GET", "/api/health", ActionNone, ScopeOpen},
	{Control, "GET", "/api/live", ActionNone, ScopeOpen},
	{Control, "GET", "/control", ActionNone, ScopeOpen}, // static page, no data

	// Engine server.
	{Engine, "GET", "/echo", ActionNone, ScopeOpen}, // the version string
	{Engine, "POST", "/settings", "get", ScopeEngine},
	{Engine, "POST", "/settings", ActionAny, ScopeAdmin}, // set, def, anything else
	{Engine, "POST", "/torrents", "list", ScopeEngine},
	{Engine, "POST", "/torrents", "active", ScopeEngine},
	{Engine, "POST", "/torrents", "get", ScopeEngine},
	{Engine, "POST", "/torrents", ActionAny, ScopeAdmin}, // add, set, rem, drop, anything else
	{Engine, "POST", "/torrent/upload", ActionNone, ScopeUpload},
	{Engine, "POST", "/cache", ActionNone, ScopeEngine}, // get is the only action
	{Engine, "GET", "/stream", ActionNone, ScopeEngine}, // the open-port rule applies unless admin
	{Engine, "HEAD", "/stream", ActionNone, ScopeEngine},
	{Engine, "GET", "/stream/*fname", ActionNone, ScopeEngine},
	{Engine, "HEAD", "/stream/*fname", ActionNone, ScopeEngine},
	{Engine, "GET", "/play/:hash/:id", ActionNone, ScopeEngine},
	{Engine, "HEAD", "/play/:hash/:id", ActionNone, ScopeEngine},
	{Engine, "GET", "/download/:size", ActionNone, ScopeEngine},
	{Engine, "GET", "/storage/settings", ActionNone, ScopeEngine},
	{Engine, "POST", "/storage/settings", ActionNone, ScopeAdmin},
	{Engine, "POST", "/shutdown", ActionNone, ScopeAdmin},
	{Engine, "POST", "/shutdown/*reason", ActionNone, ScopeAdmin},
}

// Routes returns a copy of the table, in declaration order.
func Routes() []Route { return append([]Route(nil), routeTable...) }

// Known reports whether the table has a row for this method and pattern on
// this server, whatever the action. The completeness tests use it.
func Known(server Server, method, pattern string) bool {
	for _, r := range routeTable {
		if r.Server == server && r.Method == method && r.Pattern == pattern {
			return true
		}
	}
	return false
}

// HasPattern reports whether any method of the pattern has a row.
func HasPattern(server Server, pattern string) bool {
	for _, r := range routeTable {
		if r.Server == server && r.Pattern == pattern {
			return true
		}
	}
	return false
}

// ScopeFor returns the scope a request needs: pattern is the router's own
// pattern for it, action the body action (empty when the route has none). A
// route or method with no row, and an action no row names on a route that
// has action rows, is admin.
func ScopeFor(server Server, method, pattern, action string) Scope {
	var fallback Scope
	for _, r := range routeTable {
		if r.Server != server || r.Method != method || r.Pattern != pattern {
			continue
		}
		switch {
		case r.Action == ActionNone:
			return r.Scope
		case r.Action == action:
			return r.Scope
		case r.Action == ActionAny:
			fallback = r.Scope
		}
	}
	if fallback != "" {
		return fallback
	}
	return ScopeAdmin
}
