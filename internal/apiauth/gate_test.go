package apiauth_test

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"flodni/internal/apiauth"
	"flodni/internal/apiauth/authtest"
	"flodni/internal/apiauth/jwt"
	"flodni/internal/flog"
)

// obs records what the gate reports to the refusal metric.
type obs struct {
	mu  sync.Mutex
	got [][3]string
}

func (o *obs) observe(route string, scope apiauth.Scope, reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.got = append(o.got, [3]string{route, string(scope), reason})
}

func (o *obs) last() [3]string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.got) == 0 {
		return [3]string{}
	}
	return o.got[len(o.got)-1]
}

func (o *obs) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.got)
}

// control is the control gate over the real registrar with a stub on every
// pattern of the table. reached records what a handler saw.
type control struct {
	t       *testing.T
	fx      *authtest.Fixture
	h       http.Handler
	obs     *obs
	logs    *bytes.Buffer
	reached []seen
}

type seen struct {
	Path, RawQuery, RequestURI string
	ID                         apiauth.Identity
	HasID                      bool
}

func newControl(t *testing.T, fx *authtest.Fixture, opts ...apiauth.Option) *control {
	t.Helper()
	c := &control{t: t, fx: fx, obs: &obs{}, logs: &bytes.Buffer{}}
	reg := apiauth.NewRegistrar()
	done := map[string]bool{}
	for _, r := range authtest.Golden(t) {
		if r.Server != "control" || done[r.Pattern] {
			continue
		}
		done[r.Pattern] = true
		reg.Handle(r.Pattern, func(w http.ResponseWriter, req *http.Request) {
			id, ok := apiauth.IdentityFrom(req.Context())
			c.reached = append(c.reached, seen{req.URL.Path, req.URL.RawQuery, req.RequestURI, id, ok})
			w.WriteHeader(http.StatusNoContent)
		})
	}
	all := append([]apiauth.Option{
		apiauth.WithTokens(fx.Tokens), apiauth.WithPattern(reg.Pattern), apiauth.WithObserver(c.obs.observe),
	}, opts...)
	c.h = apiauth.Middleware("", flog.NewText(c.logs), reg, all...)
	return c
}

func (c *control) do(method, target, bearer string, hdr ...string) (*httptest.ResponseRecorder, bool) {
	c.t.Helper()
	before := len(c.reached)
	req := httptest.NewRequest(method, target, nil)
	req.Host = "panel.example:9080"
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	return rec, len(c.reached) > before
}

// The matrix: every control row of the golden copy against no credential, a wrong
// token, each scope alone, a two-scope token and a cookie (which a gate with no
// cookie configured ignores). Expected results come from the golden file and the
// rule "the scope or admin", written out in the fixture, not from the table.
func TestControlScopeMatrix(t *testing.T) {
	fx := authtest.New(t)
	creds := []string{"", "wrong", "badshape", "admin", "engine", "library", "metrics", "read", "scrape", "upload", "webhook", "readlib"}
	for _, row := range authtest.Golden(t) {
		if row.Server != "control" {
			continue
		}
		for _, cred := range creds {
			c := newControl(t, fx)
			bearer := ""
			switch cred {
			case "wrong":
				bearer = fx.Unknown()
			case "badshape":
				bearer = "not-a-token"
			case "":
			default:
				bearer = fx.Token[cred]
			}
			rec, reached := c.do(row.Method, authtest.Path(row.Pattern), bearer, "Cookie", "flodni_session=x")
			name := row.Method + " " + row.Pattern + " as " + cred
			switch {
			case row.Scope == "open":
				// An open route needs no credential; with a wrong one it is still served
				// (today's behaviour: open routes are not authenticated at all).
				if !reached {
					t.Errorf("%s: open route refused with %d", name, rec.Code)
				}
			case cred == "" || cred == "wrong" || cred == "badshape":
				if reached || rec.Code != http.StatusUnauthorized {
					t.Errorf("%s: status %d reached=%v, want 401 and not reached", name, rec.Code, reached)
				}
				if rec.Header().Get("WWW-Authenticate") != "Bearer" {
					t.Errorf("%s: no WWW-Authenticate: Bearer", name)
				}
			case fx.Allows(cred, row.Scope):
				if !reached || rec.Code != http.StatusNoContent {
					t.Errorf("%s: status %d reached=%v, want served", name, rec.Code, reached)
				}
			default:
				if reached || rec.Code != http.StatusForbidden {
					t.Errorf("%s: status %d reached=%v, want 403 and not reached", name, rec.Code, reached)
				}
			}
		}
	}
}

// Unknown means admin: a method no row lists for a known path, and a path in no row.
func TestControlUnknownIsAdmin(t *testing.T) {
	fx := authtest.New(t)
	for _, tc := range []struct{ method, target string }{
		{"POST", "/control"}, {"DELETE", "/api/health"}, {"POST", "/api/live"}, {"GET", "/api/restart"},
		{"PUT", "/api/sessions"}, {"HEAD", "/api/sessions"}, {"GET", "/api/health/"}, {"GET", "/nope"},
		{"GET", "/API/SESSIONS"}, {"GET", "/metrics/"},
	} {
		for _, cred := range []string{"read", "metrics", "library", "readlib"} {
			c := newControl(t, fx)
			rec, reached := c.do(tc.method, tc.target, fx.Token[cred])
			if reached || rec.Code != http.StatusForbidden {
				t.Errorf("%s %s as %s: status %d reached=%v, want 403", tc.method, tc.target, cred, rec.Code, reached)
			}
			if got := c.obs.last(); got[2] != "scope" || got[1] != "admin" {
				t.Errorf("%s %s as %s: counted %v, want scope on admin", tc.method, tc.target, cred, got)
			}
		}
		c := newControl(t, fx)
		if rec, _ := c.do(tc.method, tc.target, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no credential: %d, want 401", tc.method, tc.target, rec.Code)
		}
	}
}

// The router names the pattern, so a path it would redirect to a route is classified as
// that route (and the redirect, which reaches no handler, goes only to a caller who
// holds its scope).
func TestControlClassifiesByTheRoutersPattern(t *testing.T) {
	fx := authtest.New(t)
	for _, target := range []string{"//api/sessions", "/api//sessions", "/api/./sessions", "/api/x/../sessions"} {
		c := newControl(t, fx)
		if rec, reached := c.do("GET", target, fx.Token["metrics"]); reached || rec.Code != http.StatusForbidden {
			t.Errorf("%s as metrics: %d reached=%v, want 403", target, rec.Code, reached)
		}
		if got := c.obs.last(); got != [3]string{"/api/sessions", "read", "scope"} {
			t.Errorf("%s counted %v", target, got)
		}
		c = newControl(t, fx)
		if rec, reached := c.do("GET", target, fx.Token["read"]); reached || rec.Code/100 != 3 {
			t.Errorf("%s as read: %d reached=%v, want the router's redirect", target, rec.Code, reached)
		}
	}
}

// Every 401 in scoped mode tells the panel to show its sign-in form.
func TestScopedUnauthorizedCarriesTheLoginHeader(t *testing.T) {
	fx := authtest.New(t)
	c := newControl(t, fx)
	for _, bearer := range []string{"", fx.Unknown()} {
		rec, _ := c.do("GET", "/api/sessions", bearer)
		if rec.Code != 401 || rec.Header().Get("Flodni-Auth") != "login" {
			t.Errorf("status %d Flodni-Auth %q", rec.Code, rec.Header().Get("Flodni-Auth"))
		}
	}
	// Legacy mode: no header, so the panel keeps its bearer prompt.
	rec := httptest.NewRecorder()
	apiauth.Middleware("tok", nil, http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest("GET", "/api/sessions", nil))
	if rec.Code != 401 || rec.Header().Get("Flodni-Auth") != "" {
		t.Errorf("legacy: %d %q", rec.Code, rec.Header().Get("Flodni-Auth"))
	}
}

func TestEmptyBearerIsNoCredential(t *testing.T) {
	fx := authtest.New(t)
	c := newControl(t, fx)
	req := httptest.NewRequest("GET", "/api/sessions", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	if got := c.obs.last(); got[2] != "missing" {
		t.Errorf("an empty bearer is %q, want missing (no credential)", got[2])
	}
}

func TestWrongBearerIsInvalidNotMissing(t *testing.T) {
	fx := authtest.New(t)
	for _, bearer := range []string{fx.Unknown(), "garbage", fx.Token["read"] + "x"} {
		c := newControl(t, fx)
		c.do("GET", "/api/sessions", bearer)
		if got := c.obs.last(); got != [3]string{"/api/sessions", "read", "invalid"} {
			t.Errorf("%q counted %v", bearer, got)
		}
	}
}

// A token in the query: only a webhook-only token, only on the two webhook routes.
func TestQueryTokens(t *testing.T) {
	fx := authtest.New(t)
	for _, route := range []string{"/webhook", "/plex/webhook"} {
		c := newControl(t, fx)
		rec, reached := c.do("POST", route+"?a=1&token="+fx.Token["webhook"]+"&b=2", "")
		if !reached || rec.Code != http.StatusNoContent {
			t.Fatalf("%s: webhook-only token in the query: %d reached=%v", route, rec.Code, reached)
		}
		got := c.reached[0]
		if strings.Contains(got.RawQuery, "token") || strings.Contains(got.RequestURI, "token") ||
			strings.Contains(got.RawQuery, fx.Token["webhook"]) || strings.Contains(got.RequestURI, fx.Token["webhook"]) {
			t.Errorf("%s: the handler saw the token: %+v", route, got)
		}
		if got.RawQuery != "a=1&b=2" || got.RequestURI != route+"?a=1&b=2" {
			t.Errorf("%s: other parameters must survive: %+v", route, got)
		}
		if got.ID.Name != "webhook" {
			t.Errorf("%s: identity %q, want webhook", route, got.ID.Name)
		}
		for _, wide := range []string{"admin", "readlib", "engine", "library", "webhookread"} {
			c := newControl(t, fx)
			rec, reached := c.do("POST", route+"?token="+fx.Token[wide], "")
			if reached || rec.Code != http.StatusUnauthorized {
				t.Errorf("%s: a %s token in the URL: %d reached=%v, want 401", route, wide, rec.Code, reached)
			}
			if got := c.obs.last(); got[2] != "query_not_allowed" {
				t.Errorf("%s: %s token in the URL counted %v, want query_not_allowed", route, wide, got)
			}
		}
		c = newControl(t, fx)
		rec, reached = c.do("POST", route+"?token="+fx.Unknown(), "")
		if reached || rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: unknown token in the query: %d", route, rec.Code)
		}
		if got := c.obs.last(); got[2] != "invalid" {
			t.Errorf("%s: unknown token counted %v, want invalid", route, got)
		}
	}
}

// Off the webhook routes a token parameter is refused without a lookup, even with a
// valid header, and the answer cannot tell a real token from a made-up one.
func TestQueryTokenOffTheWebhooksIsRefusedTheSameWhatever(t *testing.T) {
	fx := authtest.New(t)
	var first *httptest.ResponseRecorder
	for _, q := range []string{fx.Token["admin"], fx.Unknown(), "x", ""} {
		for _, bearer := range []string{"", fx.Token["admin"]} {
			c := newControl(t, fx)
			rec, reached := c.do("GET", "/api/sessions?token="+q, bearer)
			if reached || rec.Code != http.StatusUnauthorized {
				t.Fatalf("token=%q bearer=%v: %d reached=%v, want 401", q, bearer != "", rec.Code, reached)
			}
			if got := c.obs.last(); got != [3]string{"/api/sessions", "read", "query_not_allowed"} {
				t.Errorf("counted %v", got)
			}
			if first == nil {
				first = rec
			} else if rec.Body.String() != first.Body.String() || rec.Header().Get("WWW-Authenticate") != first.Header().Get("WWW-Authenticate") {
				t.Errorf("the refusal differs by value: %q vs %q", rec.Body, first.Body)
			}
		}
	}
}

// credentials: a fake proxy verifier and a fake cookie.
type fakeJWT struct {
	calls int
	err   error
	email string
}

func (f *fakeJWT) Verify(r *http.Request) (jwt.Identity, error) {
	f.calls++
	if r.Header.Get("X-Jwt") == "" {
		return jwt.Identity{}, &jwt.Error{Reason: jwt.ReasonMissing}
	}
	if f.err != nil {
		return jwt.Identity{}, f.err
	}
	return jwt.Identity{Subject: "s", Email: f.email}, nil
}

func jwtErr(reason string) error { return &jwt.Error{Reason: reason} }

func TestCredentialOrder(t *testing.T) {
	fx := authtest.New(t)
	t.Run("a verified proxy assertion is admin on the control port", func(t *testing.T) {
		v := &fakeJWT{}
		c := newControl(t, fx, apiauth.WithJWT(v))
		rec, reached := c.do("GET", "/api/config", "", "X-Jwt", "a.b.c")
		if !reached || rec.Code != 204 {
			t.Fatalf("status %d", rec.Code)
		}
		if c.reached[0].ID.Name != "jwt" || !c.reached[0].ID.Scopes.Has(apiauth.ScopeAdmin) {
			t.Errorf("identity %+v", c.reached[0].ID)
		}
	})
	t.Run("an empty bearer falls to the proxy assertion", func(t *testing.T) {
		c := newControl(t, fx, apiauth.WithJWT(&fakeJWT{}))
		req := httptest.NewRequest("GET", "/api/config", nil)
		req.Header.Set("Authorization", "Bearer ")
		req.Header.Set("X-Jwt", "a.b.c")
		req.Host = "panel.example:9080"
		rec := httptest.NewRecorder()
		c.h.ServeHTTP(rec, req)
		if rec.Code != 204 {
			t.Fatalf("status %d, want 204", rec.Code)
		}
	})
	t.Run("a wrong bearer is refused and never falls through", func(t *testing.T) {
		v := &fakeJWT{}
		c := newControl(t, fx, apiauth.WithJWT(v))
		rec, reached := c.do("GET", "/api/config", fx.Unknown(), "X-Jwt", "a.b.c")
		if reached || rec.Code != 401 || v.calls != 0 {
			t.Fatalf("status %d reached=%v jwt calls=%d, want 401, not reached, verifier not asked", rec.Code, reached, v.calls)
		}
	})
	t.Run("a valid bearer decides, whatever the assertion would grant", func(t *testing.T) {
		v := &fakeJWT{}
		c := newControl(t, fx, apiauth.WithJWT(v))
		rec, reached := c.do("POST", "/api/config", fx.Token["read"], "X-Jwt", "a.b.c")
		if reached || rec.Code != 403 || v.calls != 0 {
			t.Fatalf("status %d reached=%v jwt calls=%d, want 403 for the read token", rec.Code, reached, v.calls)
		}
	})
	t.Run("a failed assertion is refused with its reason", func(t *testing.T) {
		for _, reason := range []string{jwt.ReasonMalformed, jwt.ReasonAlg, jwt.ReasonUnknownKid, jwt.ReasonKeysUnavailable,
			jwt.ReasonSignature, jwt.ReasonIssuer, jwt.ReasonAudience, jwt.ReasonExpired, jwt.ReasonNotYetValid, jwt.ReasonClaims} {
			c := newControl(t, fx, apiauth.WithJWT(&fakeJWT{err: jwtErr(reason)}))
			rec, reached := c.do("GET", "/api/config", "", "X-Jwt", "a.b.c")
			if reached || rec.Code != 401 {
				t.Errorf("%s: status %d reached=%v", reason, rec.Code, reached)
			}
			if got := c.obs.last(); got != [3]string{"/api/config", "admin", reason} {
				t.Errorf("%s: counted %v", reason, got)
			}
		}
	})
	t.Run("no assertion header is missing, not a jwt reason", func(t *testing.T) {
		c := newControl(t, fx, apiauth.WithJWT(&fakeJWT{}))
		rec, _ := c.do("GET", "/api/config", "")
		if rec.Code != 401 {
			t.Fatalf("status %d", rec.Code)
		}
		if got := c.obs.last(); got[2] != "missing" {
			t.Errorf("counted %v, want missing", got)
		}
	})
	t.Run("the assertion is not asked for an open route", func(t *testing.T) {
		v := &fakeJWT{err: jwtErr(jwt.ReasonExpired)}
		c := newControl(t, fx, apiauth.WithJWT(v))
		if rec, reached := c.do("GET", "/api/health", "", "X-Jwt", "a.b.c"); !reached || rec.Code != 204 || v.calls != 0 {
			t.Errorf("status %d reached=%v calls=%d", rec.Code, reached, v.calls)
		}
	})
	t.Run("a cookie credential sits between the bearer and the assertion", func(t *testing.T) {
		ok := func(r *http.Request) (string, bool, error) {
			if r.Header.Get("Cookie") == "" {
				return "", false, nil
			}
			return "operator", true, nil
		}
		v := &fakeJWT{}
		c := newControl(t, fx, apiauth.WithJWT(v), apiauth.WithCookie(ok))
		rec, reached := c.do("GET", "/api/config", "", "Cookie", "flodni_session=x", "X-Jwt", "a.b.c")
		if !reached || rec.Code != 204 || v.calls != 0 || c.reached[0].ID.Name != "operator" {
			t.Fatalf("status %d reached=%v jwt calls=%d id=%+v", rec.Code, reached, v.calls, c.reached)
		}
		// A bearer still wins over the cookie.
		c = newControl(t, fx, apiauth.WithCookie(ok))
		rec, reached = c.do("POST", "/api/config", fx.Token["read"], "Cookie", "flodni_session=x")
		if reached || rec.Code != 403 {
			t.Fatalf("a bearer with a cookie: status %d reached=%v", rec.Code, reached)
		}
		// A bad cookie is refused, never falling through to the assertion.
		bad := func(r *http.Request) (string, bool, error) { return "", true, errors.New("expired") }
		v = &fakeJWT{}
		c = newControl(t, fx, apiauth.WithJWT(v), apiauth.WithCookie(bad))
		rec, reached = c.do("GET", "/api/config", "", "Cookie", "flodni_session=x", "X-Jwt", "a.b.c")
		if reached || rec.Code != 401 || v.calls != 0 {
			t.Fatalf("a bad cookie: status %d reached=%v jwt calls=%d", rec.Code, reached, v.calls)
		}
		if got := c.obs.last(); got[2] != "session" {
			t.Errorf("counted %v, want session", got)
		}
	})
}

func adminSet(t *testing.T) apiauth.ScopeSet {
	t.Helper()
	s, err := apiauth.ParseScopes("admin")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A request authenticated by a cookie or a proxy header is one a browser sends by
// itself: a state-changing one needs an Origin of the same host and port, a
// same-origin Sec-Fetch-Site when present, and a JSON content type.
func TestAmbientCredentialsNeedTheCSRFRules(t *testing.T) {
	fx := authtest.New(t)
	post := func(c *control, host string, hdr ...string) (*httptest.ResponseRecorder, bool) {
		before := len(c.reached)
		req := httptest.NewRequest("POST", "/api/config", strings.NewReader("{}"))
		req.Host = host
		req.Header.Set("X-Jwt", "a.b.c")
		for i := 0; i+1 < len(hdr); i += 2 {
			if hdr[i+1] == "-" {
				req.Header.Del(hdr[i])
			} else {
				req.Header.Set(hdr[i], hdr[i+1])
			}
		}
		rec := httptest.NewRecorder()
		c.h.ServeHTTP(rec, req)
		return rec, len(c.reached) > before
	}
	good := []string{"Origin", "http://panel.example:9080", "Content-Type", "application/json"}
	for _, tc := range []struct {
		name  string
		host  string
		hdr   []string
		opts  []apiauth.Option
		allow bool
	}{
		{"same origin", "panel.example:9080", good, nil, true},
		{"content type parameters", "panel.example:9080", []string{"Origin", "http://panel.example:9080", "Content-Type", "application/json; charset=utf-8"}, nil, true},
		{"content type any case", "panel.example:9080", []string{"Origin", "http://panel.example:9080", "Content-Type", "Application/JSON"}, nil, true},
		{"sec-fetch-site same-origin", "panel.example:9080", append([]string{"Sec-Fetch-Site", "same-origin"}, good...), nil, true},
		{"host and origin case", "Panel.Example:9080", []string{"Origin", "http://panel.EXAMPLE:9080", "Content-Type", "application/json"}, nil, true},
		{"tls tunnel, no port on Host", "panel.example", []string{"Origin", "https://panel.example", "Content-Type", "application/json"}, nil, true},
		{"tls tunnel, plain origin on port 80", "panel.example", []string{"Origin", "http://panel.example", "Content-Type", "application/json"}, nil, true},
		{"https origin, explicit default port", "panel.example", []string{"Origin", "https://panel.example:443", "Content-Type", "application/json"}, nil, true},
		{"https on another port", "panel.example:8443", []string{"Origin", "https://panel.example:8443", "Content-Type", "application/json"}, nil, true},
		{"ipv6 literal", "[::1]:9080", []string{"Origin", "http://[::1]:9080", "Content-Type", "application/json"}, nil, true},
		{"panel https and an https origin", "panel.example", []string{"Origin", "https://panel.example", "Content-Type", "application/json"}, []apiauth.Option{apiauth.WithPanelHTTPS(true)}, true},

		{"no origin", "panel.example:9080", []string{"Content-Type", "application/json"}, nil, false},
		{"another host", "panel.example:9080", []string{"Origin", "http://evil.example:9080", "Content-Type", "application/json"}, nil, false},
		{"another port on the same host", "panel.example:9080", []string{"Origin", "http://panel.example:8080", "Content-Type", "application/json"}, nil, false},
		{"origin has no port, host has one", "panel.example:9080", []string{"Origin", "http://panel.example", "Content-Type", "application/json"}, nil, false},
		{"origin has a port, host has none", "panel.example", []string{"Origin", "http://panel.example:9080", "Content-Type", "application/json"}, nil, false},
		{"origin null", "panel.example:9080", []string{"Origin", "null", "Content-Type", "application/json"}, nil, false},
		{"origin with a path is not an origin", "panel.example:9080", []string{"Origin", "http://panel.example:9080/x", "Content-Type", "application/json"}, nil, false},
		{"origin with userinfo", "panel.example:9080", []string{"Origin", "http://u@panel.example:9080", "Content-Type", "application/json"}, nil, false},
		{"origin not http", "panel.example:9080", []string{"Origin", "chrome-extension://panel.example:9080", "Content-Type", "application/json"}, nil, false},
		{"subdomain", "panel.example:9080", []string{"Origin", "http://a.panel.example:9080", "Content-Type", "application/json"}, nil, false},
		{"text/plain", "panel.example:9080", []string{"Origin", "http://panel.example:9080", "Content-Type", "text/plain"}, nil, false},
		{"form post", "panel.example:9080", []string{"Origin", "http://panel.example:9080", "Content-Type", "application/x-www-form-urlencoded"}, nil, false},
		{"no content type", "panel.example:9080", []string{"Origin", "http://panel.example:9080"}, nil, false},
		{"json lookalike", "panel.example:9080", []string{"Origin", "http://panel.example:9080", "Content-Type", "application/jsonp"}, nil, false},
		{"sec-fetch-site same-site", "panel.example:9080", append([]string{"Sec-Fetch-Site", "same-site"}, good...), nil, false},
		{"sec-fetch-site cross-site", "panel.example:9080", append([]string{"Sec-Fetch-Site", "cross-site"}, good...), nil, false},
		{"sec-fetch-site none", "panel.example:9080", append([]string{"Sec-Fetch-Site", "none"}, good...), nil, false},
		{"panel https and a plain origin", "panel.example", []string{"Origin", "http://panel.example", "Content-Type", "application/json"}, []apiauth.Option{apiauth.WithPanelHTTPS(true)}, false},
		{"forwarded proto https and a plain origin", "panel.example", []string{"Origin", "http://panel.example", "Content-Type", "application/json", "X-Forwarded-Proto", "https"}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newControl(t, fx, append([]apiauth.Option{apiauth.WithJWT(&fakeJWT{})}, tc.opts...)...)
			rec, reached := post(c, tc.host, tc.hdr...)
			if tc.allow && (!reached || rec.Code != 204) {
				t.Fatalf("status %d reached=%v, want served", rec.Code, reached)
			}
			if !tc.allow {
				if reached || rec.Code != 403 {
					t.Fatalf("status %d reached=%v, want 403", rec.Code, reached)
				}
				if got := c.obs.last(); got != [3]string{"/api/config", "admin", "csrf"} {
					t.Errorf("counted %v, want csrf", got)
				}
			}
		})
	}

	t.Run("a bearer request is outside these rules", func(t *testing.T) {
		c := newControl(t, fx, apiauth.WithJWT(&fakeJWT{}))
		req := httptest.NewRequest("POST", "/api/config", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+fx.Token["admin"])
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		c.h.ServeHTTP(rec, req)
		if rec.Code != 204 {
			t.Fatalf("status %d, want 204", rec.Code)
		}
	})
	t.Run("a GET with an ambient credential keeps the cross-site refusal", func(t *testing.T) {
		for site, want := range map[string]int{"cross-site": 403, "same-origin": 204, "same-site": 204, "none": 204, "": 204} {
			c := newControl(t, fx, apiauth.WithJWT(&fakeJWT{}))
			hdr := []string{"X-Jwt", "a.b.c"}
			if site != "" {
				hdr = append(hdr, "Sec-Fetch-Site", site)
			}
			rec, _ := c.do("GET", "/api/config", "", hdr...)
			if rec.Code != want {
				t.Errorf("Sec-Fetch-Site %q: status %d, want %d", site, rec.Code, want)
			}
			if want == 403 {
				if got := c.obs.last(); got[2] != "cross_site" {
					t.Errorf("counted %v, want cross_site", got)
				}
			}
		}
	})
	t.Run("the same rules for a cookie", func(t *testing.T) {
		ck := func(r *http.Request) (string, bool, error) { return "operator", true, nil }
		c := newControl(t, fx, apiauth.WithCookie(ck))
		req := httptest.NewRequest("POST", "/api/config", strings.NewReader("{}"))
		req.Host = "panel.example:9080"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		c.h.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Fatalf("a cookie post with no Origin: status %d, want 403", rec.Code)
		}
	})
}

func TestScopedModeCountsTheHostAndOriginRefusalsAndOpenModeToo(t *testing.T) {
	fx := authtest.New(t)
	c := newControl(t, fx, apiauth.WithAllowedHosts([]string{"panel.example"}))
	req := httptest.NewRequest("GET", "/api/sessions", nil)
	req.Host = "evil.example"
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	if rec.Code != 403 || c.obs.last() != [3]string{"/api/sessions", "read", "host"} {
		t.Errorf("host: status %d counted %v", rec.Code, c.obs.last())
	}

	// Open mode: no credential configured, the old refusals, now counted.
	o := &obs{}
	reg := apiauth.NewRegistrar()
	reg.Handle("/api/sessions", func(w http.ResponseWriter, r *http.Request) {})
	h := apiauth.Middleware("", nil, reg, apiauth.WithPattern(reg.Pattern), apiauth.WithObserver(o.observe))
	for _, tc := range []struct {
		hdr  [2]string
		want string
	}{
		{[2]string{"Origin", "https://evil.example"}, "origin"},
		{[2]string{"Sec-Fetch-Site", "cross-site"}, "cross_site"},
	} {
		req := httptest.NewRequest("POST", "/api/sessions", nil)
		req.Host = "panel.example"
		req.Header.Set(tc.hdr[0], tc.hdr[1])
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 403 || o.last()[2] != tc.want || o.last()[0] != "/api/sessions" {
			t.Errorf("%s: status %d counted %v", tc.want, rec.Code, o.last())
		}
	}
	// An unmatched path is labelled unmatched, never the raw path.
	req = httptest.NewRequest("POST", "/some/secret/path", nil)
	req.Header.Set("Origin", "https://evil.example")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got := o.last(); got[0] != "unmatched" {
		t.Errorf("route label %q, want unmatched", got[0])
	}
}

func goStorm(t *testing.T, fx *authtest.Fixture, opts ...apiauth.Option) (http.Handler, *obs, *[]seen) {
	t.Helper()
	o := &obs{}
	var reached []seen
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := apiauth.IdentityFrom(r.Context())
		reached = append(reached, seen{r.URL.Path, r.URL.RawQuery, r.RequestURI, id, ok})
		w.WriteHeader(http.StatusNoContent)
	})
	all := append([]apiauth.Option{apiauth.WithTokens(fx.Tokens), apiauth.WithObserver(o.observe)}, opts...)
	return apiauth.GoStorm("", nil, next, all...), o, &reached
}

// The engine's outer gate authenticates the bearer and nothing else; the scope check
// is the router's half (internal/gostorm/web).
func TestEngineGateAuthenticatesTheBearerOnly(t *testing.T) {
	fx := authtest.New(t)
	for _, name := range []string{"admin", "engine", "webhook", "readlib"} {
		h, _, reached := goStorm(t, fx)
		req := httptest.NewRequest("POST", "/torrents", nil)
		req.Header.Set("Authorization", "Bearer "+fx.Token[name])
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 204 || len(*reached) != 1 || (*reached)[0].ID.Name != name {
			t.Errorf("%s: status %d reached=%v", name, rec.Code, *reached)
		}
	}
	for name, bearer := range map[string]string{"none": "", "wrong": fx.Unknown(), "bad shape": "x", "empty": " "} {
		h, o, reached := goStorm(t, fx)
		req := httptest.NewRequest("POST", "/torrents", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 401 || len(*reached) != 0 {
			t.Errorf("%s: status %d reached=%v", name, rec.Code, *reached)
		}
		if got := o.last(); got[0] != "pre_route" || got[1] != "" {
			t.Errorf("%s: counted %v, want route pre_route with an empty scope", name, got)
		}
	}
	h, o, reached := goStorm(t, fx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/echo", nil))
	if rec.Code != 204 || len(*reached) != 1 || o.count() != 0 {
		t.Errorf("/echo: status %d", rec.Code)
	}
}

// 8090 never reads the cookie or the proxy header, even when the control gate does.
func TestEngineGateIgnoresTheCookieAndTheProxyHeader(t *testing.T) {
	fx := authtest.New(t)
	ck := func(r *http.Request) (string, bool, error) { return "operator", true, nil }
	v := &fakeJWT{}
	h, o, reached := goStorm(t, fx, apiauth.WithCookie(ck), apiauth.WithJWT(v))
	req := httptest.NewRequest("POST", "/torrents", nil)
	req.Header.Set("Cookie", "flodni_session=x")
	req.Header.Set("X-Jwt", "a.b.c")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 || len(*reached) != 0 || v.calls != 0 {
		t.Fatalf("status %d reached=%v jwt calls=%d", rec.Code, *reached, v.calls)
	}
	if got := o.last(); got[2] != "missing" {
		t.Errorf("counted %v, want missing", got)
	}
}

func TestEngineGateRefusesAnyQueryToken(t *testing.T) {
	fx := authtest.New(t)
	for _, target := range []string{"/torrents?token=", "/torrents?token=" + fx.Token["webhook"], "/webhook?token=" + fx.Token["webhook"], "/stream?link=a&token=x"} {
		for _, bearer := range []string{"", fx.Token["admin"]} {
			h, o, reached := goStorm(t, fx)
			req := httptest.NewRequest("POST", target, nil)
			if bearer != "" {
				req.Header.Set("Authorization", "Bearer "+bearer)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 401 || len(*reached) != 0 {
				t.Errorf("%s bearer=%v: status %d", target, bearer != "", rec.Code)
			}
			if got := o.last(); got != [3]string{"pre_route", "", "query_not_allowed"} {
				t.Errorf("%s: counted %v", target, got)
			}
		}
	}
}

func TestScopedControlRefusesToBeBuiltWithoutARouter(t *testing.T) {
	fx := authtest.New(t)
	defer func() {
		if recover() == nil {
			t.Fatal("a scoped control gate with no router was built: it could not name a pattern, so it would guess")
		}
	}()
	apiauth.Middleware("", nil, http.NotFoundHandler(), apiauth.WithTokens(fx.Tokens))
}

// A success on a mutating route logs the token's name; nothing logs a secret.
func TestLogsTheTokenNameOnAMutatingSuccess(t *testing.T) {
	fx := authtest.New(t)
	c := newControl(t, fx)
	c.do("POST", "/api/library/add", fx.Token["library"])
	c.do("GET", "/api/library/list", fx.Token["read"])
	c.do("POST", "/api/library/remove", fx.Token["read"]) // refused: scope
	c.do("POST", "/api/library/remove?token=SECRETQ", "") // refused: query
	c.do("POST", "/api/config", fx.Token["admin"])        // admin
	c.do("POST", "/webhook", fx.Token["webhook"])         // a webhook is not logged by name
	c.do("POST", "/api/library/add", fx.Token["library"], "Cookie", "flodni_session=SECRETCOOKIE")
	out := c.logs.String()
	if n := strings.Count(out, `[Control] token "library" POST "/api/library/add"`); n != 2 {
		t.Errorf("library add logged by name %d times, want 2: %q", n, out)
	}
	if !strings.Contains(out, `[Control] token "admin" POST "/api/config"`) {
		t.Errorf("config write not logged by name: %q", out)
	}
	for _, bad := range []string{"fl_", "SECRET", "flodni_session", fx.Token["library"], fx.Token["admin"], `"webhook"`, "/api/library/list"} {
		if strings.Contains(out, bad) {
			t.Errorf("a log line holds %q: %q", bad, out)
		}
	}
}

// The refusal log carries method, quoted path, remote address and reason; no query.
func TestRefusalLogsReasonAndNoSecret(t *testing.T) {
	fx := authtest.New(t)
	c := newControl(t, fx)
	c.do("GET", "/api/sessions?token="+fx.Token["admin"]+"&x=1", "")
	c.do("GET", "/api/sessions", fx.Unknown())
	c.do("POST", "/api/library/add", fx.Token["read"])
	out := c.logs.String()
	if n := strings.Count(out, "\n"); n != 3 {
		t.Fatalf("want 3 lines, got %d: %q", n, out)
	}
	for _, want := range []string{"query_not_allowed", "invalid", "scope", `"/api/sessions"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q: %q", want, out)
		}
	}
	for _, bad := range []string{"fl_", "token=", "x=1", fx.Unknown()} {
		if strings.Contains(out, bad) {
			t.Errorf("log holds %q: %q", bad, out)
		}
	}
}

func TestRefusalsKeepLabelsBounded(t *testing.T) {
	var r apiauth.Refusals
	r.Observe("/api/sessions", apiauth.ScopeRead, "scope")
	r.Observe("/api/sessions", apiauth.ScopeRead, "scope")
	r.Observe("pre_route", "", "missing")
	r.Observe("unmatched", apiauth.ScopeAdmin, "invalid")
	for i := 0; i < 1000; i++ { // a path or a secret is never a label
		r.Observe("/hash/"+strings.Repeat("a", i%40), apiauth.Scope("custom-"+strings.Repeat("b", i%40)), "reason-"+strings.Repeat("c", i%40))
	}
	snap := r.Snapshot()
	if len(snap) > 4 {
		t.Fatalf("labels are not bounded: %d series: %v", len(snap), snap)
	}
	for _, s := range snap {
		if s.Route == "/api/sessions" && s.Count != 2 {
			t.Errorf("count %d, want 2", s.Count)
		}
	}
	for i := 1; i < len(snap); i++ {
		a, b := snap[i-1], snap[i]
		if a.Route+"\x00"+a.Scope+"\x00"+a.Reason > b.Route+"\x00"+b.Scope+"\x00"+b.Reason {
			t.Errorf("snapshot not sorted: %v", snap)
		}
	}
}

// CheckAmbient is the one implementation of the rules; the session package calls it.
func TestCheckAmbient(t *testing.T) {
	mk := func(method, host string, hdr ...string) *http.Request {
		r := httptest.NewRequest(method, "/x", nil)
		r.Host = host
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		return r
	}
	if err := apiauth.CheckAmbient(mk("POST", "p.example", "Origin", "https://p.example", "Content-Type", "application/json"), true); err != nil {
		t.Errorf("tunnel post refused: %v", err)
	}
	if apiauth.CheckAmbient(mk("POST", "p.example", "Origin", "http://p.example", "Content-Type", "application/json"), true) == nil {
		t.Error("plain origin accepted for an https panel")
	}
	if apiauth.CheckAmbient(mk("POST", "p.example:9080", "Origin", "http://p.example:1", "Content-Type", "application/json"), false) == nil {
		t.Error("another port accepted")
	}
	if apiauth.CheckAmbient(mk("GET", "p.example", "Sec-Fetch-Site", "cross-site"), false) == nil {
		t.Error("cross-site GET accepted")
	}
	if err := apiauth.CheckAmbient(mk("GET", "p.example"), false); err != nil {
		t.Errorf("plain GET refused: %v", err)
	}
}

// Legacy api_token keeps the v0.2.6 fall-through (a wrong bearer plus the right query
// token on a webhook passes); scoped mode refuses a present-but-wrong credential.
func TestLegacyFallsThroughToTheQueryTokenScopedDoesNot(t *testing.T) {
	const secret = "legacy-secret"
	reached := 0
	h := apiauth.Middleware(secret, nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ }))
	do := func(h http.Handler, target, bearer string) int {
		req := httptest.NewRequest("POST", target, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := do(h, "/webhook?token="+secret, "wrong"); code != 200 || reached != 1 {
		t.Errorf("legacy wrong bearer + right query: %d reached=%d, want the v0.2.6 pass", code, reached)
	}
	if code := do(h, "/webhook?token=bad", "wrong"); code != 401 {
		t.Errorf("legacy wrong bearer + wrong query: %d", code)
	}
	if code := do(h, "/api/sessions?token="+secret, "wrong"); code != 401 {
		t.Errorf("legacy off the webhooks: %d", code)
	}
	fx := authtest.New(t)
	c := newControl(t, fx)
	rec, got := c.do("POST", "/webhook?token="+fx.Token["webhook"], fx.Unknown())
	if got || rec.Code != 401 {
		t.Errorf("scoped wrong bearer + right query: %d reached=%v, want refused", rec.Code, got)
	}
}

// A nil store handed to the gate is a wiring mistake: it fails closed, never open.
func TestWithTokensNilFailsClosed(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	for name, h := range map[string]http.Handler{
		"control": apiauth.Middleware("", nil, next, apiauth.WithTokens(nil)),
		"engine":  apiauth.GoStorm("", nil, next, apiauth.WithTokens(nil)),
	} {
		for _, target := range []string{"/api/sessions", "/echo", "/api/health", "/control"} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
			if called || rec.Code != http.StatusServiceUnavailable {
				t.Errorf("%s %s: %d called=%v, want 503", name, target, rec.Code, called)
			}
		}
	}
}

// Credentials are not evaluated on an open route: junk beside it changes nothing, and
// neither the cookie hook nor the verifier is called.
func TestOpenRoutesIgnoreEveryCredential(t *testing.T) {
	fx := authtest.New(t)
	cookieCalls := 0
	ck := func(*http.Request) (string, bool, error) { cookieCalls++; return "", true, errors.New("stale") }
	v := &fakeJWT{err: jwtErr(jwt.ReasonExpired)}
	for _, target := range []string{"/control", "/api/live", "/api/health"} {
		c := newControl(t, fx, apiauth.WithCookie(ck), apiauth.WithJWT(v))
		rec, reached := c.do("GET", target, "junk", "Cookie", "flodni_session=stale", "X-Jwt", "a.b.c")
		if !reached || rec.Code != 204 {
			t.Errorf("%s: %d reached=%v, want served", target, rec.Code, reached)
		}
	}
	if cookieCalls != 0 || v.calls != 0 {
		t.Errorf("the cookie hook ran %d times and the verifier %d on open routes", cookieCalls, v.calls)
	}
}

// Flodni-Auth names the panel's sign-in, so it rides every control 401 in scoped mode
// (a refused cookie included) and never the engine's.
func TestLoginHeaderOnEveryControl401NeverOnTheEngine(t *testing.T) {
	fx := authtest.New(t)
	bad := func(*http.Request) (string, bool, error) { return "", true, errors.New("expired") }
	for name, opts := range map[string][]apiauth.Option{
		"no credential": nil, "refused cookie": {apiauth.WithCookie(bad)}, "failed assertion": {apiauth.WithJWT(&fakeJWT{err: jwtErr(jwt.ReasonSignature)})},
	} {
		c := newControl(t, fx, opts...)
		rec, _ := c.do("GET", "/api/config", "", "Cookie", "flodni_session=x", "X-Jwt", "a.b.c")
		if rec.Code != 401 || rec.Header().Get("Flodni-Auth") != "login" {
			t.Errorf("%s: %d Flodni-Auth %q", name, rec.Code, rec.Header().Get("Flodni-Auth"))
		}
	}
	h, _, _ := goStorm(t, fx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/torrents", nil))
	if rec.Code != 401 || rec.Header().Get("Flodni-Auth") != "" {
		t.Errorf("engine: %d Flodni-Auth %q, want none", rec.Code, rec.Header().Get("Flodni-Auth"))
	}
}

// Names are logged only for a mutating success, on the control port and the engine's.
func TestNamesAreNotLoggedForReadsOrFailures(t *testing.T) {
	fx := authtest.New(t)
	c := newControl(t, fx)
	c.do("GET", "/api/library/list", fx.Token["read"])
	c.do("GET", "/api/config", fx.Token["admin"])
	c.do("POST", "/api/library/add", fx.Token["read"]) // 403
	c.do("POST", "/api/library/add", fx.Unknown())     // 401
	c.do("POST", "/api/library/add?token=x", "")       // 401
	if out := c.logs.String(); strings.Contains(out, "token \"") {
		t.Errorf("a name was logged for a read or a failure: %q", out)
	}
	var logs bytes.Buffer
	reg := apiauth.NewRegistrar()
	reg.Handle("/api/library/add", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	h := apiauth.Middleware("", flog.NewText(&logs), reg, apiauth.WithTokens(fx.Tokens), apiauth.WithPattern(reg.Pattern))
	req := httptest.NewRequest("POST", "/api/library/add", nil)
	req.Header.Set("Authorization", "Bearer "+fx.Token["library"])
	h.ServeHTTP(httptest.NewRecorder(), req)
	if strings.Contains(logs.String(), "token \"") {
		t.Errorf("a failed write logged a name: %q", logs.String())
	}
}

// The gate under concurrent requests (go test -race): shared tokens, observer and logger.
func TestGateUnderConcurrentRequests(t *testing.T) {
	fx := authtest.New(t)
	var o obs
	var logs bytes.Buffer
	var mu sync.Mutex
	reg := apiauth.NewRegistrar()
	reg.Handle("/api/library/add", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	reg.Handle("/api/sessions", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	ctl := apiauth.Middleware("", flog.NewText(&syncWriter{&mu, &logs}), reg, apiauth.WithTokens(fx.Tokens), apiauth.WithPattern(reg.Pattern),
		apiauth.WithObserver(o.observe), apiauth.WithJWT(&fakeJWT{}))
	eng := apiauth.GoStorm("", nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }),
		apiauth.WithTokens(fx.Tokens), apiauth.WithObserver(o.observe))
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				for _, tc := range []struct {
					h      http.Handler
					method string
					target string
					bearer string
					want   int
				}{
					{ctl, "POST", "/api/library/add", fx.Token["library"], 204},
					{ctl, "POST", "/api/library/add", fx.Token["read"], 403},
					{ctl, "GET", "/api/sessions", fx.Unknown(), 401},
					{ctl, "GET", "/api/sessions?token=x", "", 401},
					{eng, "POST", "/torrents", fx.Token["engine"], 204},
					{eng, "POST", "/torrents", "", 401},
				} {
					req := httptest.NewRequest(tc.method, tc.target, nil)
					if tc.bearer != "" {
						req.Header.Set("Authorization", "Bearer "+tc.bearer)
					}
					rec := httptest.NewRecorder()
					tc.h.ServeHTTP(rec, req)
					if rec.Code != tc.want {
						t.Errorf("%s %s: %d, want %d", tc.method, tc.target, rec.Code, tc.want)
						return
					}
				}
			}
		}(i)
	}
	wg.Wait()
}

type syncWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(b)
}

// The CSRF rules the first pass left unpinned.
func TestCheckAmbientDetails(t *testing.T) {
	mk := func(hdr ...[2]string) *http.Request {
		r := httptest.NewRequest("POST", "/x", nil)
		r.Host = "p.example"
		r.Header.Set("Content-Type", "application/json")
		for _, h := range hdr {
			r.Header.Add(h[0], h[1])
		}
		return r
	}
	ok := [2]string{"Origin", "https://p.example"}
	if err := apiauth.CheckAmbient(mk(ok), false); err != nil {
		t.Fatalf("baseline refused: %v", err)
	}
	for name, r := range map[string]*http.Request{
		"repeated Origin":                 mk(ok, ok),
		"repeated Sec-Fetch-Site":         mk(ok, [2]string{"Sec-Fetch-Site", "same-origin"}, [2]string{"Sec-Fetch-Site", "same-origin"}),
		"forwarded proto list, https":     mk([2]string{"Origin", "http://p.example"}, [2]string{"X-Forwarded-Proto", "https, http"}),
		"origin trailing dot, other host": mk([2]string{"Origin", "https://q.example."}),
	} {
		if apiauth.CheckAmbient(r, false) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A request that arrived over TLS needs an https Origin.
	r := mk([2]string{"Origin", "http://p.example"})
	r.TLS = &tls.ConnectionState{}
	if apiauth.CheckAmbient(r, false) == nil {
		t.Error("an http Origin on a TLS arrival was accepted")
	}
	r = mk(ok)
	r.TLS = &tls.ConnectionState{}
	if err := apiauth.CheckAmbient(r, false); err != nil {
		t.Errorf("an https Origin on a TLS arrival refused: %v", err)
	}
	// A trailing dot on the Origin is the same host as without.
	if err := apiauth.CheckAmbient(mk([2]string{"Origin", "https://p.example."}), false); err != nil {
		t.Errorf("origin with a trailing dot refused: %v", err)
	}
	r = mk([2]string{"Origin", "http://p.example"}, [2]string{"X-Forwarded-Proto", "http, https"})
	if err := apiauth.CheckAmbient(r, false); err != nil {
		t.Errorf("the first X-Forwarded-Proto element is http: %v", err)
	}
}

// The one CSRF table (the session package has none of its own): what CheckAmbient
// accepts for a POST with an ambient credential, and the methods that need the same.
func TestCheckAmbientTable(t *testing.T) {
	type c struct {
		name  string
		host  string
		hdr   map[string]string // an empty value sets an empty header; "-" removes one
		https bool              // panelHTTPS
		tls   bool
		ok    bool
	}
	const j = "application/json"
	o := func(origin string, kv ...string) map[string]string {
		m := map[string]string{"Origin": origin}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	for _, tc := range []c{
		{name: "same host and port", host: "panel.lan:9080", hdr: o("http://panel.lan:9080"), ok: true},
		{name: "host case differs", host: "Panel.LAN:9080", hdr: o("http://panel.lan:9080"), ok: true},
		{name: "another port of the same host", host: "panel.lan:9080", hdr: o("http://panel.lan:8080")},
		{name: "another host", host: "panel.lan:9080", hdr: o("http://evil.example:9080")},
		{name: "no Origin", host: "panel.lan:9080", hdr: map[string]string{}},
		{name: "null Origin", host: "panel.lan:9080", hdr: o("null")},
		{name: "unparsable Origin", host: "panel.lan:9080", hdr: o("://")},
		{name: "Origin with a path", host: "panel.lan:9080", hdr: o("http://panel.lan:9080/x")},
		{name: "Origin with a bare query mark", host: "panel.lan:9080", hdr: o("http://panel.lan:9080?")},
		{name: "Origin with a bare fragment mark", host: "panel.lan:9080", hdr: o("http://panel.lan:9080#")},
		{name: "Origin with userinfo", host: "panel.lan:9080", hdr: o("http://u@panel.lan:9080")},
		{name: "Origin with a scheme that is not http", host: "panel.lan:9080", hdr: o("ftp://panel.lan:9080")},
		{name: "text/plain", host: "panel.lan:9080", hdr: o("http://panel.lan:9080", "Content-Type", "text/plain")},
		{name: "form post", host: "panel.lan:9080", hdr: o("http://panel.lan:9080", "Content-Type", "application/x-www-form-urlencoded")},
		{name: "no content type", host: "panel.lan:9080", hdr: o("http://panel.lan:9080", "Content-Type", "-")},
		{name: "json with a charset", host: "panel.lan:9080", hdr: o("http://panel.lan:9080", "Content-Type", "application/json; charset=utf-8"), ok: true},
		{name: "json look-alike", host: "panel.lan:9080", hdr: o("http://panel.lan:9080", "Content-Type", "application/jsonp")},
		{name: "json suffix type", host: "panel.lan:9080", hdr: o("http://panel.lan:9080", "Content-Type", "application/vnd.x+json")},
		{name: "same-origin fetch metadata", host: "panel.lan:9080", hdr: o("http://panel.lan:9080", "Sec-Fetch-Site", "same-origin"), ok: true},
		{name: "same-site fetch metadata", host: "panel.lan:9080", hdr: o("http://panel.lan:9080", "Sec-Fetch-Site", "same-site")},
		{name: "cross-site fetch metadata", host: "panel.lan:9080", hdr: o("http://panel.lan:9080", "Sec-Fetch-Site", "cross-site")},
		{name: "none fetch metadata", host: "panel.lan:9080", hdr: o("http://panel.lan:9080", "Sec-Fetch-Site", "none")},
		{name: "unknown fetch metadata value", host: "panel.lan:9080", hdr: o("http://panel.lan:9080", "Sec-Fetch-Site", "bogus")},
		{name: "empty fetch metadata value", host: "panel.lan:9080", hdr: o("http://panel.lan:9080", "Sec-Fetch-Site", "")},
		// The TLS tunnel: Flodni sees plain http with the original Host.
		{name: "tunnel, no port on either side", host: "panel.example", hdr: o("https://panel.example"), ok: true},
		{name: "tunnel, explicit matching port", host: "panel.example:8443", hdr: o("https://panel.example:8443"), ok: true},
		{name: "tunnel, Host without a port, Origin on 8443", host: "panel.example", hdr: o("https://panel.example:8443")},
		{name: "tunnel, Host on 8443, Origin without a port", host: "panel.example:8443", hdr: o("https://panel.example")},
		{name: "default port spelt out on the Host", host: "panel.example:443", hdr: o("https://panel.example"), ok: true},
		{name: "http default port spelt out", host: "panel.example:80", hdr: o("http://panel.example"), ok: true},
		{name: "http host on 80, https origin", host: "panel.example:80", hdr: o("https://panel.example")},
		// The scheme is not compared, so the other scheme on default ports is only
		// separated by fetch metadata.
		{name: "http origin, no ports, no fetch metadata", host: "panel.example", hdr: o("http://panel.example"), ok: true},
		{name: "http origin, no ports, same-site", host: "panel.example", hdr: o("http://panel.example", "Sec-Fetch-Site", "same-site")},
		// Known https: the Origin scheme must be https too.
		{name: "panel known https by config, http origin", host: "panel.example", hdr: o("http://panel.example"), https: true},
		{name: "panel known https by config, https origin", host: "panel.example", hdr: o("https://panel.example"), https: true, ok: true},
		{name: "known https by header, http origin", host: "panel.example", hdr: o("http://panel.example", "X-Forwarded-Proto", "https")},
		{name: "known https by header, https origin", host: "panel.example", hdr: o("https://panel.example", "X-Forwarded-Proto", "https"), ok: true},
		{name: "known https by TLS, http origin", host: "panel.example", hdr: o("http://panel.example"), tls: true},
		{name: "known https by TLS, https origin", host: "panel.example", hdr: o("https://panel.example"), tls: true, ok: true},
		{name: "trailing dot on the Host only", host: "panel.example.", hdr: o("https://panel.example"), ok: true},
		{name: "trailing dot on the Origin only", host: "panel.example", hdr: o("https://panel.example."), ok: true},
		{name: "ipv6 host", host: "[::1]:9080", hdr: o("http://[::1]:9080"), ok: true},
		{name: "ipv6 other port", host: "[::1]:9080", hdr: o("http://[::1]:9081")},
		{name: "empty host", host: "", hdr: o("http://panel.lan")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/control/login", nil)
			r.Host = tc.host
			r.Header.Set("Content-Type", j)
			for k, v := range tc.hdr {
				if v == "-" {
					r.Header.Del(k)
				} else {
					r.Header.Set(k, v)
				}
			}
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			err := apiauth.CheckAmbient(r, tc.https)
			if tc.ok && err != nil {
				t.Errorf("refused: %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("accepted, want refused")
			}
		})
	}
}

// Every method but GET needs the full rule, not only POST: HEAD, PUT, DELETE and
// OPTIONS with no Origin are refused, and a repeated header is refused whichever
// value comes first.
func TestCheckAmbientEveryNonGetMethodNeedsTheFullRule(t *testing.T) {
	for _, m := range []string{"HEAD", "PUT", "DELETE", "PATCH", "OPTIONS", "POST"} {
		r := httptest.NewRequest(m, "/x", nil)
		r.Host = "p.example"
		r.Header.Set("Content-Type", "application/json")
		if apiauth.CheckAmbient(r, false) == nil {
			t.Errorf("%s with no Origin accepted", m)
		}
		r.Header.Set("Origin", "http://p.example")
		if err := apiauth.CheckAmbient(r, false); err != nil {
			t.Errorf("%s with the full rule refused: %v", m, err)
		}
		r.Header.Set("Content-Type", "text/plain")
		if apiauth.CheckAmbient(r, false) == nil {
			t.Errorf("%s with text/plain accepted", m)
		}
	}
	mk := func() *http.Request {
		r := httptest.NewRequest("POST", "/x", nil)
		r.Host = "p.example"
		r.Header.Set("Origin", "http://p.example")
		r.Header.Set("Content-Type", "application/json")
		return r
	}
	r := mk()
	r.Header.Add("Content-Type", "text/plain")
	if apiauth.CheckAmbient(r, false) == nil {
		t.Error("a second, text/plain Content-Type accepted behind a JSON one")
	}
	r = mk()
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Add("Sec-Fetch-Site", "cross-site")
	if apiauth.CheckAmbient(r, false) == nil {
		t.Error("a repeated Sec-Fetch-Site accepted")
	}
	// GET reads the header the same way: every value counts.
	g := httptest.NewRequest("GET", "/x", nil)
	g.Header.Add("Sec-Fetch-Site", "same-origin")
	g.Header.Add("Sec-Fetch-Site", "cross-site")
	if apiauth.CheckAmbient(g, false) == nil {
		t.Error("a GET with a repeated Sec-Fetch-Site, one of them cross-site, accepted")
	}
	for site, ok := range map[string]bool{"": true, "same-origin": true, "none": true, "same-site": true, "cross-site": false} {
		g := httptest.NewRequest("GET", "/api/sessions", nil)
		if site != "" {
			g.Header.Set("Sec-Fetch-Site", site)
		}
		if got := apiauth.CheckAmbient(g, false) == nil; got != ok {
			t.Errorf("GET with Sec-Fetch-Site %q: accepted=%v, want %v", site, got, ok)
		}
	}
}
