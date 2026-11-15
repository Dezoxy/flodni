package apiauth

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Open port (no token): a browser page on another origin must not be able to
// drive the API with a "simple" cross-site POST. Requests with no Origin
// (webhooks, curl, scripts, the same-origin panel and its proxy) pass.
func TestOpenPortRefusesCrossOrigin(t *testing.T) {
	tests := []struct {
		name, host, origin string
		noOrigin           bool
		want               int
	}{
		{"no origin", "x.example:9080", "", true, 200},
		{"same origin", "x.example:9080", "http://x.example:9080", false, 200},
		{"tls tunnel: https origin, plain http host", "x.example", "https://x.example", false, 200},
		{"port ignored", "x.example:9080", "http://x.example:8080", false, 200},
		{"case ignored", "x.example", "https://X.Example", false, 200},
		{"ipv6 literal", "[::1]:9080", "http://[::1]:9080", false, 200},
		{"ipv6 literal no port", "[::1]", "https://[::1]", false, 200},
		{"cross origin", "x.example", "https://evil.example", false, 403},
		{"cross origin on lan address", "192.168.1.5:9080", "http://evil.example", false, 403},
		{"subdomain is another host", "x.example", "https://a.x.example", false, 403},
		{"origin null", "x.example", "null", false, 403},
		{"empty origin value", "x.example", "", false, 403},
		{"unparsable origin", "x.example", "http://[::1", false, 403},
		{"origin without host", "x.example", "https://", false, 403},
	}
	handlers := map[string]func(string, *slog.Logger, http.Handler) http.Handler{"control": Middleware, "gostorm": GoStorm}
	for hname, wrap := range handlers {
		for _, tt := range tests {
			t.Run(hname+"/"+tt.name, func(t *testing.T) {
				called := false
				next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
				req := httptest.NewRequest("POST", "/torrents", strings.NewReader(`{}`))
				req.Host = tt.host
				req.Header.Set("Content-Type", "text/plain")
				if !tt.noOrigin {
					req.Header["Origin"] = []string{tt.origin}
				}
				var logs bytes.Buffer
				rec := httptest.NewRecorder()
				wrap("", slog.New(slog.NewTextHandler(&logs, nil)), next).ServeHTTP(rec, req)
				if rec.Code != tt.want {
					t.Fatalf("status = %d, want %d", rec.Code, tt.want)
				}
				if tt.want == 403 {
					if called {
						t.Error("handler ran on a refused request")
					}
					if !strings.Contains(logs.String(), "403") || !strings.Contains(logs.String(), "/torrents") {
						t.Errorf("refusal not logged: %q", logs.String())
					}
				}
			})
		}
	}
}

// With a token set the Origin check is not applied: the token is the gate.
func TestTokenModeIgnoresOrigin(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	for name, wrap := range map[string]func(string, *slog.Logger, http.Handler) http.Handler{"control": Middleware, "gostorm": GoStorm} {
		for _, tc := range []struct {
			auth string
			want int
		}{{"Bearer " + tok, 200}, {"", 401}} {
			req := httptest.NewRequest("POST", "/torrents", nil)
			req.Host = "x.example"
			req.Header.Set("Origin", "https://evil.example")
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			wrap(tok, nil, next).ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("%s auth=%q: status = %d, want %d", name, tc.auth, rec.Code, tc.want)
			}
		}
	}
}

// Two Origin headers are ambiguous and refused; a GET with a foreign Origin is
// refused like any other method (an XHR or fetch GET carries one).
func TestOpenPortOriginEdgeCases(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	for name, wrap := range map[string]func(string, *slog.Logger, http.Handler) http.Handler{"control": Middleware, "gostorm": GoStorm} {
		req := httptest.NewRequest("GET", "/api/config", nil)
		req.Host = "x.example"
		req.Header.Set("Origin", "https://evil.example")
		rec := httptest.NewRecorder()
		wrap("", nil, next).ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Errorf("%s GET foreign origin: %d, want 403", name, rec.Code)
		}
		req = httptest.NewRequest("POST", "/api/config", nil)
		req.Host = "x.example"
		req.Header["Origin"] = []string{"https://x.example", "https://x.example"}
		rec = httptest.NewRecorder()
		wrap("", nil, next).ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Errorf("%s two Origin values: %d, want 403", name, rec.Code)
		}
	}
}
