package daemon

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"flodni/internal/apiauth"
	"flodni/internal/apiauth/authtest"
	"flodni/internal/apiauth/jwt"
	"flodni/internal/apiauth/session"
	"flodni/internal/flog"
)

// countingJWT is a proxy verifier that records being asked and always says "no header".
type countingJWT struct{ calls int }

func (c *countingJWT) Verify(*http.Request) (jwt.Identity, error) {
	c.calls++
	return jwt.Identity{}, &jwt.Error{Reason: jwt.ReasonMissing}
}

// cookieGate is the control server as main builds it, in scoped mode, with the real
// login and logout handlers, the real cookie hook and a counting proxy verifier. Every
// other handler is a stub that records being reached.
type cookieGate struct {
	h       http.Handler
	sess    *session.Handlers
	fx      *authtest.Fixture
	jwt     *countingJWT
	reached *[]string
	logs    *bytes.Buffer
}

func newCookieGate(t *testing.T, fx *authtest.Fixture) *cookieGate {
	t.Helper()
	if fx == nil {
		fx = authtest.New(t)
	}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	body := `{"log_level":"info","gostorm_url":"http://127.0.0.1:8090","auth_tokens_file":` + jsonString(fx.Path) + `}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MKV_PROXY_CONFIG_PATH", cfgPath)
	t.Setenv("FLODNI_API_TOKEN", "")
	t.Setenv("FLODNI_AUTH_TOKENS_FILE", "")
	a := newTestApp(t)
	a.auth = fx.Tokens
	prevCfg, prevLog := globalConfig.Load(), logger
	cfg := loadCfg(t)
	globalConfig.Store(&cfg)
	logs := &bytes.Buffer{}
	logger = flog.NewText(logs)
	t.Cleanup(func() { globalConfig.Store(prevCfg); logger = prevLog })

	var reached []string
	var h controlHandlers
	v := reflect.ValueOf(&h).Elem()
	stub := func(w http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}
	for i := 0; i < v.NumField(); i++ {
		v.Field(i).Set(reflect.ValueOf(stub))
	}
	sess := newControlSession(fx.Tokens, logger, false)
	h.Login, h.Logout = sess.Login, sess.Logout
	routes := apiauth.NewRegistrar()
	registerControlRoutes(routes, h)
	j := &countingJWT{}
	return &cookieGate{
		h:    newControlGate(routes, fx.Tokens, j, gc(), apiauth.WithCookie(sess.Verify)),
		sess: sess, fx: fx, jwt: j, reached: &reached, logs: logs,
	}
}

func (g *cookieGate) do(method, target, body string, hdr map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = "panel.example:9080"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	g.h.ServeHTTP(rec, req)
	return rec
}

// ambient is what the panel's own fetch sends: same origin, JSON.
var ambient = map[string]string{"Origin": "http://panel.example:9080", "Content-Type": "application/json"}

func (g *cookieGate) login(t *testing.T, token string) *http.Cookie {
	t.Helper()
	rec := g.do("POST", "/control/login", `{"token":"`+token+`"}`, ambient)
	if rec.Code != http.StatusNoContent || len(rec.Result().Cookies()) != 1 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	return rec.Result().Cookies()[0]
}

func TestCookieSignsInThroughTheGate(t *testing.T) {
	g := newCookieGate(t, nil)
	ck := g.login(t, g.fx.Token["admin"])
	if rec := g.do("GET", "/api/sessions", "", nil, ck); rec.Code != http.StatusNoContent {
		t.Fatalf("GET with the cookie: %d", rec.Code)
	}
	// admin reaches the admin routes too, and the CSRF rules apply to the POST
	if rec := g.do("POST", "/api/restart", "", ambient, ck); rec.Code != http.StatusNoContent {
		t.Errorf("POST with the cookie and the panel's headers: %d", rec.Code)
	}
	for name, hdr := range map[string]map[string]string{
		"no Origin":    {"Content-Type": "application/json"},
		"text/plain":   {"Origin": ambient["Origin"], "Content-Type": "text/plain"},
		"another port": {"Origin": "http://panel.example:1", "Content-Type": "application/json"},
		"same-site":    {"Origin": ambient["Origin"], "Content-Type": "application/json", "Sec-Fetch-Site": "same-site"},
	} {
		before := len(*g.reached)
		rec := g.do("POST", "/api/restart", "", hdr, ck)
		if rec.Code != http.StatusForbidden || len(*g.reached) != before {
			t.Errorf("%s: %d, handler reached=%v, want 403 and not reached", name, rec.Code, len(*g.reached) != before)
		}
	}
}

// A wrong cookie is refused as "session" with the sign-in header, and the proxy's
// assertion is not asked: a present credential that is wrong never falls through.
func TestWrongCookieIsRefusedWithoutFallingThrough(t *testing.T) {
	g := newCookieGate(t, nil)
	good := g.login(t, g.fx.Token["admin"])
	for name, ck := range map[string]*http.Cookie{
		"junk":     {Name: session.CookieName, Value: "junk"},
		"tampered": {Name: session.CookieName, Value: good.Value[:len(good.Value)-3] + "AAA"},
	} {
		before := len(*g.reached)
		calls := g.jwt.calls
		rec := g.do("GET", "/api/sessions", "", nil, ck)
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("Flodni-Auth") != "login" {
			t.Errorf("%s: %d Flodni-Auth %q, want 401 login", name, rec.Code, rec.Header().Get("Flodni-Auth"))
		}
		if g.jwt.calls != calls || len(*g.reached) != before {
			t.Errorf("%s: the verifier was asked or the handler reached after a wrong cookie", name)
		}
	}
	found := false
	for _, c := range authRefusals.Snapshot() {
		if c.Reason == "session" {
			found = true
		}
	}
	if !found {
		t.Error("the refusals were not counted with reason session")
	}
	if strings.Contains(g.logs.String(), good.Value) {
		t.Error("a cookie value reached the log")
	}
}

// The engine ignores the cookie: it is a control-port credential only.
func TestEngineGateIgnoresTheCookie(t *testing.T) {
	g := newCookieGate(t, nil)
	ck := g.login(t, g.fx.Token["admin"])
	called := false
	h := apiauth.GoStorm("", nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }),
		apiauth.WithTokens(g.fx.Tokens), apiauth.WithCookie(g.sess.Verify))
	req := httptest.NewRequest("POST", "/torrents", strings.NewReader(`{"action":"list"}`))
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || called {
		t.Errorf("engine with a cookie: %d called=%v, want 401", rec.Code, called)
	}
	if rec.Header().Get("Flodni-Auth") != "" {
		t.Error("the engine answered a sign-in header")
	}
}

// Login is an open route: a stale or junk cookie beside it changes nothing, so a
// restart cannot lock the browser out of signing in again.
func TestLoginIsReachableWithAStaleCookie(t *testing.T) {
	g := newCookieGate(t, nil)
	stale := &http.Cookie{Name: session.CookieName, Value: "junk"}
	rec := g.do("POST", "/control/login", `{"token":"`+g.fx.Token["admin"]+`"}`, ambient, stale)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("login with a junk cookie: %d", rec.Code)
	}
}

// A restart makes a new key: the old cookie is a 401 with the sign-in header, and the
// browser signs in again.
func TestRestartInvalidatesTheOldCookie(t *testing.T) {
	g1 := newCookieGate(t, nil)
	old := g1.login(t, g1.fx.Token["admin"])
	g2 := newCookieGate(t, g1.fx)
	rec := g2.do("GET", "/api/sessions", "", nil, old)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("Flodni-Auth") != "login" {
		t.Fatalf("old cookie after a restart: %d Flodni-Auth %q", rec.Code, rec.Header().Get("Flodni-Auth"))
	}
	fresh := g2.login(t, g2.fx.Token["admin"])
	if rec := g2.do("GET", "/api/sessions", "", nil, fresh); rec.Code != http.StatusNoContent {
		t.Errorf("fresh cookie: %d", rec.Code)
	}
}

// A token without admin cannot sign in, and a cookie never grants more than admin's
// own routes: login answers 403 for it.
func TestLoginNeedsAnAdminTokenThroughTheGate(t *testing.T) {
	g := newCookieGate(t, nil)
	rec := g.do("POST", "/control/login", `{"token":"`+g.fx.Token["read"]+`"}`, ambient)
	if rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 {
		t.Errorf("a read-only token: %d, want 403 and no cookie", rec.Code)
	}
	var invalid, scope bool
	rec = g.do("POST", "/control/login", `{"token":"`+g.fx.Unknown()+`"}`, ambient)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("an unknown token: %d", rec.Code)
	}
	for _, c := range authRefusals.Snapshot() {
		if c.Route == "/control/login" && c.Reason == "invalid" {
			invalid = true
		}
		if c.Route == "/control/login" && c.Reason == "scope" {
			scope = true
		}
	}
	if !invalid || !scope {
		t.Errorf("login refusals counted: invalid=%v scope=%v, want both", invalid, scope)
	}
}

// Logout through the gate: the credential's kind decides the CSRF rules, an empty
// bearer is no credential, and Basic is none of ours.
func TestLogoutThroughTheGate(t *testing.T) {
	g := newCookieGate(t, nil)
	ck := g.login(t, g.fx.Token["admin"])
	// no credential at all: an empty bearer, Basic, nothing
	for name, hdr := range map[string]map[string]string{
		"empty bearer": {"Authorization": "Bearer ", "Origin": ambient["Origin"], "Content-Type": "application/json"},
		"basic":        {"Authorization": "Basic x", "Origin": ambient["Origin"], "Content-Type": "application/json"},
		"nothing":      ambient,
	} {
		if rec := g.do("POST", "/control/logout", "", hdr); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, rec.Code)
		}
	}
	// the cookie with an empty bearer or Basic beside it is still the cookie: CSRF applies
	for name, auth := range map[string]string{"empty bearer": "Bearer ", "basic": "Basic x"} {
		rec := g.do("POST", "/control/logout", "", map[string]string{"Authorization": auth, "Content-Type": "application/json"}, ck)
		if rec.Code != http.StatusForbidden {
			t.Errorf("cookie and %s with no Origin: %d, want 403", name, rec.Code)
		}
		if rec := g.do("GET", "/api/sessions", "", nil, ck); rec.Code != http.StatusNoContent {
			t.Fatalf("a refused logout still revoked (%s): %d", name, rec.Code)
		}
	}
	// a bearer needs no Origin
	if rec := g.do("POST", "/control/logout", "", map[string]string{"Authorization": "Bearer " + g.fx.Token["admin"]}); rec.Code != http.StatusNoContent {
		t.Errorf("bearer logout: %d, want 204", rec.Code)
	}
	if rec := g.do("GET", "/api/sessions", "", nil, ck); rec.Code != http.StatusUnauthorized || rec.Header().Get("Flodni-Auth") != "login" {
		t.Errorf("the cookie after logout: %d", rec.Code)
	}
	// a cookie logout with the panel's headers
	ck2 := g.login(t, g.fx.Token["admin"])
	if rec := g.do("POST", "/control/logout", "", ambient, ck2); rec.Code != http.StatusNoContent {
		t.Errorf("cookie logout: %d", rec.Code)
	}
	// a token without admin cannot log everyone out
	if rec := g.do("POST", "/control/logout", "", map[string]string{"Authorization": "Bearer " + g.fx.Token["read"]}); rec.Code != http.StatusForbidden {
		t.Errorf("read-token logout: %d, want 403", rec.Code)
	}
}
