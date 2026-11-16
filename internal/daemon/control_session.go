package daemon

import (
	"log/slog"

	"flodni/internal/apiauth"
	"flodni/internal/apiauth/session"
)

// newControlSession builds the panel's login and logout handlers and the cookie
// verifier over the start-up token store (ADR 0004). The handlers answer 404 outside
// scoped mode. The gate takes the cookie through sess.Verify (apiauth.WithCookie), and
// a login refusal is counted in flodni_auth_refusals_total. panelHTTPS is
// auth_panel_https.
func newControlSession(auth *apiauth.Tokens, log *slog.Logger, panelHTTPS bool) *session.Handlers {
	return session.NewHandlers(auth, session.HandlerOptions{Logger: log, PanelHTTPS: panelHTTPS, Refused: refusedByHandler})
}
