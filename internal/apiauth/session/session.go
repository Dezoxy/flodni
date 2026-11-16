// Package session is the panel's login cookie (ADR 0004, "Session cookie"): a signed,
// expiring, revocable value that lets a person use the control panel without a bearer
// token in the browser. It is self-contained: the gate calls Manager.Verify through
// a plain function hook, and nothing here reads the route table or the gate.
//
// The cookie holds base64url(name|expiry|generation) and an HMAC-SHA256 tag over it,
// joined by a dot. The key is 32 random bytes made at start and held in memory only, so
// a restart signs everyone out and the key is tied to no file. A process-wide
// generation counter is part of what is signed; logout increments it, which
// invalidates every cookie issued before it, a stolen copy included.
package session

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// CookieName is the session cookie.
const CookieName = "flodni_session"

// Lifetime is the absolute lifetime of a cookie. There is no sliding window: a stolen
// cookie has a bound, and using one never extends it.
const Lifetime = 12 * time.Hour

// maxValue bounds what Verify will even decode; a real value is under 150 bytes.
const maxValue = 512

// ErrSession is wrapped by every refusal of a cookie that was present: expired,
// tampered, from before a logout or a restart, or for an entry that is gone. The gate
// counts all of them as reason "session" and does not try another credential.
var ErrSession = errors.New("session cookie refused")

// Options configures a Manager.
type Options struct {
	// PanelHTTPS is auth_panel_https: the panel is served over https even where the
	// request does not say so (a proxy that sends no X-Forwarded-Proto).
	PanelHTTPS bool
	// Active reports whether the token entry called name may still hold a session (it
	// exists and holds admin). It is called on every Verify. Required.
	Active func(name string) bool
	// Now is the clock; nil means time.Now. Tests set it.
	Now func() time.Time
}

// Manager issues and verifies session cookies. It is safe for concurrent use.
type Manager struct {
	key        []byte
	gen        atomic.Uint64
	panelHTTPS bool
	active     func(string) bool
	now        func() time.Time
}

// New makes a Manager with a fresh random key. It never reads a key from anywhere.
func New(o Options) (*Manager, error) {
	if o.Active == nil {
		return nil, errors.New("session: Options.Active is required")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	m := &Manager{key: key, panelHTTPS: o.PanelHTTPS, active: o.Active, now: o.Now}
	if m.now == nil {
		m.now = time.Now
	}
	return m, nil
}

var enc = base64.RawURLEncoding

func (m *Manager) tag(payload string) string {
	h := hmac.New(sha256.New, m.key)
	h.Write([]byte(payload))
	return enc.EncodeToString(h.Sum(nil))
}

// Issue sets a cookie for the token entry called name. Secure follows the ADR: it is
// set when the request arrived over TLS, when the login request's Origin is https,
// when a proxy said X-Forwarded-Proto: https, or when auth_panel_https is true.
func (m *Manager) Issue(w http.ResponseWriter, r *http.Request, name string) {
	exp := m.now().Add(Lifetime)
	payload := enc.EncodeToString([]byte(name + "|" + strconv.FormatInt(exp.Unix(), 10) + "|" + strconv.FormatUint(m.gen.Load(), 10)))
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    payload + "." + m.tag(payload),
		Path:     "/",
		Expires:  exp,
		MaxAge:   int(Lifetime / time.Second),
		HttpOnly: true,
		Secure:   m.secure(r),
		SameSite: http.SameSiteStrictMode,
	})
}

func (m *Manager) secure(r *http.Request) bool {
	return m.isHTTPS(r) || originScheme(r) == "https"
}

// isHTTPS reports that the panel is known to be served over https: the request came
// over TLS, a proxy said so, or the owner configured it.
func (m *Manager) isHTTPS(r *http.Request) bool {
	if m.panelHTTPS || r.TLS != nil {
		return true
	}
	first, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	return strings.EqualFold(strings.TrimSpace(first), "https")
}

// Revoke is logout: it increments the generation, so every cookie issued before it
// stops verifying, and clears the caller's cookie.
func (m *Manager) Revoke(w http.ResponseWriter) {
	m.gen.Add(1)
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// Verify is the hook the gate calls for the cookie credential, control port only. The
// result is the token entry's name; present is false when there is no cookie (or an
// empty one, which is what a cleared cookie looks like), so the gate moves on to the
// next credential. present with a non-nil error means a cookie was sent and is wrong:
// the gate refuses it with reason "session" and does not fall through. A verified
// cookie stands for an admin entry, re-checked against the store here on every call.
func (m *Manager) Verify(r *http.Request) (name string, present bool, err error) {
	var value string
	n := 0
	for _, c := range r.Cookies() {
		if c.Name == CookieName {
			value = c.Value
			n++
		}
	}
	if n == 0 || (n == 1 && value == "") {
		return "", false, nil
	}
	if n > 1 {
		return "", true, ErrSession // ambiguous: never "the first one wins"
	}
	name, err = m.open(value)
	if err != nil {
		return "", true, err
	}
	return name, true, nil
}

func (m *Manager) open(value string) (string, error) {
	if len(value) > maxValue {
		return "", ErrSession
	}
	payload, tag, ok := strings.Cut(value, ".")
	if !ok || strings.Contains(tag, ".") || payload == "" || tag == "" {
		return "", ErrSession
	}
	if !hmac.Equal([]byte(tag), []byte(m.tag(payload))) {
		return "", ErrSession
	}
	raw, err := enc.DecodeString(payload)
	if err != nil {
		return "", ErrSession
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 {
		return "", ErrSession
	}
	exp, err1 := strconv.ParseInt(parts[1], 10, 64)
	gen, err2 := strconv.ParseUint(parts[2], 10, 64)
	if err1 != nil || err2 != nil || gen != m.gen.Load() {
		return "", ErrSession
	}
	if !m.now().Before(time.Unix(exp, 0)) {
		return "", ErrSession
	}
	if !m.active(parts[0]) {
		return "", ErrSession
	}
	return parts[0], nil
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
