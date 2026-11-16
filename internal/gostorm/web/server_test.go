package web

import (
	"bytes"
	"flodni/internal/apiauth"
	"flodni/internal/gostorm/web/api"
	"fmt"
	"github.com/gin-gonic/gin"
	stdlog "log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The engine must not tell a browser that any origin may read its responses or
// reach it from a private network.
func TestRouteSetsNoCORSHeaders(t *testing.T) {
	route := newRoute()
	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/echo"},
		{http.MethodOptions, "/torrents"},
	} {
		req := httptest.NewRequest(tc.method, tc.target, nil)
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Access-Control-Request-Method", "POST")
		req.Header.Set("Access-Control-Request-Private-Network", "true")
		rec := httptest.NewRecorder()
		route.ServeHTTP(rec, req)
		for k := range rec.Header() {
			if len(k) > 14 && k[:14] == "Access-Control" {
				t.Errorf("%s %s: sent %s: %q", tc.method, tc.target, k, rec.Header().Get(k))
			}
		}
	}
}

// The engine port logs its Origin refusals through the logger the daemon hands
// over, the way the control port does (production wiring: newServer).
func TestServerLogsCrossOriginRefusal(t *testing.T) {
	var buf bytes.Buffer
	prev := Logger
	Logger = slog.New(slog.NewTextHandler(&buf, nil))
	t.Cleanup(func() { Logger = prev })

	srv := newServer("127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler ran on a refused request")
	}))
	req := httptest.NewRequest(http.MethodPost, "/torrents", nil)
	req.Host = "x.example"
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	if !strings.Contains(buf.String(), "403") || !strings.Contains(buf.String(), "/torrents") {
		t.Errorf("refusal not logged: %q", buf.String())
	}
}

// The router marks a request as token-enforced when the gate authenticated it as a
// caller holding admin (the legacy token holds every scope); the handlers read the
// mark to allow an add by GET. With no token configured nothing is marked.
func TestNewRouteSetsTokenEnforcedKey(t *testing.T) {
	prev, prevAuth := APIToken, Auth
	t.Cleanup(func() { APIToken, Auth = prev, prevAuth })
	Auth = nil
	for _, tok := range []string{"", "secret"} {
		APIToken = tok
		route := newRoute()
		route.GET("/probe", func(c *gin.Context) { c.String(200, "%v", c.GetBool(api.TokenEnforcedKey)) })
		req := httptest.NewRequest(http.MethodGet, "/probe", nil)
		if tok != "" {
			legacy, _ := apiauth.Load(apiauth.LoadOptions{APIToken: tok})
			_, scopes, _ := legacy.Lookup(tok)
			req = req.WithContext(apiauth.WithIdentity(req.Context(), apiauth.Identity{Name: "legacy", Scopes: scopes}))
		}
		rec := httptest.NewRecorder()
		route.ServeHTTP(rec, req)
		if want := tok != ""; rec.Body.String() != fmt.Sprint(want) {
			t.Errorf("token %q: key = %s, want %v", tok, rec.Body.String(), want)
		}
	}
}

// A panicking handler on the engine router answers 500 and logs the panic with
// a method and path only: no header, cookie, proxy JWT or query token.
func TestRoutePanicLogsNoRequestSecrets(t *testing.T) {
	var buf bytes.Buffer
	prevOut, prevFlags := stdlog.Writer(), stdlog.Flags()
	stdlog.SetOutput(&buf)
	t.Cleanup(func() { stdlog.SetOutput(prevOut); stdlog.SetFlags(prevFlags) })

	route := newRoute()
	route.GET("/boom", func(c *gin.Context) { panic("handler exploded") })
	req := httptest.NewRequest(http.MethodGet, "/boom?token=SECRETQUERY&link=x", nil)
	req.Header.Set("Cookie", "flodni_session=SECRETCOOKIE")
	req.Header.Set("Authorization", "Bearer SECRETBEARER")
	req.Header.Set("X-Forwarded-Jwt", "SECRETJWT")
	rec := httptest.NewRecorder()
	route.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	out := buf.String()
	if !strings.Contains(out, "handler exploded") || !strings.Contains(out, `GET "/boom"`) {
		t.Errorf("panic not logged with method and path: %q", out)
	}
	for _, s := range []string{"SECRET", "Cookie", "flodni_session", "Authorization", "X-Forwarded-Jwt"} {
		if strings.Contains(out, s) {
			t.Errorf("log holds %q: %q", s, out)
		}
	}
}

// Sweep: requests carrying ?token=SECRET through the engine's gate (refused and
// panicking) leave no token in the gate's log or the panic log.
func TestEngineLogsHoldNoToken(t *testing.T) {
	var gateBuf, stdBuf bytes.Buffer
	prevLogger, prevToken := Logger, APIToken
	Logger = slog.New(slog.NewTextHandler(&gateBuf, nil))
	APIToken = "right-token"
	prevOut := stdlog.Writer()
	stdlog.SetOutput(&stdBuf)
	t.Cleanup(func() { Logger, APIToken = prevLogger, prevToken; stdlog.SetOutput(prevOut) })

	route := newRoute()
	route.GET("/boom", func(c *gin.Context) { panic("boom") })
	srv := newServer("127.0.0.1:0", route)
	for _, target := range []string{
		"/torrents?token=SECRET", "/boom?token=right-token", "/boom?access_token=SECRET",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Host = "x.example"
		req.Header.Set("Origin", "https://evil.example")
		srv.Handler.ServeHTTP(httptest.NewRecorder(), req)
	}
	// Authorized (header) panic path, query token present.
	req := httptest.NewRequest(http.MethodGet, "/boom?token=SECRET", nil)
	req.Header.Set("Authorization", "Bearer right-token")
	srv.Handler.ServeHTTP(httptest.NewRecorder(), req)

	for name, out := range map[string]string{"gate": gateBuf.String(), "panic": stdBuf.String()} {
		if out == "" {
			t.Errorf("%s log is empty", name)
		}
		if strings.Contains(out, "SECRET") || strings.Contains(out, "right-token") {
			t.Errorf("%s log holds a token: %q", name, out)
		}
	}
}

// The engine's http.Server prints a panic outside gin's Recovery (WebLogger,
// the blocker, the gate) through ErrorLog; its text must be redacted.
func TestServerErrorLogRedacts(t *testing.T) {
	var buf bytes.Buffer
	prevOut := stdlog.Writer()
	stdlog.SetOutput(&buf)
	t.Cleanup(func() { stdlog.SetOutput(prevOut) })
	srv := newServer("127.0.0.1:0", http.NotFoundHandler())
	if srv.ErrorLog == nil {
		t.Fatal("ErrorLog not set")
	}
	srv.ErrorLog.Printf("panic: bad ?token=SECRET")
	if strings.Contains(buf.String(), "SECRET") || buf.Len() == 0 {
		t.Errorf("got %q", buf.String())
	}
}
