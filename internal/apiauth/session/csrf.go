package session

import (
	"errors"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// CheckAmbient applies the ADR 0004 rules for a request whose credential a browser
// attaches by itself (the session cookie, the proxy's assertion), nil when it passes.
//
// A GET keeps the existing refusal of Sec-Fetch-Site: cross-site and nothing more.
// A non-GET (every state-changing control route is a POST) needs all of:
//   - an Origin whose host and port equal the request's Host. The scheme is not
//     compared, because a TLS tunnel hands Flodni plain http with the original Host,
//     except that when the panel is known to be https (TLS, X-Forwarded-Proto, or
//     auth_panel_https) the Origin must be https. A missing Origin is refused;
//   - Sec-Fetch-Site: same-origin when the header is present (a missing one passes);
//   - Content-Type: application/json, parameters allowed.
//
// A port missing from Host or Origin means the default of the scheme in question, so
// Host "panel.example" matches Origin "https://panel.example" and "http://panel.example"
// but not "https://panel.example:8443", and another port of the same host never matches.
func (m *Manager) CheckAmbient(r *http.Request) error {
	if r.Method == http.MethodGet {
		if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
			return errors.New("cross-site request")
		}
		return nil
	}
	if err := m.checkOrigin(r); err != nil {
		return err
	}
	if v, ok := r.Header["Sec-Fetch-Site"]; ok {
		if len(v) != 1 || !strings.EqualFold(strings.TrimSpace(v[0]), "same-origin") {
			return errors.New("Sec-Fetch-Site is not same-origin")
		}
	}
	if !jsonContentType(r) {
		return errors.New("Content-Type is not application/json")
	}
	return nil
}

func (m *Manager) checkOrigin(r *http.Request) error {
	vals := r.Header["Origin"]
	if len(vals) != 1 {
		return errors.New("Origin missing or repeated")
	}
	u, err := url.Parse(vals[0])
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Path != "" || u.RawQuery != "" || u.User != nil {
		return errors.New("Origin is not a web origin")
	}
	if m.isHTTPS(r) && u.Scheme != "https" {
		return errors.New("Origin is not https")
	}
	host, hport := splitHost(r.Host)
	if host == "" || !strings.EqualFold(host, u.Hostname()) {
		return errors.New("Origin host differs from Host")
	}
	oport := u.Port()
	if oport == "" {
		oport = defaultPort(u.Scheme)
	}
	if hport == "" {
		hport = defaultPort(u.Scheme) // no port on Host: the default of the Origin's scheme
	}
	if hport != oport {
		return errors.New("Origin port differs from Host")
	}
	return nil
}

func defaultPort(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}

// splitHost returns the lowercased hostname (no brackets, no trailing dot) and the
// port, "" when there is none.
func splitHost(hostport string) (host, port string) {
	host = strings.TrimSpace(hostport)
	if h, p, err := net.SplitHostPort(host); err == nil {
		host, port = h, p
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return strings.TrimSuffix(strings.ToLower(host), "."), port
}

func jsonContentType(r *http.Request) bool {
	vals := r.Header["Content-Type"]
	if len(vals) != 1 {
		return false
	}
	mt, _, err := mime.ParseMediaType(vals[0])
	return err == nil && mt == "application/json"
}

// originScheme is the scheme of a single, parsable Origin header, "" otherwise.
func originScheme(r *http.Request) string {
	vals := r.Header["Origin"]
	if len(vals) != 1 {
		return ""
	}
	u, err := url.Parse(vals[0])
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Scheme)
}
