package apiauth

import (
	"errors"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// A request authenticated by a cookie or by the proxy's assertion is one a browser
// sends by itself, so a forged form post carries it too. A state-changing request
// (anything but GET) then needs all three of: an Origin whose host and port are the
// request's Host, Sec-Fetch-Site same-origin when the header is there, and a JSON
// content type. The open port's hostname-only Origin check is not enough here:
// cookies and SameSite ignore ports, so a page on another port of the same host is
// "same site" and would send the cookie. A GET keeps the cross-site refusal only.
//
// It returns the refusal reason, or "" when the request passes.
//
// CheckAmbient is the one implementation of these rules: the gate applies it to a
// cookie or assertion, and the session package's login calls it (that package imports
// apiauth, never the reverse).
func CheckAmbient(r *http.Request, panelHTTPS bool) error {
	if reason := ambientRefusal(r, panelHTTPS); reason != "" {
		return errors.New("ambient credential refused: " + reason)
	}
	return nil
}

func ambientRefusal(r *http.Request, panelHTTPS bool) string {
	if r.Method == http.MethodGet {
		for _, v := range r.Header.Values("Sec-Fetch-Site") { // every value counts, not the first only
			if strings.EqualFold(strings.TrimSpace(v), "cross-site") {
				return ReasonCrossSite
			}
		}
		return ""
	}
	if !originMatchesHost(r, panelHTTPS) {
		return ReasonCSRF
	}
	if vals := r.Header.Values("Sec-Fetch-Site"); len(vals) > 0 {
		if len(vals) != 1 || !strings.EqualFold(strings.TrimSpace(vals[0]), "same-origin") {
			return ReasonCSRF
		}
	}
	cts := r.Header.Values("Content-Type")
	if len(cts) != 1 {
		return ReasonCSRF
	}
	if mt, _, err := mime.ParseMediaType(cts[0]); err != nil || mt != "application/json" {
		return ReasonCSRF
	}
	return ""
}

// originMatchesHost reports whether the request's single Origin names the host and
// port it was sent to. The scheme is not compared, because a TLS tunnel hands Flodni
// plain http with the original Host, except when the panel is known to be https (the
// request came over TLS, X-Forwarded-Proto says https, or auth_panel_https): then
// the Origin must be https too. A missing port means the default of its own scheme
// (an Origin) or of the Origin's scheme (a Host), so a tunnel's Host panel.example
// matches Origin https://panel.example, and another port on the host does not.
func originMatchesHost(r *http.Request, panelHTTPS bool) bool {
	vals := r.Header.Values("Origin")
	if len(vals) != 1 {
		return false // missing: browsers send Origin on a POST, scripts use a bearer token
	}
	u, err := url.Parse(vals[0])
	if err != nil || u.User != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	// Exactly scheme://host[:port]: no bare "?" or "#", which url.Parse drops silently.
	if vals[0] != u.Scheme+"://"+u.Host {
		return false
	}
	if scheme != "https" && knownHTTPS(r, panelHTTPS) {
		return false
	}
	oHost, oPort := strings.TrimSuffix(strings.ToLower(u.Hostname()), "."), u.Port()
	if oPort == "" {
		oPort = defaultPort(scheme)
	}
	hHost, hPort := splitHostPort(r.Host)
	if hPort == "" {
		hPort = defaultPort(scheme)
	}
	return oHost != "" && oHost == hHost && oPort == hPort
}

func defaultPort(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}

// splitHostPort lower-cases a Host, strips the brackets of an IPv6 literal and one
// trailing dot, and returns the port ("" when there is none).
func splitHostPort(hostport string) (host, port string) {
	host = strings.TrimSpace(hostport)
	if h, p, err := net.SplitHostPort(host); err == nil {
		host, port = h, p
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return strings.TrimSuffix(strings.ToLower(host), "."), port
}
