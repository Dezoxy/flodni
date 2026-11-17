package apiauth

import (
	"net/http"
	"strings"
)

// withSecurityHeaders sets the response headers every control-port answer carries,
// whoever is asking and whatever the answer is (served, refused, unrouted, open
// route). It is the outermost layer of the control gate, so no route can miss it.
//
//   - X-Content-Type-Options: nosniff, so a JSON or text body is never read as HTML.
//   - Referrer-Policy: same-origin, so a link out of the panel leaks nothing; the
//     panel's own same-origin requests keep their Referer.
//   - Strict-Transport-Security, only when the request is known to be https (TLS,
//     the first X-Forwarded-Proto element, or auth_panel_https). Over plain http the
//     browser ignores it, and it would pin a host that has no https.
//
// The panel's frame refusal stays in the panel handler. No Content-Security-Policy is
// added here: the panel page runs an inline script, which a script-src would break.
func withSecurityHeaders(panelHTTPS bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		if knownHTTPS(r, panelHTTPS) {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// knownHTTPS: the same test originMatchesHost uses to hold an Origin to https.
func knownHTTPS(r *http.Request, panelHTTPS bool) bool {
	fwd, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	return panelHTTPS || r.TLS != nil || strings.EqualFold(strings.TrimSpace(fwd), "https")
}
