package daemon

import "net/http"

// handlePanel serves GET /control, the static panel page. The page holds no data, but
// it is where a signed-in operator clicks, so no other origin may frame it: the
// header pair is the same refusal for current and older browsers (ADR 0004,
// threat model T-rows on the panel).
func handlePanel(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html")
	h.Set("Content-Security-Policy", "frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY")
	_, _ = w.Write(settingsHTML)
}
