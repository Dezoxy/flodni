package web

import (
	"net/http"
	"net/http/httptest"
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
