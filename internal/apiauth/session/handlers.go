package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"flodni/internal/apiauth"
)

const (
	loginRoute  = "/control/login"
	logoutRoute = "/control/logout"
	// maxLoginBody bounds the login body: a token is 46 bytes, so 4 KiB is generous.
	maxLoginBody = 4 << 10
)

// HandlerOptions configures NewHandlers.
type HandlerOptions struct {
	// Logger gets one line per refusal (method, quoted path, remote address, reason) and
	// one per success (the token entry's name). It never gets a token, a cookie or a
	// header value. Nil logs nothing.
	Logger *slog.Logger
	// Refused is told each refusal by route pattern and reason ("invalid", "scope",
	// "csrf"), for the refusal metric. Nil is fine.
	Refused func(route, reason string)
	// PanelHTTPS is auth_panel_https.
	PanelHTTPS bool
}

// Handlers are POST /control/login and POST /control/logout over one Manager. They
// exist only in scoped mode: in open and api_token-only mode they answer 404, which is
// what tells the panel to keep its bearer prompt.
type Handlers struct {
	m          *Manager // unexported: Issue would mint an admin cookie for any name
	tokens     *apiauth.Tokens
	log        *slog.Logger
	refuse     func(route, reason string)
	panelHTTPS bool
}

// Verify is the gate's cookie hook (apiauth.WithCookie): see Manager.Verify.
func (h *Handlers) Verify(r *http.Request) (name string, present bool, err error) {
	return h.m.Verify(r)
}

// NewHandlers builds the manager (a fresh random key) and the two handlers. The cookie
// names its token entry, re-checked against tokens on every Verify: the entry must
// still exist and still hold admin.
func NewHandlers(tokens *apiauth.Tokens, o HandlerOptions) *Handlers {
	m, err := New(Options{
		PanelHTTPS: o.PanelHTTPS,
		Active: func(name string) bool {
			s, ok := tokens.ScopesOf(name)
			return ok && s.Has(apiauth.ScopeAdmin)
		},
	})
	if err != nil { // crypto/rand failed: nothing safe to do
		panic("session: " + err.Error())
	}
	return &Handlers{m: m, tokens: tokens, log: o.Logger, refuse: o.Refused, panelHTTPS: o.PanelHTTPS}
}

// Login handles POST /control/login with {"token":"..."}. The token must hold admin: one
// that does not answers 403, a wrong one 401. Success sets the cookie and answers 204.
// It is an ambient-credential-style request, so it needs the same Origin,
// Sec-Fetch-Site and Content-Type rules as any cookie-authenticated POST.
func (h *Handlers) Login(w http.ResponseWriter, r *http.Request) {
	if !h.allowed(w, r) {
		return
	}
	if err := apiauth.CheckAmbient(r, h.panelHTTPS); err != nil {
		h.refused(w, r, loginRoute, http.StatusForbidden, "csrf", "cross-site request refused")
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLoginBody))
	dec.DisallowUnknownFields()
	err := dec.Decode(&in)
	if err == nil {
		if _, err2 := dec.Token(); err2 != io.EOF {
			err = errors.New("trailing data")
		}
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "body too large")
			return
		}
		writeError(w, http.StatusBadRequest, `body must be {"token":"..."}`)
		return
	}
	name, scopes, ok := h.tokens.Lookup(in.Token)
	if !ok {
		h.refused(w, r, loginRoute, http.StatusUnauthorized, "invalid", "invalid token")
		return
	}
	if !scopes.Has(apiauth.ScopeAdmin) {
		h.refused(w, r, loginRoute, http.StatusForbidden, "scope", "this token cannot sign in to the panel")
		return
	}
	h.m.Issue(w, r, name)
	w.Header().Set("Cache-Control", "no-store")
	if h.log != nil {
		h.log.Info(fmt.Sprintf("[Control] panel sign-in by token %q from %s", name, r.RemoteAddr), "name", name, "remote_addr", r.RemoteAddr)
	}
	w.WriteHeader(http.StatusNoContent)
}

// Logout handles POST /control/logout: it revokes every session at once (a process-wide
// generation counter, so a stolen cookie dies too) and clears the caller's cookie.
// The gate decides who may call it (admin) and which credential authenticated: a
// bearer needs no CSRF check (a page cannot set that header), and a cookie or an
// assertion has already had the ambient-credential rules applied. With no identity in
// the context (the handler reached without the gate) it fails closed and applies the
// rules itself, so no header of its own can exempt a request.
func (h *Handlers) Logout(w http.ResponseWriter, r *http.Request) {
	if !h.allowed(w, r) {
		return
	}
	if _, ok := apiauth.IdentityFrom(r.Context()); !ok {
		if err := apiauth.CheckAmbient(r, h.panelHTTPS); err != nil {
			h.refused(w, r, logoutRoute, http.StatusForbidden, "csrf", "cross-site request refused")
			return
		}
	}
	h.m.Revoke(w, r)
	w.Header().Set("Cache-Control", "no-store")
	if h.log != nil {
		h.log.Info(fmt.Sprintf("[Control] panel sessions revoked from %s", r.RemoteAddr), "remote_addr", r.RemoteAddr)
	}
	w.WriteHeader(http.StatusNoContent)
}

// allowed answers 404 outside scoped mode and 405 for a method other than POST.
func (h *Handlers) allowed(w http.ResponseWriter, r *http.Request) bool {
	if h.tokens.Mode() != apiauth.ModeScoped {
		http.NotFound(w, r)
		return false
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
	return true
}

func (h *Handlers) refused(w http.ResponseWriter, r *http.Request, route string, code int, reason, msg string) {
	if h.refuse != nil {
		h.refuse(route, reason)
	}
	if h.log != nil {
		h.log.Info(fmt.Sprintf("[Control] %d %s %q from %s: %s", code, r.Method, r.URL.Path, r.RemoteAddr, reason),
			"method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr, "reason", reason)
	}
	writeError(w, code, msg)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(b)
}
