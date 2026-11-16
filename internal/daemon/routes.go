package daemon

import "net/http"

// controlRoutes is the control server's mux plus a record of every pattern put
// on it. http.ServeMux cannot list its patterns, so the route-to-scope
// completeness test reads the calls to handle instead. It adds no behaviour:
// handle is mux.HandleFunc.
type controlRoutes struct {
	mux      *http.ServeMux
	patterns []string
}

// newControlRoutes starts an own mux: nothing else in the process (expvar,
// pprof) can add routes to the control port.
func newControlRoutes() *controlRoutes {
	return &controlRoutes{mux: http.NewServeMux()}
}

// handle registers h on the mux for pattern and records the pattern.
func (c *controlRoutes) handle(pattern string, h func(http.ResponseWriter, *http.Request)) {
	c.patterns = append(c.patterns, pattern)
	c.mux.HandleFunc(pattern, h)
}

// Patterns returns the patterns registered so far, in order.
func (c *controlRoutes) Patterns() []string { return append([]string(nil), c.patterns...) }

// handleLive answers GET /api/live: the process serves. It checks nothing, so a
// container or load balancer can poll it cheaply; GET /api/health runs the
// mount, engine and database checks and reports warnings.
func handleLive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}
