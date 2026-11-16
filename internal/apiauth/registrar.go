package apiauth

import "net/http"

// Registrar is the control server's mux plus a record of every pattern put on it.
// The mux is unexported, so Handle is the only way to add a route and the
// route-to-scope tests see all of them (http.ServeMux cannot list its patterns).
// Handle is mux.HandleFunc and adds no behaviour.
type Registrar struct {
	mux      *http.ServeMux
	patterns []string
}

// NewRegistrar starts an own mux: nothing else in the process (expvar, pprof) can
// add routes to the control port.
func NewRegistrar() *Registrar { return &Registrar{mux: http.NewServeMux()} }

// Handle registers h for pattern and records the pattern. A nil h panics at
// registration, so a handler that was never wired fails the start, not a request.
func (r *Registrar) Handle(pattern string, h func(http.ResponseWriter, *http.Request)) {
	if h == nil {
		panic("apiauth: nil handler for " + pattern)
	}
	r.patterns = append(r.patterns, pattern)
	r.mux.HandleFunc(pattern, h)
}

// Patterns returns the registered patterns in registration order.
func (r *Registrar) Patterns() []string { return append([]string(nil), r.patterns...) }

// ServeHTTP serves through the mux.
func (r *Registrar) ServeHTTP(w http.ResponseWriter, req *http.Request) { r.mux.ServeHTTP(w, req) }

// Pattern is the pattern the router matches req to, "" when it matches none:
// the router names the pattern, so no second matcher can disagree with it.
func (r *Registrar) Pattern(req *http.Request) string {
	_, p := r.mux.Handler(req)
	return p
}
