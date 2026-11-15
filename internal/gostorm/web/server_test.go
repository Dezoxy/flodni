package web

import (
	"bytes"
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
