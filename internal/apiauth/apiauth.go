// Package apiauth is the optional gate on the control server and on the engine's
// (GoStorm) server: a bearer token, scoped tokens from a file (ADR 0004), and on the
// control port a proxy-signed assertion and a session cookie.
package apiauth

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"

	"flodni/internal/apiauth/jwt"
)

// Control profile: everything needs the token except the static panel page,
// which holds no data, GET /api/health, which reports status words only, and
// GET /api/live, which answers "ok" and checks nothing. The Plex webhook cannot send headers, so only
// those two routes also take the token as ?token=; a query token elsewhere
// would leak into logs and browser history.
// POST /control/login is open too: its credential is the body, which the handler
// checks (and which answers 404 outside scoped mode).
func controlOpen(r *http.Request) bool {
	if r.Method == http.MethodPost {
		return r.URL.Path == "/control/login"
	}
	return r.Method == http.MethodGet && (r.URL.Path == "/control" || r.URL.Path == "/api/health" || r.URL.Path == "/api/live")
}

func controlWebhook(r *http.Request) bool {
	return r.URL.Path == "/plex/webhook" || r.URL.Path == "/webhook"
}

// GoStorm profile: everything needs the header except the readiness probe
// GET /echo, which returns only the version. No route takes a query token.
func gostormOpen(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == "/echo"
}

func never(*http.Request) bool { return false }

// Middleware gates the control server. With no credential configured (token empty
// and no WithTokens) it only applies the open-port refusals. Otherwise it answers
// 401 unless the request carries a credential, and 403 unless that credential holds
// the scope the route needs (see "Scoped mode" below). The token and the credential
// offered are never logged.
//
// token is the legacy api_token: one credential with every scope. WithTokens
// replaces it with the scoped store.
func Middleware(token string, logger *slog.Logger, next http.Handler, opts ...Option) http.Handler {
	return gate(controlProfile, token, logger, next, opts)
}

// GoStorm gates the engine's server on port 8090 the same way, with only GET /echo
// open and no query token anywhere. In scoped mode it authenticates the bearer and
// puts the identity in the request context; the scope check needs the route
// pattern, which only the router knows, and is made there (internal/gostorm/web).
// The engine never reads a cookie or the proxy's header. A nil logger logs nothing.
func GoStorm(token string, logger *slog.Logger, next http.Handler, opts ...Option) http.Handler {
	return gate(gostormProfile, token, logger, next, opts)
}

// profile is what differs between the two servers.
type profile struct {
	name    string
	server  Server
	open    func(*http.Request) bool
	webhook func(*http.Request) bool
	// ambient: the server reads a cookie and the proxy's assertion (control only).
	ambient bool
}

var (
	controlProfile = profile{"Control", Control, controlOpen, controlWebhook, true}
	gostormProfile = profile{"GoStorm", Engine, gostormOpen, never, false}
)

// Option tunes a gate. The zero set is today's behaviour.
type Option func(*options)

type options struct {
	hosts      map[string]bool
	tokens     *Tokens
	pattern    func(*http.Request) string
	jwt        JWTVerifier
	cookie     CookieCredential
	observer   Observer
	panelHTTPS bool
	// tokensSet: WithTokens was given, even with nil. A nil store is a wiring mistake and
	// fails closed; it is never read as "no credential configured".
	tokensSet bool
}

// JWTVerifier checks the proxy's assertion (internal/apiauth/jwt.Verifier).
type JWTVerifier interface {
	Verify(r *http.Request) (jwt.Identity, error)
}

// CookieCredential reads the session cookie of ADR 0004 step 6. present false: no
// cookie, try the next credential. present with an error: refused with reason session,
// no fall-through. present with nil: authenticated as name, with admin scope.
type CookieCredential func(r *http.Request) (name string, present bool, err error)

// WithTokens sets the credential store, replacing the token argument. A nil or open
// store leaves the port open.
func WithTokens(t *Tokens) Option {
	return func(o *options) { o.tokens, o.tokensSet = t, true }
}

// WithPattern gives the control gate the router's own pattern for a request (the
// registrar's Pattern), the one the scope table is keyed by. A scoped control gate
// needs it: without it every route would be a guess, so building one without it
// panics at start.
func WithPattern(f func(*http.Request) string) Option { return func(o *options) { o.pattern = f } }

// WithJWT turns on the proxy's assertion as a credential on the control port.
func WithJWT(v JWTVerifier) Option { return func(o *options) { o.jwt = v } }

// WithCookie plugs in the session cookie as a credential on the control port, tried
// after the bearer and before the assertion. The engine gate ignores it.
func WithCookie(c CookieCredential) Option { return func(o *options) { o.cookie = c } }

// WithObserver reports every refusal (see Observer).
func WithObserver(f Observer) Option { return func(o *options) { o.observer = f } }

// WithPanelHTTPS says the panel is served over https behind a TLS tunnel
// (auth_panel_https): the Origin of an ambient-credential post must then be https.
func WithPanelHTTPS(on bool) Option { return func(o *options) { o.panelHTTPS = on } }

// WithAllowedHosts turns on the Host allowlist, the defence against DNS
// rebinding: a request whose Host hostname (port stripped, lowercased, trailing
// dot removed) is not listed is refused with 403, whether or not a token is
// set. IP literals, "localhost" and this machine's own hostname are always
// allowed in addition. An empty list leaves the check off. Entries are
// normalised here the same way, so a caller may pass them as configured.
func WithAllowedHosts(hosts []string) Option {
	return func(o *options) {
		set := map[string]bool{}
		for _, h := range hosts {
			if h = normHost(h); h != "" {
				set[h] = true
			}
		}
		if len(set) == 0 {
			return
		}
		set["localhost"] = true
		if own, err := os.Hostname(); err == nil {
			if own = normHost(own); own != "" {
				set[own] = true
			}
		}
		o.hosts = set
	}
}

// normHost lowercases a host or host:port, strips the port and the brackets of
// an IPv6 literal, and removes one trailing dot.
func normHost(hostport string) string {
	host := strings.TrimSpace(hostport)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

func hostAllowed(r *http.Request, allowed map[string]bool) bool {
	h := normHost(r.Host)
	if h == "" {
		return false
	}
	if allowed[h] {
		return true
	}
	_, err := netip.ParseAddr(h)
	return err == nil
}

func gate(p profile, token string, logger *slog.Logger, next http.Handler, opts []Option) http.Handler {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	tokens := o.tokens
	if tokens == nil && o.tokensSet {
		// Fail closed: answer nothing at all, open routes included.
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "authentication is not configured", http.StatusServiceUnavailable)
		})
	}
	if tokens == nil {
		tokens, _ = Load(LoadOptions{APIToken: token}) // api_token alone cannot fail
	}
	if p.server == Control && tokens.Mode() == ModeScoped && o.pattern == nil {
		panic("apiauth: a scoped control gate needs WithPattern (the router's own pattern)")
	}
	g := &gater{p: p, o: o, tokens: tokens, logger: logger, next: next}
	if !p.ambient {
		g.o.cookie, g.o.jwt = nil, nil // the engine port never reads either
	}
	var h http.Handler = http.HandlerFunc(g.serve)
	if o.hosts == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(r, o.hosts) {
			g.refuse(w, r, "host not allowed", "host not allowed", ReasonHost)
			return
		}
		h.ServeHTTP(w, r)
	})
}

type gater struct {
	p      profile
	o      options
	tokens *Tokens
	logger *slog.Logger
	next   http.Handler
}

// route returns the label and scope of a refusal of r: the router's pattern when
// the gate can ask for it, "pre_route" and no scope when it cannot (the engine).
func (g *gater) route(r *http.Request) (label string, scope Scope, pattern string) {
	if g.p.server != Control || g.o.pattern == nil {
		return RoutePreRoute, "", ""
	}
	pattern = g.o.pattern(r)
	if !HasPattern(Control, pattern) {
		return RouteUnmatched, ScopeFor(Control, r.Method, "", ""), ""
	}
	return pattern, ScopeFor(Control, r.Method, pattern, ""), pattern
}

func (g *gater) observe(r *http.Request, reason string) {
	if g.o.observer == nil {
		return
	}
	label, scope, _ := g.route(r)
	g.o.observer(label, scope, reason)
}

// refuse answers 403 for the host, origin and cross-site refusals and counts it.
func (g *gater) refuse(w http.ResponseWriter, r *http.Request, what, body, reason string) {
	g.observe(r, reason)
	refuse(w, r, g.p.name, g.logger, what, body)
}

func refuse(w http.ResponseWriter, r *http.Request, name string, logger *slog.Logger, what, body string) {
	if logger != nil {
		logger.Info(fmt.Sprintf("[%s] 403 %s %s %q from %s (Host %q)", name, what, r.Method, r.URL.Path, r.RemoteAddr, r.Host), "name", name, "method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr, "host", r.Host)
	}
	http.Error(w, body, http.StatusForbidden)
}

func (g *gater) serve(w http.ResponseWriter, r *http.Request) {
	if g.tokens.Mode() == ModeOpen {
		if crossOrigin(r) {
			g.refuse(w, r, "cross-origin", "cross-origin request refused", ReasonOrigin)
			return
		}
		if crossSite(r) && !g.p.open(r) {
			g.refuse(w, r, "cross-site", "cross-site request refused", ReasonCrossSite)
			return
		}
		g.next.ServeHTTP(w, r)
		return
	}
	if g.p.open(r) {
		g.next.ServeHTTP(w, r)
		return
	}
	id, r2, d := g.authenticate(r)
	if d == nil && id.Ambient {
		if reason := ambientRefusal(r, g.o.panelHTTPS); reason != "" {
			d = &denial{http.StatusForbidden, reason, "request refused"}
		}
	}
	label, scope, _ := g.route(r)
	if d == nil && g.o.pattern != nil && scope != ScopeOpen && !id.Scopes.Has(scope) {
		d = &denial{http.StatusForbidden, ReasonScope, "forbidden"}
	}
	if d != nil {
		g.deny(w, r, d)
		return
	}
	r2 = r2.WithContext(WithIdentity(r2.Context(), id))
	if g.logsName(r, scope) && g.logger != nil {
		sw := &statusWriter{ResponseWriter: w}
		g.next.ServeHTTP(sw, r2)
		if sw.status() < 400 {
			g.logger.Info(fmt.Sprintf("[%s] token %q %s %q", g.p.name, id.Name, r.Method, label), "name", g.p.name, "token", id.Name, "method", r.Method, "route", label)
		}
		return
	}
	g.next.ServeHTTP(w, r2)
}

// logsName: a success on a mutating route logs the token's name (never the token).
func (g *gater) logsName(r *http.Request, scope Scope) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return false
	}
	return scope == ScopeLibrary || scope == ScopeAdmin || scope == ScopeUpload
}

type denial struct {
	status int
	reason string
	body   string
}

func (g *gater) deny(w http.ResponseWriter, r *http.Request, d *denial) {
	g.observe(r, d.reason)
	if g.logger != nil {
		g.logger.Info(fmt.Sprintf("[%s] %d %s %q from %s: %s", g.p.name, d.status, r.Method, r.URL.Path, r.RemoteAddr, d.reason),
			"name", g.p.name, "method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr, "reason", d.reason)
	}
	if d.status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
		if g.tokens.Mode() == ModeScoped && g.p.server == Control {
			w.Header().Set("Flodni-Auth", "login") // the panel shows its sign-in form on it; the engine has none
		}
	}
	http.Error(w, d.body, d.status)
}

func unauthorized(reason string) *denial {
	return &denial{http.StatusUnauthorized, reason, "unauthorized"}
}

// authenticate finds the credential, in this order, the first one present deciding
// and a wrong one refused without falling through: the bearer header (an empty
// bearer is no credential), a token in the query (the two webhook routes only), the
// session cookie, the proxy's assertion. It returns the request the handler gets,
// with the token parameter stripped from what it can see.
func (g *gater) authenticate(r *http.Request) (Identity, *http.Request, *denial) {
	scoped := g.tokens.Mode() == ModeScoped
	q := r.URL.Query()
	_, hasParam := q["token"]
	onWebhook := g.p.webhook(r)
	// Off the webhook routes a token parameter is refused before anything is hashed or
	// compared, whatever its value and whatever else the request carries, so the
	// refusal says nothing about whether the value was a real token. (A legacy
	// api_token keeps today's behaviour: the parameter is ignored there.)
	if scoped && hasParam && !onWebhook {
		return Identity{}, r, unauthorized(ReasonQueryNotAllowed)
	}
	pass := func(name string, scopes ScopeSet) (Identity, *http.Request, *denial) {
		return Identity{Name: name, Scopes: scopes}, g.stripped(r, onWebhook && hasParam), nil
	}
	if v, ok := bearerOf(r); ok {
		name, scopes, found := g.tokens.Lookup(v)
		if found {
			return pass(name, scopes)
		}
		// A scoped store refuses a present-but-wrong credential. api_token alone keeps
		// v0.2.6, where a wrong bearer fell through to the query token on a webhook.
		if g.tokens.Mode() != ModeLegacyToken || !onWebhook || q.Get("token") == "" {
			return Identity{}, r, unauthorized(ReasonInvalid)
		}
	}
	if onWebhook && hasParam {
		if v := q.Get("token"); v != "" {
			name, scopes, found := g.tokens.Lookup(v)
			switch {
			case !found:
				return Identity{}, r, unauthorized(ReasonInvalid)
			case scoped && scopes != scopeSetOf(ScopeWebhook):
				// A URL is where credentials leak from: only one that can do little lives there.
				return Identity{}, r, unauthorized(ReasonQueryNotAllowed)
			}
			return pass(name, scopes)
		}
	}
	if g.o.cookie != nil {
		if name, present, err := g.o.cookie(r); present {
			if err != nil {
				return Identity{}, r, unauthorized(ReasonSession)
			}
			return Identity{Name: name, Scopes: scopeSetOf(ScopeAdmin), Ambient: true}, g.stripped(r, onWebhook && hasParam), nil
		}
	}
	if g.o.jwt != nil {
		_, err := g.o.jwt.Verify(r)
		switch reason := jwt.Reason(err); {
		case err == nil:
			// A verified assertion is admin on the control port: the proxy decided who may.
			return Identity{Name: "jwt", Scopes: scopeSetOf(ScopeAdmin), Ambient: true}, g.stripped(r, onWebhook && hasParam), nil
		case reason == jwt.ReasonMissing:
		case reason == "":
			return Identity{}, r, unauthorized(jwt.ReasonMalformed)
		default:
			return Identity{}, r, unauthorized(reason)
		}
	}
	return Identity{}, r, unauthorized(ReasonMissing)
}

// bearerOf returns the token of a non-empty "Authorization: Bearer" header.
func bearerOf(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") || len(h) == len("Bearer ") {
		return "", false
	}
	return h[len("Bearer "):], true
}

// stripped returns r, or when strip is set a copy whose URL and RequestURI no longer
// carry a token parameter, so no handler, log or error text downstream can read it.
func (g *gater) stripped(r *http.Request, strip bool) *http.Request {
	if !strip {
		return r
	}
	r2 := r.Clone(r.Context())
	r2.URL.RawQuery = dropParam(r.URL.RawQuery, "token")
	r2.RequestURI = r2.URL.RequestURI()
	return r2
}

// dropParam removes every pair whose decoded key is name, keeping the others as
// written.
func dropParam(raw, name string) string {
	var keep []string
	for _, pair := range strings.Split(raw, "&") {
		key, _, _ := strings.Cut(pair, "=")
		if k, err := url.QueryUnescape(key); err == nil && k == name {
			continue
		}
		if pair != "" {
			keep = append(keep, pair)
		}
	}
	return strings.Join(keep, "&")
}

// statusWriter records the status a handler wrote, for the success log line.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) {
	if w.code == 0 {
		w.code = c
	}
	w.ResponseWriter.WriteHeader(c)
}

func (w *statusWriter) status() int {
	if w.code == 0 {
		return http.StatusOK
	}
	return w.code
}

// Flush and Unwrap keep http.Flusher and http.NewResponseController working through it.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// crossOrigin reports whether r carries an Origin header whose hostname is not
// the hostname the request was sent to. Only an open port (no token) applies
// it: a page in the owner's browser could otherwise POST text/plain JSON here
// with no preflight. A request with no Origin (Plex, curl, scripts, and the
// GET navigations, <img> and <script> loads a browser sends without one) passes.
// The panel's same-origin fetch POSTs do carry Origin and pass because the
// hostnames match, so a reverse proxy in front must keep the Host header. Scheme and port are ignored on purpose: a TLS-terminating tunnel
// delivers plain http with the original Host, so the browser's https Origin
// would never match scheme://host:port. "null", an empty value and anything
// that does not parse count as cross-origin. A page on the same hostname but
// another port is not told apart.
func crossOrigin(r *http.Request) bool {
	vals, ok := r.Header["Origin"]
	if !ok {
		return false
	}
	if len(vals) != 1 {
		return true
	}
	u, err := url.Parse(vals[0])
	if err != nil || u.Hostname() == "" {
		return true
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return !strings.EqualFold(u.Hostname(), host)
}

// crossSite reports Sec-Fetch-Site: cross-site, on any method. A browser sets
// the header itself and a page cannot forge it, but only over https or on
// localhost (a "potentially trustworthy" origin): a page fetching a plain-http
// LAN address sends none, and then only the Origin check applies. same-origin,
// same-site, none and a missing header pass; the panel's own fetches are
// same-origin, also behind a TLS tunnel. A second signal, not a replacement
// for the Host allowlist.
func crossSite(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site")
}
