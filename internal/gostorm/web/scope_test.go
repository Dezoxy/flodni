package web

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	stdlog "log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"flodni/internal/apiauth"
	"flodni/internal/apiauth/authtest"
	"flodni/internal/gostorm/web/api"

	"github.com/gin-gonic/gin"
)

// scoped puts the engine's package in scoped mode for one test and returns the
// fixture, the full handler (outer gate and router, as newServer builds it) and what
// the router's scope middleware let through.
type engineRun struct {
	fx      *authtest.Fixture
	h       http.Handler
	mu      sync.Mutex
	reached []reach
	counted [][3]string
}

type reach struct {
	Method, Path, FullPath string
	Body                   string
	Enforced               bool
	ID                     apiauth.Identity
	Peeked                 string
	HasPeeked              bool
}

func newEngineRun(t *testing.T) *engineRun {
	t.Helper()
	e := &engineRun{fx: authtest.New(t)}
	prevAuth, prevObs, prevPass, prevTok := Auth, AuthObserver, onScopePass, APIToken
	t.Cleanup(func() { Auth, AuthObserver, onScopePass, APIToken = prevAuth, prevObs, prevPass, prevTok })
	Auth, APIToken = e.fx.Tokens, ""
	AuthObserver = func(route string, scope apiauth.Scope, reason string) {
		e.mu.Lock()
		e.counted = append(e.counted, [3]string{route, string(scope), reason})
		e.mu.Unlock()
	}
	// The probe replaces the handlers: a refused request is shown never to get here.
	onScopePass = func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		id, _ := apiauth.IdentityFrom(c.Request.Context())
		e.mu.Lock()
		peeked, hasPeeked := c.Get(api.PeekedActionKey)
		ps, _ := peeked.(string)
		e.reached = append(e.reached, reach{c.Request.Method, c.Request.URL.Path, c.FullPath(), string(body), c.GetBool(api.TokenEnforcedKey), id, ps, hasPeeked})
		e.mu.Unlock()
		c.AbortWithStatus(http.StatusNoContent)
	}
	e.h = newServer("127.0.0.1:0", newRoute()).Handler
	return e
}

func (e *engineRun) do(method, target, bearer, body string, hdr ...string) (*httptest.ResponseRecorder, bool) {
	e.mu.Lock()
	before := len(e.reached)
	e.mu.Unlock()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	req.Host = "engine.example:8090"
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	e.mu.Lock()
	defer e.mu.Unlock()
	return rec, len(e.reached) > before
}

func (e *engineRun) lastCount() [3]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.counted) == 0 {
		return [3]string{}
	}
	return e.counted[len(e.counted)-1]
}

// actionsOf are the bodies the matrix sends to a route that has an action: every
// action the golden copy names, one it does not (the fallback row), and none.
func actionsOf(rows []authtest.Row, pattern string) []string {
	acts := []string{"zzz", ""}
	for _, r := range rows {
		if r.Server == "engine" && r.Pattern == pattern && r.Action != "" && r.Action != "*" {
			acts = append(acts, r.Action)
		}
	}
	return acts
}

func hasActionRows(rows []authtest.Row, pattern string) bool {
	for _, r := range rows {
		if r.Server == "engine" && r.Pattern == pattern && r.Action != "" {
			return true
		}
	}
	return false
}

func goldenScope(rows []authtest.Row, method, pattern, action string) string {
	fallback := ""
	for _, r := range rows {
		if r.Server != "engine" || r.Method != method || r.Pattern != pattern {
			continue
		}
		switch r.Action {
		case "":
			return r.Scope
		case action:
			return r.Scope
		case "*":
			fallback = r.Scope
		}
	}
	if fallback != "" {
		return fallback
	}
	return "admin"
}

// The matrix on the real router: every engine row of the golden copy, with every body
// action, against no credential, a wrong token, each scope alone, a two-scope token
// and a cookie. Expected results come from the golden file and the rule written out
// in the fixture. A route registered before the scope middleware is never guarded
// and fails here (an authorised request would not reach the probe, or an
// unauthorised one would reach a handler).
func TestEngineScopeMatrix(t *testing.T) {
	e := newEngineRun(t)
	rows := authtest.Golden(t)
	creds := []string{"", "wrong", "admin", "engine", "library", "metrics", "read", "scrape", "upload", "webhook", "readlib"}
	type reqSpec struct {
		method, pattern, action string
	}
	var specs []reqSpec
	seen := map[reqSpec]bool{}
	for _, row := range rows {
		if row.Server != "engine" {
			continue
		}
		if row.Action == "" {
			specs = append(specs, reqSpec{row.Method, row.Pattern, ""})
			continue
		}
		for _, a := range actionsOf(rows, row.Pattern) {
			s := reqSpec{row.Method, row.Pattern, a}
			if !seen[s] {
				seen[s] = true
				specs = append(specs, s)
			}
		}
	}
	for _, sp := range specs {
		want := goldenScope(rows, sp.method, sp.pattern, sp.action)
		for _, cred := range creds {
			bearer := ""
			switch cred {
			case "wrong":
				bearer = e.fx.Unknown()
			case "":
			default:
				bearer = e.fx.Token[cred]
			}
			body := ""
			if sp.action != "" {
				body = `{"action":"` + sp.action + `"}`
			} else if sp.method == "POST" && (sp.pattern == "/settings" || sp.pattern == "/torrents") {
				body = `{}` // an action-less request: a missing action is admin
			}
			name := sp.method + " " + sp.pattern + " action=" + sp.action + " as " + cred
			rec, reached := e.do(sp.method, authtest.Path(sp.pattern), bearer, body, "Cookie", "flodni_session=x", "X-Jwt", "a.b.c")
			switch {
			case want == "open":
				if !reached {
					t.Errorf("%s: open route refused with %d", name, rec.Code)
				}
			case cred == "" || cred == "wrong":
				if reached || rec.Code != http.StatusUnauthorized {
					t.Errorf("%s: status %d reached=%v, want 401 and not reached", name, rec.Code, reached)
				}
			case e.fx.Allows(cred, want):
				if !reached || rec.Code != http.StatusNoContent {
					t.Errorf("%s: status %d reached=%v, want reached", name, rec.Code, reached)
				}
			default:
				if reached || rec.Code != http.StatusForbidden {
					t.Errorf("%s: status %d reached=%v, want 403 and not reached", name, rec.Code, reached)
				}
				// The label is the scope the route requires. A caller holding none of the scopes
				// a row of the route names is refused before the body is read, so the action is
				// not known and the route counts as admin.
				label := want
				if hasActionRows(rows, sp.pattern) && !e.fx.Allows(cred, "engine") {
					label = "admin"
				}
				if got := e.lastCount(); got != [3]string{sp.pattern, label, "scope"} {
					t.Errorf("%s: counted %v", name, got)
				}
			}
		}
	}
}

// The engine port never takes the cookie or the proxy's header, and never a query token.
func TestEngineIgnoresCookieAndProxyHeader(t *testing.T) {
	e := newEngineRun(t)
	rec, reached := e.do("POST", "/torrents", "", `{"action":"list"}`, "Cookie", "flodni_session=x", "X-Jwt", "a.b.c", "Cf-Access-Jwt-Assertion", "a.b.c")
	if reached || rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d reached=%v", rec.Code, reached)
	}
	if got := e.lastCount(); got != [3]string{"pre_route", "", "missing"} {
		t.Errorf("counted %v, want the outer gate's pre_route", got)
	}
}

func TestEngineUnknownRoutesAreAdmin(t *testing.T) {
	e := newEngineRun(t)
	for _, tc := range []struct{ method, target string }{
		{"GET", "/nope"}, {"POST", "/torrent"}, {"GET", "/torrents"}, {"PUT", "/torrents"}, {"DELETE", "/settings"},
		{"GET", "/shutdown"}, {"POST", "/torrents/"}, {"POST", "//torrents"}, {"POST", "/Torrents"},
		{"POST", "/echo"}, {"GET", "/echo/x"}, {"OPTIONS", "/torrents"}, {"POST", "/settings/"}, {"POST", "/cache/"},
	} {
		for _, cred := range []string{"engine", "readlib", "upload"} {
			rec, reached := e.do(tc.method, tc.target, e.fx.Token[cred], `{"action":"list"}`)
			if reached || rec.Code != http.StatusForbidden {
				t.Errorf("%s %s as %s: status %d reached=%v, want 403", tc.method, tc.target, cred, rec.Code, reached)
			}
		}
	}
}

// With every redirect and method-not-allowed answer off, and the 404 chain guarded, no
// response is produced before the middleware runs.
func TestRouterFlagsArePinned(t *testing.T) {
	for _, mode := range []string{"open", "scoped"} {
		prev := Auth
		if mode == "scoped" {
			Auth = authtest.New(t).Tokens
		} else {
			Auth = nil
		}
		r := newRoute()
		Auth = prev
		if r.RedirectTrailingSlash || r.RedirectFixedPath || r.HandleMethodNotAllowed || r.UseRawPath || r.UseEscapedPath || r.RemoveExtraSlash {
			t.Errorf("%s: router flags are not all off: %+v", mode, struct{ A, B, C, D, E bool }{r.RedirectTrailingSlash, r.RedirectFixedPath, r.HandleMethodNotAllowed, r.UseRawPath, r.RemoveExtraSlash})
		}
	}
	// POST /torrents/ answers 404 where gin's default answered a 307 redirect.
	for _, mode := range []string{"open", "scoped"} {
		prev := Auth
		var id apiauth.Identity
		if mode == "scoped" {
			Auth = authtest.New(t).Tokens
			id = apiauth.InternalIdentity()
		} else {
			Auth = nil
		}
		r := newRoute()
		Auth = prev
		req := httptest.NewRequest("POST", "/torrents/", strings.NewReader(`{"action":"list"}`))
		if mode == "scoped" {
			req = req.WithContext(apiauth.WithIdentity(req.Context(), id))
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: POST /torrents/ = %d, want 404", mode, rec.Code)
		}
	}
}

// A request that reaches the router with no identity on a route that is not open is
// refused, never read as "open mode": the mode is fixed when the router is built.
func TestRouterWithNoIdentityRefusesInScopedMode(t *testing.T) {
	e := newEngineRun(t)
	route := newRoute()
	for _, target := range []string{"/torrents", "/settings", "/stream", "/play/a/1", "/nope"} {
		rec := httptest.NewRecorder()
		route.ServeHTTP(rec, httptest.NewRequest("POST", target, strings.NewReader(`{"action":"list"}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", target, rec.Code)
		}
	}
	if len(e.reached) != 0 {
		t.Errorf("a handler was reached: %v", e.reached)
	}
	// GET /echo is open: it reaches the probe with no identity.
	before := len(e.reached)
	route.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/echo", nil))
	if len(e.reached) != before+1 {
		t.Error("/echo did not pass")
	}
}

// TokenEnforcedKey is set per request, and only for a caller holding admin.
func TestTokenEnforcedKeyIsPerRequestAndAdminOnly(t *testing.T) {
	e := newEngineRun(t)
	for cred, want := range map[string]bool{"admin": true, "engine": false} {
		if rec, reached := e.do("GET", "/stream?link=a", e.fx.Token[cred], ""); !reached {
			t.Fatalf("%s: %d", cred, rec.Code)
		}
		if got := e.reached[len(e.reached)-1].Enforced; got != want {
			t.Errorf("%s: TokenEnforcedKey = %v, want %v", cred, got, want)
		}
	}
}

// The legacy token (api_token alone) still opens everything and sets the key.
func TestLegacyTokenIsEverythingOnTheEngine(t *testing.T) {
	prevAuth, prevTok, prevPass := Auth, APIToken, onScopePass
	t.Cleanup(func() { Auth, APIToken, onScopePass = prevAuth, prevTok, prevPass })
	Auth, APIToken = nil, "legacy-secret"
	var enforced []bool
	onScopePass = func(c *gin.Context) {
		enforced = append(enforced, c.GetBool(api.TokenEnforcedKey))
		c.AbortWithStatus(204)
	}
	h := newServer("127.0.0.1:0", newRoute()).Handler
	for _, target := range []string{"/torrents", "/shutdown", "/settings", "/nope", "/stream?link=a"} {
		req := httptest.NewRequest("POST", target, strings.NewReader(`{"action":"rem"}`))
		req.Header.Set("Authorization", "Bearer legacy-secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 204 {
			t.Errorf("%s: %d", target, rec.Code)
		}
	}
	for _, b := range enforced {
		if !b {
			t.Error("the legacy token did not set TokenEnforcedKey")
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/torrents", strings.NewReader(`{}`)))
	if rec.Code != 401 {
		t.Errorf("no token: %d", rec.Code)
	}
}

func TestOpenModeSetsNothingAndPeeksNothing(t *testing.T) {
	prevAuth, prevTok, prevPass := Auth, APIToken, onScopePass
	t.Cleanup(func() { Auth, APIToken, onScopePass = prevAuth, prevTok, prevPass })
	Auth, APIToken = nil, ""
	var key []bool
	onScopePass = func(c *gin.Context) { key = append(key, c.GetBool(api.TokenEnforcedKey)); c.AbortWithStatus(204) }
	r := newRoute()
	req := httptest.NewRequest("POST", "/nope", &failingBody{t: t})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", rec.Code)
	}
	if len(key) != 0 {
		t.Errorf("open mode ran the scope middleware: %v", key)
	}
}

type failingBody struct{ t *testing.T }

func (f *failingBody) Read([]byte) (int, error) { f.t.Error("the body was read"); return 0, io.EOF }
func (f *failingBody) Close() error             { return nil }

// ---- the body action ----

func TestBodyActionPeek(t *testing.T) {
	e := newEngineRun(t)
	big := `{"action":"list","pad":"` + strings.Repeat("x", 64<<10) + `"}`
	for _, tc := range []struct {
		name, body string
		cred       string
		want       int
		wantReason string
	}{
		{"list as engine", `{"action":"list"}`, "engine", 204, ""},
		{"add as engine", `{"action":"add","link":"magnet:?xt=urn:btih:x"}`, "engine", 403, "scope"},
		{"rem as engine", `{"action":"rem","hash":"x"}`, "engine", 403, "scope"},
		{"upper-case key wins, last", `{"action":"list","ACTION":"rem"}`, "engine", 403, "scope"},
		{"mixed-case key", `{"Action":"rem"}`, "engine", 403, "scope"},
		{"duplicate, last wins: rem", `{"action":"list","action":"rem"}`, "engine", 403, "scope"},
		{"duplicate, last wins: list", `{"action":"rem","action":"list"}`, "engine", 204, ""},
		{"escaped key", `{"action":"rem"}`, "engine", 403, "scope"},
		{"escaped value", `{"action":"rem"}`, "engine", 403, "scope"},
		{"trailing data is ignored", `{"action":"list"} {"action":"rem"}`, "engine", 204, ""},
		{"trailing garbage is ignored", `{"action":"list"} xx`, "engine", 204, ""},
		{"unknown action", `{"action":"zzz"}`, "engine", 403, "scope"},
		{"no action", `{"hash":"x"}`, "engine", 403, "scope"},
		{"null action", `{"action":null}`, "engine", 403, "scope"},
		{"unknown action as admin", `{"action":"zzz"}`, "admin", 204, ""},
		{"not json", `action=list`, "engine", 400, "body"},
		{"truncated", `{"action":"list"`, "engine", 400, "body"},
		{"empty body", ``, "engine", 400, "body"},
		{"action of the wrong type", `{"action":5}`, "engine", 400, "body"},
		{"an array", `[{"action":"list"}]`, "engine", 400, "body"},
		{"over the cap", big, "engine", 413, "body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, reached := e.do("POST", "/torrents", e.fx.Token[tc.cred], tc.body)
			if rec.Code != tc.want || reached != (tc.want == 204) {
				t.Fatalf("status %d reached=%v, want %d", rec.Code, reached, tc.want)
			}
			if tc.wantReason != "" {
				if got := e.lastCount(); got[2] != tc.wantReason || got[0] != "/torrents" {
					t.Errorf("counted %v, want %s on /torrents", got, tc.wantReason)
				}
			}
		})
	}
}

// The handler receives the bytes that were decoded, exactly.
func TestBodyActionReplaysTheBytes(t *testing.T) {
	e := newEngineRun(t)
	for _, body := range []string{`{"action":"list"}`, `{"action":"get","hash":"abc","unknown":[1,2,{"a":null}]}` + "\n", `{"action":"list"}  trailing`} {
		if rec, reached := e.do("POST", "/torrents", e.fx.Token["engine"], body); !reached {
			t.Fatalf("%q: %d", body, rec.Code)
		}
		if got := e.reached[len(e.reached)-1].Body; got != body {
			t.Errorf("handler got %q, want %q", got, body)
		}
	}
}

func TestBodyActionRefusesAnEncodedBody(t *testing.T) {
	e := newEngineRun(t)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(`{"action":"list"}`))
	_ = zw.Close()
	for _, enc := range []string{"gzip", "deflate", "br", "GZIP", "identity, gzip", "gzip, identity"} {
		rec, reached := e.do("POST", "/torrents", e.fx.Token["engine"], gz.String(), "Content-Encoding", enc)
		if reached || rec.Code != http.StatusBadRequest {
			t.Errorf("Content-Encoding %q: %d reached=%v, want 400", enc, rec.Code, reached)
		}
		if got := e.lastCount(); got[2] != "body" {
			t.Errorf("Content-Encoding %q counted %v", enc, got)
		}
	}
	for _, enc := range []string{"", "identity", "Identity"} {
		hdr := []string{}
		if enc != "" {
			hdr = []string{"Content-Encoding", enc}
		}
		if rec, reached := e.do("POST", "/torrents", e.fx.Token["engine"], `{"action":"list"}`, hdr...); !reached {
			t.Errorf("Content-Encoding %q refused: %d", enc, rec.Code)
		}
	}
}

// The cap is read, not trusted: a chunked body has no Content-Length.
func TestBodyActionCapsAChunkedBody(t *testing.T) {
	e := newEngineRun(t)
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte(`{"action":"list","pad":"`))
		chunk := bytes.Repeat([]byte("x"), 8<<10)
		for i := 0; i < 16; i++ { // 128 KiB, well over 64 KiB
			if _, err := pw.Write(chunk); err != nil {
				return
			}
		}
		_ = pw.Close()
	}()
	req := httptest.NewRequest("POST", "/torrents", pr)
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	req.Header.Set("Authorization", "Bearer "+e.fx.Token["engine"])
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	_ = pr.Close()
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", rec.Code)
	}
}

// Nothing reads a body before the caller has authenticated.
func TestNoBodyIsReadBeforeAuthentication(t *testing.T) {
	e := newEngineRun(t)
	for _, bearer := range []string{"", e.fx.Unknown()} {
		req := httptest.NewRequest("POST", "/torrents", &failingBody{t: t})
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status %d", rec.Code)
		}
	}
	// A token that holds nothing any row of /torrents could need is refused unread.
	for _, cred := range []string{"webhook", "readlib", "metrics", "upload"} {
		req := httptest.NewRequest("POST", "/torrents", &failingBody{t: t})
		req.Header.Set("Authorization", "Bearer "+e.fx.Token[cred])
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", cred, rec.Code)
		}
	}
	// A token without a scope that could apply to the route does not make Flodni read
	// either: upload cannot reach /torrents with any action.
	req := httptest.NewRequest("POST", "/storage/settings", &failingBody{t: t})
	req.Header.Set("Authorization", "Bearer "+e.fx.Token["engine"])
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status %d", rec.Code)
	}
}

// A slow body hits the read deadline set for the peek alone, and the deadline is
// cleared afterwards so a long upload or stream is not cut.
func TestBodyActionReadDeadline(t *testing.T) {
	e := newEngineRun(t)
	prev := peekReadDeadline
	peekReadDeadline = 300 * time.Millisecond
	t.Cleanup(func() { peekReadDeadline = prev })

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer("", newRoute())
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	dial := func() net.Conn {
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		return c
	}
	// Slow: announce 100 bytes, send 10, stall.
	c := dial()
	defer c.Close()
	_, _ = c.Write([]byte("POST /torrents HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer " + e.fx.Token["engine"] +
		"\r\nContent-Length: 100\r\n\r\n{\"action\":"))
	start := time.Now()
	resp, err := http.ReadResponse(bufioReader(c), nil)
	if err != nil {
		t.Fatalf("no answer to a stalled body: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("answered after %s: the deadline did not apply", time.Since(start))
	}
	if got := e.lastCount(); got[2] != "body" {
		t.Errorf("counted %v, want body", got)
	}

	// Fast, then a pause longer than the deadline after the peek: not cut.
	c2 := dial()
	defer c2.Close()
	body := `{"action":"list"}`
	_, _ = c2.Write([]byte("POST /torrents HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer " + e.fx.Token["engine"] +
		"\r\nContent-Length: 17\r\n\r\n" + body))
	resp, err = http.ReadResponse(bufioReader(c2), nil)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("a prompt body: %v %v", resp, err)
	}
}

// ---- the self-calls ----

func TestInProcessTransportCarriesTheInternalIdentity(t *testing.T) {
	prev := inproc.Load()
	t.Cleanup(func() { inproc.Store(prev) })
	var got struct {
		sync.Mutex
		id       apiauth.Identity
		ok       bool
		auth     string
		path     string
		enforced bool
	}
	inproc.Store(&handlerBox{h: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Lock()
		defer got.Unlock()
		got.id, got.ok = apiauth.IdentityFrom(r.Context())
		got.auth, got.path = r.Header.Get("Authorization"), r.URL.Path
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://evil.example:1/after", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "hello")
	})})
	c := &http.Client{Transport: InProcess()}

	// A weak identity in the request's own context (the settings proxy passes the
	// inbound r.Context()) is overwritten by the in-process one.
	weak, _ := apiauth.ParseScopes("webhook")
	ctx := apiauth.WithIdentity(context.Background(), apiauth.Identity{Name: "weak", Scopes: weak})
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://no-such-host.invalid:1/torrents", strings.NewReader(`{"action":"list"}`))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("the host in the URL must be ignored: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	got.Lock()
	if string(b) != "hello" || resp.StatusCode != 200 || got.path != "/torrents" {
		t.Errorf("answer %d %q path %q", resp.StatusCode, b, got.path)
	}
	if !got.ok || got.id.Name != "internal" || !got.id.Internal || !got.id.Scopes.Has(apiauth.ScopeAdmin) {
		t.Errorf("identity = %+v, want the in-process admin one", got.id)
	}
	if got.auth != "" {
		t.Errorf("a credential rode the request: %q", got.auth)
	}
	got.Unlock()

	// A redirect cannot carry anything anywhere: it is served in-process too.
	resp, err = c.Get("http://x.invalid/redirect")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	got.Lock()
	if got.path != "/after" {
		t.Errorf("redirect followed to %q in-process, want /after", got.path)
	}
	got.Unlock()
}

func TestInProcessTransportNeverFallsBackToTheNetwork(t *testing.T) {
	prev := inproc.Load()
	t.Cleanup(func() { inproc.Store(prev) })
	inproc.Store(nil)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hit := make(chan struct{}, 1)
	go func() {
		_ = http.Serve(l, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit <- struct{}{} }))
	}()
	t.Cleanup(func() { _ = l.Close() })
	_, err = (&http.Client{Transport: InProcess()}).Get("http://" + l.Addr().String() + "/")
	if err == nil {
		t.Fatal("an unset handler answered")
	}
	select {
	case <-hit:
		t.Fatal("the transport dialled the network")
	case <-time.After(100 * time.Millisecond):
	}
}

// Through the real router: the internal identity passes every row and sets the key.
func TestInProcessRequestsPassTheRouterAsAdmin(t *testing.T) {
	e := newEngineRun(t)
	prev := inproc.Load()
	t.Cleanup(func() { inproc.Store(prev) })
	inproc.Store(&handlerBox{h: newRoute()})
	c := &http.Client{Transport: InProcess()}
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/torrents", `{"action":"rem","hash":"x"}`}, {"POST", "/settings", `{"action":"set"}`},
		{"GET", "/stream?link=x", ""}, {"POST", "/shutdown", ""},
	} {
		req, _ := http.NewRequest(tc.method, "http://engine.invalid"+tc.path, strings.NewReader(tc.body))
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("%s %s: %d", tc.method, tc.path, resp.StatusCode)
		}
		if last := e.reached[len(e.reached)-1]; !last.Enforced || last.ID.Name != "internal" {
			t.Errorf("%s %s: %+v", tc.method, tc.path, last)
		}
	}
}

func bufioReader(c net.Conn) *bufio.Reader { return bufio.NewReader(c) }

// An engine token gets the open-port /stream rule: no add by link, no fetch of an
// http(s) link, no file: link, no save or preload. The rule, not the scope, is what
// makes "engine cannot add" true; the real handlers run here.
func TestStreamRefusalsForAnEngineToken(t *testing.T) {
	e := newEngineRun(t)
	onScopePass = nil // the real handlers
	hash := strings.Repeat("ab", 20)
	for _, link := range []string{
		"http://127.0.0.1:1/x.torrent", "https://127.0.0.1:1/x.torrent", "file:///etc/passwd",
		"magnet:?xt=urn:btih:" + hash, hash,
	} {
		for _, extra := range []string{"", "&save", "&preload"} {
			rec, _ := e.do("GET", "/stream?index=1&play&link="+urlEscape(link)+extra, e.fx.Token["engine"], "")
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "GET") {
				t.Errorf("link %q%s as engine: %d %q, want the open-port refusal", link, extra, rec.Code, rec.Body.String())
			}
		}
	}
}

func urlEscape(s string) string {
	return strings.NewReplacer(":", "%3A", "/", "%2F", "?", "%3F", "=", "%3D").Replace(s)
}

// The TLS listener shares newServer, so it enforces the same tokens.
func TestTLSListenerEnforcesTheSameTokens(t *testing.T) {
	e := newEngineRun(t)
	ts := httptest.NewUnstartedServer(newServer("", newRoute()).Handler)
	ts.StartTLS()
	t.Cleanup(ts.Close)
	get := func(bearer string) int {
		req, _ := http.NewRequest("POST", ts.URL+"/torrents", strings.NewReader(`{"action":"list"}`))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := get(""); got != 401 {
		t.Errorf("no token over TLS: %d", got)
	}
	if got := get(e.fx.Token["webhook"]); got != 403 {
		t.Errorf("webhook token over TLS: %d", got)
	}
	if got := get(e.fx.Token["engine"]); got != 204 {
		t.Errorf("engine token over TLS: %d", got)
	}
}

// The middleware itself records the action it authorized (the handlers cross-check it),
// for a caller it had to peek; an admin caller is not peeked and is not capped.
func TestPeekedActionIsSetByTheMiddlewareAndAdminIsNotPeeked(t *testing.T) {
	e := newEngineRun(t)
	if _, reached := e.do("POST", "/torrents", e.fx.Token["engine"], `{"action":"list"}`); !reached {
		t.Fatal("engine list refused")
	}
	if last := e.reached[len(e.reached)-1]; !last.HasPeeked || last.Peeked != "list" {
		t.Errorf("engine caller: peeked %q (set=%v), want list", last.Peeked, last.HasPeeked)
	}
	big := `{"action":"rem","hash":"x","pad":"` + strings.Repeat("x", 100<<10) + `"}`
	if rec, reached := e.do("POST", "/torrents", e.fx.Token["admin"], big); !reached || rec.Code != 204 {
		t.Fatalf("admin with a 100 KiB body: %d reached=%v, want it uncapped", rec.Code, reached)
	}
	last := e.reached[len(e.reached)-1]
	if last.HasPeeked || last.Body != big {
		t.Errorf("admin was peeked (%v) or its body changed (%d bytes)", last.HasPeeked, len(last.Body))
	}
	if rec, _ := e.do("POST", "/torrents", e.fx.Token["engine"], big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("engine with the same body: %d, want 413", rec.Code)
	}
}

// ---- names in the engine log, from the action the handler bound ----

func captureStdLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := stdlog.Writer(), stdlog.Flags()
	stdlog.SetOutput(&buf)
	t.Cleanup(func() { stdlog.SetOutput(prevOut); stdlog.SetFlags(prevFlags) })
	return &buf
}

func TestEngineLogsATokenNameOnlyForAMutatingSuccess(t *testing.T) {
	e := newEngineRun(t)
	onScopePass = nil // the real chain, with the real handlers
	buf := captureStdLog(t)
	// A read through the real settings handler: a 200 that is not a write.
	if rec, _ := e.do("POST", "/settings", e.fx.Token["admin"], `{"action":"get"}`); rec.Code != 200 {
		t.Fatalf("settings get: %d", rec.Code)
	}
	// A refused write, a failed write and a GET.
	e.do("POST", "/torrents", e.fx.Token["engine"], `{"action":"rem","hash":"x"}`) // 403
	e.do("POST", "/torrents", e.fx.Token["admin"], `{"action":"zzz"}`)             // 400
	e.do("GET", "/echo", "", "")
	if out := buf.String(); strings.Contains(out, "[Auth] token") {
		t.Errorf("a name was logged for a read, a refusal or a failure: %q", out)
	}
	// A write that succeeds, through a route the test adds behind the same chain.
	route := newRoute()
	route.POST("/probe-write", func(c *gin.Context) { c.Status(200) })
	route.GET("/probe-read", func(c *gin.Context) { c.Status(200) })
	route.POST("/probe-fail", func(c *gin.Context) { c.Status(500) })
	h := newServer("", route).Handler
	for _, tc := range []struct {
		method, path string
		wantLog      bool
	}{{"POST", "/probe-write", true}, {"GET", "/probe-read", false}, {"POST", "/probe-fail", false}} {
		buf.Reset()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+e.fx.Token["admin"])
		h.ServeHTTP(httptest.NewRecorder(), req)
		got := strings.Contains(buf.String(), `[Auth] token "admin" `+tc.method)
		if got != tc.wantLog {
			t.Errorf("%s %s: logged=%v, want %v: %q", tc.method, tc.path, got, tc.wantLog, buf.String())
		}
		if strings.Contains(buf.String(), e.fx.Token["admin"]) {
			t.Error("the token itself was logged")
		}
	}
}

// The action logged is the one the handler bound, so an admin caller (never peeked) does
// not turn every list or get into a logged write.
func TestLoggedWriteFollowsTheBoundAction(t *testing.T) {
	for _, tc := range []struct {
		method, pattern, bound string
		want                   bool
	}{
		{"POST", "/torrents", "list", false}, {"POST", "/torrents", "get", false}, {"POST", "/torrents", "active", false},
		{"POST", "/torrents", "add", true}, {"POST", "/torrents", "rem", true}, {"POST", "/torrents", "drop", true},
		{"POST", "/torrents", "set", true}, {"POST", "/torrents", "", true},
		{"POST", "/settings", "get", false}, {"POST", "/settings", "set", true}, {"POST", "/settings", "def", true},
		{"POST", "/cache", "get", false}, {"POST", "/torrent/upload", "", true}, {"POST", "/shutdown", "", true},
		{"POST", "/storage/settings", "", true}, {"GET", "/storage/settings", "", false}, {"GET", "/stream", "", false},
		{"HEAD", "/play/:hash/:id", "", false}, {"POST", "", "", true},
	} {
		if got := loggedWrite(tc.method, tc.pattern, tc.bound); got != tc.want {
			t.Errorf("%s %s action %q: logged = %v, want %v", tc.method, tc.pattern, tc.bound, got, tc.want)
		}
	}
}

// A caller that gives up before the headers (a disconnect, the client timeout) ends the
// handler: its first Write fails instead of blocking for good.
func TestInProcessTransportEndsTheHandlerWhenTheCallerGivesUp(t *testing.T) {
	prev := inproc.Load()
	t.Cleanup(func() { inproc.Store(prev) })
	release := make(chan struct{})
	done := make(chan error, 1)
	inproc.Store(&handlerBox{h: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, err := w.Write([]byte("late"))
		done <- err
	})})
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://x.invalid/slow", nil)
	errc := make(chan error, 1)
	go func() { _, err := (&http.Client{Transport: InProcess()}).Do(req); errc <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("a cancelled call returned a response")
	}
	close(release)
	select {
	case err := <-done:
		if err == nil {
			t.Error("the handler's write succeeded for a caller that left")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the handler is still blocked in Write: its goroutine leaked")
	}
}

// When the daemon says auth is in force but hands over no store, the engine answers
// nothing (it does not fall back to api_token or to open).
func TestEngineFailsClosedWhenTheStoreIsMissing(t *testing.T) {
	prevAuth, prevClosed, prevTok := Auth, AuthClosed, APIToken
	t.Cleanup(func() { Auth, AuthClosed, APIToken = prevAuth, prevClosed, prevTok })
	Auth, AuthClosed, APIToken = nil, true, ""
	h := newServer("", newRoute()).Handler
	for _, target := range []string{"/echo", "/torrents", "/stream?link=a"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", target, strings.NewReader(`{"action":"list"}`)))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: %d, want 503", target, rec.Code)
		}
	}
	// The router alone, with no identity, refuses a route that is not open.
	rec := httptest.NewRecorder()
	newRoute().ServeHTTP(rec, httptest.NewRequest("POST", "/torrents", strings.NewReader(`{"action":"list"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("router alone: %d, want 401", rec.Code)
	}
}
