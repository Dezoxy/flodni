package session

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"flodni/internal/apiauth"
)

// Tokens are generated at run time so no fixture holds a secret-shaped literal.
type fixture struct {
	h                      *Handlers
	log                    *bytes.Buffer
	admin, reader          string // the two tokens
	refused                *[]string
	tokens                 *apiauth.Tokens
	adminBearer, adminName string
}

func scopedTokens(t *testing.T) (tk *apiauth.Tokens, admin, reader string) {
	t.Helper()
	admin, adminHash, err := apiauth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	reader, readerHash, err := apiauth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	body := `{"tokens":[{"name":"owner","scopes":["admin"],"sha256":"` + adminHash + `"},{"name":"viewer","scopes":["read"],"sha256":"` + readerHash + `"}]}`
	p := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	tk, err = apiauth.Load(apiauth.LoadOptions{TokensFile: p, GoStormURL: "http://127.0.0.1:8090"})
	if err != nil {
		t.Fatal(err)
	}
	return tk, admin, reader
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	tk, admin, reader := scopedTokens(t)
	f := &fixture{log: &bytes.Buffer{}, admin: admin, reader: reader, tokens: tk}
	var mu sync.Mutex
	var refused []string
	f.refused = &refused
	f.h = NewHandlers(tk, HandlerOptions{
		Logger: slog.New(slog.NewTextHandler(f.log, nil)),
		Refused: func(route, reason string) {
			mu.Lock()
			refused = append(refused, route+" "+reason)
			mu.Unlock()
		},
	})
	return f
}

func loginReq(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/control/login", strings.NewReader(body))
	r.Host = "panel.lan:9080"
	r.Header.Set("Origin", "http://panel.lan:9080")
	r.Header.Set("Content-Type", "application/json")
	return r
}

func tokenBody(tok string) string {
	b, _ := json.Marshal(map[string]string{"token": tok})
	return string(b)
}

func do(h http.HandlerFunc, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

func TestLoginIssuesTheCookieForAnAdminToken(t *testing.T) {
	f := newFixture(t)
	rec := do(f.h.Login, loginReq(tokenBody(f.admin)))
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("status %d body %q, want 204 and no body", rec.Code, rec.Body.String())
	}
	cs := rec.Result().Cookies()
	if len(cs) != 1 || cs[0].Name != CookieName || !cs[0].HttpOnly || cs[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookies %+v", cs)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", cc)
	}
	name, present, err := f.h.Verify(withCookie(cs[0]))
	if name != "owner" || !present || err != nil {
		t.Errorf("Verify = %q %v %v, want owner", name, present, err)
	}
}

func TestLoginRefusals(t *testing.T) {
	f := newFixture(t)
	other, _, _ := apiauth.GenerateToken() // well-formed, not in the file
	cases := []struct {
		name   string
		req    *http.Request
		code   int
		reason string
	}{
		{"a token without admin", loginReq(tokenBody(f.reader)), 403, "scope"},
		{"a well-formed unknown token", loginReq(tokenBody(other)), 401, "invalid"},
		{"a token without the fl_ shape", loginReq(tokenBody("owner-chosen-secret")), 401, "invalid"},
		{"an empty token", loginReq(tokenBody("")), 401, "invalid"},
		{"a missing token", loginReq(`{}`), 401, "invalid"},
		{"a 3000 byte token", loginReq(tokenBody(strings.Repeat("fl_", 1000))), 401, "invalid"},
		{"an oversized body", loginReq(`{"token":"` + strings.Repeat("a", 64<<10) + `"}`), 413, ""},
		{"not json", loginReq(`token=abc`), 400, ""},
		{"a json string", loginReq(`"x"`), 400, ""},
		{"a numeric token", loginReq(`{"token":5}`), 400, ""},
		{"an unknown field", loginReq(`{"token":"x","admin":true}`), 400, ""},
		{"trailing data", loginReq(tokenBody(f.admin) + `{}`), 400, ""},
		{"an empty body", loginReq(``), 400, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(f.h.Login, tc.req)
			if rec.Code != tc.code {
				t.Errorf("status %d, want %d (%s)", rec.Code, tc.code, rec.Body.String())
			}
			if len(rec.Result().Cookies()) != 0 {
				t.Error("a refusal set a cookie")
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("error Content-Type %q, want JSON", ct)
			}
			var e struct{ Error string }
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error == "" {
				t.Errorf("body %q is not {\"error\":...}", rec.Body.String())
			}
		})
	}
	want := []string{"scope", "invalid", "invalid", "invalid", "invalid", "invalid"}
	var got []string
	for _, e := range *f.refused {
		got = append(got, strings.TrimPrefix(e, "/control/login "))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("refusal reasons %v, want %v", got, want)
	}
}

// Login is an ambient-style request: it needs the same Origin, Sec-Fetch-Site and
// Content-Type rules, and a refused one is not even looked at as a token.
func TestLoginCSRF(t *testing.T) {
	f := newFixture(t)
	for name, mod := range map[string]func(*http.Request){
		"no Origin":    func(r *http.Request) { r.Header.Del("Origin") },
		"another port": func(r *http.Request) { r.Header.Set("Origin", "http://panel.lan:8080") },
		"another host": func(r *http.Request) { r.Header.Set("Origin", "http://evil.example:9080") },
		"text/plain":   func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		"same-site":    func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") },
		"cross-site":   func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"none":         func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "none") },
	} {
		t.Run(name, func(t *testing.T) {
			before := len(*f.refused)
			r := loginReq(tokenBody(f.admin))
			mod(r)
			rec := do(f.h.Login, r)
			if rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 {
				t.Errorf("status %d, cookies %d, want 403 and none", rec.Code, len(rec.Result().Cookies()))
			}
			if got := (*f.refused)[before:]; len(got) != 1 || got[0] != "/control/login csrf" {
				t.Errorf("refusals %v, want one csrf", got)
			}
		})
	}
	// The TLS tunnel case: no port on Host, https Origin, and the cookie is Secure.
	r := loginReq(tokenBody(f.admin))
	r.Host = "panel.example"
	r.Header.Set("Origin", "https://panel.example")
	rec := do(f.h.Login, r)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("tunnel login: %d", rec.Code)
	}
	if cs := rec.Result().Cookies(); len(cs) != 1 || !cs[0].Secure {
		t.Errorf("tunnel cookie %+v, want Secure", cs)
	}
}

func TestLoginMethodAndMode(t *testing.T) {
	f := newFixture(t)
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodHead} {
		r := loginReq("")
		r.Method = m
		rec := do(f.h.Login, r)
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
			t.Errorf("%s: status %d Allow %q", m, rec.Code, rec.Header().Get("Allow"))
		}
	}
	// Only scoped mode has these routes: open, legacy_token and a nil store answer 404.
	legacy, err := apiauth.Load(apiauth.LoadOptions{APIToken: "owner-chosen"})
	if err != nil {
		t.Fatal(err)
	}
	open, err := apiauth.Load(apiauth.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for name, tk := range map[string]*apiauth.Tokens{"legacy": legacy, "open": open, "nil": nil} {
		h := NewHandlers(tk, HandlerOptions{})
		for route, fn := range map[string]http.HandlerFunc{"login": h.Login, "logout": h.Logout} {
			rec := do(fn, loginReq(tokenBody("owner-chosen")))
			if rec.Code != http.StatusNotFound || len(rec.Result().Cookies()) != 0 {
				t.Errorf("%s %s: status %d, want 404", name, route, rec.Code)
			}
		}
	}
}

func logoutReq() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/control/logout", nil)
	r.Host = "panel.lan:9080"
	r.Header.Set("Origin", "http://panel.lan:9080")
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestLogoutRevokesAndClears(t *testing.T) {
	f := newFixture(t)
	login := do(f.h.Login, loginReq(tokenBody(f.admin)))
	ck := login.Result().Cookies()[0]
	r := logoutReq()
	r.AddCookie(ck)
	rec := do(f.h.Logout, r)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", rec.Code)
	}
	if cs := rec.Result().Cookies(); len(cs) != 1 || cs[0].Value != "" || cs[0].MaxAge >= 0 {
		t.Errorf("logout cookie %+v, want cleared", cs)
	}
	if _, present, err := f.h.Verify(withCookie(ck)); !present || err == nil {
		t.Errorf("the cookie survived logout: present=%v err=%v", present, err)
	}
}

// A cookie-authenticated logout is a forged-form target like any ambient request.
// A request that carries a bearer is not: a page cannot set that header.
func TestLogoutCSRF(t *testing.T) {
	f := newFixture(t)
	ck := do(f.h.Login, loginReq(tokenBody(f.admin))).Result().Cookies()[0]
	for name, mod := range map[string]func(*http.Request){
		"no Origin":    func(r *http.Request) { r.Header.Del("Origin") },
		"text/plain":   func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		"another port": func(r *http.Request) { r.Header.Set("Origin", "http://panel.lan:1") },
		"same-site":    func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") },
	} {
		r := logoutReq()
		r.AddCookie(ck)
		mod(r)
		if rec := do(f.h.Logout, r); rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, rec.Code)
		}
		if _, _, err := f.h.Verify(withCookie(ck)); err != nil {
			t.Errorf("%s: a refused logout still revoked: %v", name, err)
		}
	}
	r := logoutReq()
	r.Header.Del("Origin")
	r.Header.Del("Content-Type")
	r.Header.Set("Authorization", "Bearer "+f.admin)
	if rec := do(f.h.Logout, r); rec.Code != http.StatusNoContent {
		t.Errorf("a bearer-authenticated logout: %d, want 204", rec.Code)
	}
	if _, _, err := f.h.Verify(withCookie(ck)); err == nil {
		t.Error("the bearer logout did not revoke")
	}
	r = logoutReq()
	r.Method = http.MethodGet
	if rec := do(f.h.Logout, r); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET logout: %d", rec.Code)
	}
}

// Neither a token nor a cookie value (nor its tag) reaches a log line.
func TestNoSecretInAnyLogLine(t *testing.T) {
	f := newFixture(t)
	other, _, _ := apiauth.GenerateToken()
	var secrets []string
	secrets = append(secrets, f.admin, f.reader, other)
	ck := do(f.h.Login, loginReq(tokenBody(f.admin))).Result().Cookies()[0]
	secrets = append(secrets, ck.Value, ck.Value[:strings.IndexByte(ck.Value, '.')], ck.Value[strings.IndexByte(ck.Value, '.')+1:])
	do(f.h.Login, loginReq(tokenBody(f.reader)))
	do(f.h.Login, loginReq(tokenBody(other)))
	do(f.h.Login, loginReq(tokenBody("owner-chosen-secret-value")))
	secrets = append(secrets, "owner-chosen-secret-value")
	bad := loginReq(tokenBody(f.admin))
	bad.Header.Del("Origin")
	do(f.h.Login, bad)
	r := logoutReq()
	r.AddCookie(ck)
	do(f.h.Logout, r)
	r = logoutReq()
	r.AddCookie(&http.Cookie{Name: CookieName, Value: ck.Value})
	r.Header.Del("Origin")
	do(f.h.Logout, r)
	f.h.Verify(withCookie(&http.Cookie{Name: CookieName, Value: ck.Value + "x"}))
	if f.log.Len() == 0 {
		t.Fatal("nothing was logged: the test proves nothing")
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(f.log.String(), s) {
			t.Errorf("a log line holds a secret: %q", s)
		}
	}
	if strings.Contains(f.log.String(), "Bearer") {
		t.Error("a log line holds a header value")
	}
}

// The race detector over login, verify and logout together.
func TestHandlersConcurrent(t *testing.T) {
	f := newFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				rec := do(f.h.Login, loginReq(tokenBody(f.admin)))
				if cs := rec.Result().Cookies(); len(cs) == 1 {
					f.h.Verify(withCookie(cs[0]))
				}
				if j%25 == 0 {
					do(f.h.Logout, logoutReq())
				}
			}
		}()
	}
	wg.Wait()
}
