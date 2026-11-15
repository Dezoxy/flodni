// Package apiauth is the optional bearer-token gate on the control server and
// on the engine's (GoStorm) server.
package apiauth

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
)

// Control profile: everything needs the token except the static panel page,
// which holds no data, and GET /api/health, which reports status words only. The Plex webhook cannot send headers, so only
// those two routes also take the token as ?token=; a query token elsewhere
// would leak into logs and browser history.
func controlOpen(r *http.Request) bool {
	return r.Method == http.MethodGet && (r.URL.Path == "/control" || r.URL.Path == "/api/health")
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

// Middleware gates the control server. It returns next unchanged when token is
// empty (auth off). Otherwise it answers 401 unless the request carries the
// token. The token and the credential offered are never logged.
func Middleware(token string, logger *slog.Logger, next http.Handler, opts ...Option) http.Handler {
	return gate("Control", token, logger, next, controlOpen, controlWebhook, opts)
}

// GoStorm gates the engine's server on port 8090 the same way, with only
// GET /echo open and no query token anywhere. A nil logger logs nothing.
func GoStorm(token string, logger *slog.Logger, next http.Handler, opts ...Option) http.Handler {
	return gate("GoStorm", token, logger, next, gostormOpen, never, opts)
}

// Option tunes a gate. The zero set is today's behaviour.
type Option func(*options)

type options struct{ hosts map[string]bool }

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

func gate(name, token string, logger *slog.Logger, next http.Handler, open, queryToken func(*http.Request) bool, opts []Option) http.Handler {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	inner := gateToken(name, token, logger, next, open, queryToken)
	if o.hosts == nil {
		return inner
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(r, o.hosts) {
			refuse(w, r, name, logger, "host not allowed", "host not allowed")
			return
		}
		inner.ServeHTTP(w, r)
	})
}

func refuse(w http.ResponseWriter, r *http.Request, name string, logger *slog.Logger, what, body string) {
	if logger != nil {
		logger.Info(fmt.Sprintf("[%s] 403 %s %s %s from %s (Host %q)", name, what, r.Method, r.URL.Path, r.RemoteAddr, r.Host), "name", name, "method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr, "host", r.Host)
	}
	http.Error(w, body, http.StatusForbidden)
}

func gateToken(name, token string, logger *slog.Logger, next http.Handler, open, queryToken func(*http.Request) bool) http.Handler {
	if token == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if crossOrigin(r) {
				refuse(w, r, name, logger, "cross-origin", "cross-origin request refused")
				return
			}
			if crossSite(r) && !open(r) {
				refuse(w, r, name, logger, "cross-site", "cross-site request refused")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if open(r) || authorized(r, want, queryToken) {
			next.ServeHTTP(w, r)
			return
		}
		if logger != nil {
			logger.Info(fmt.Sprintf("[%s] 401 %s %s from %s", name, r.Method, r.URL.Path, r.RemoteAddr), "name", name, "method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr)
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

func authorized(r *http.Request, want []byte, queryToken func(*http.Request) bool) bool {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if equal([]byte(h[len("Bearer "):]), want) {
			return true
		}
	}
	if queryToken(r) {
		return equal([]byte(r.URL.Query().Get("token")), want)
	}
	return false
}

func equal(got, want []byte) bool {
	return len(got) == len(want) && subtle.ConstantTimeCompare(got, want) == 1
}

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
