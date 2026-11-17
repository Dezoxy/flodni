package daemon

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"flodni/internal/apiauth"
	"flodni/internal/apiauth/authtest"
)

// Every control route, answered or refused, carries the two headers that cost nothing:
// nosniff (a JSON body is never sniffed into HTML) and same-origin Referrer-Policy.
func TestControlResponsesCarryNosniffAndReferrerPolicy(t *testing.T) {
	s := newScopedFixture(t, "")
	n := 0
	for _, row := range authtest.Golden(t) {
		if row.Server != "control" {
			continue
		}
		for _, cred := range []string{"", "admin"} { // refused (401) and served
			req := httptest.NewRequest(row.Method, authtest.Path(row.Pattern), nil)
			req.Host = "panel.example:9080"
			if cred != "" {
				req.Header.Set("Authorization", "Bearer "+s.fx.Token[cred])
			}
			rec := httptest.NewRecorder()
			s.h.ServeHTTP(rec, req)
			n++
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("%s %s as %q (%d): X-Content-Type-Options = %q", row.Method, row.Pattern, cred, rec.Code, got)
			}
			if got := rec.Header().Get("Referrer-Policy"); got != "same-origin" {
				t.Errorf("%s %s as %q (%d): Referrer-Policy = %q", row.Method, row.Pattern, cred, rec.Code, got)
			}
		}
	}
	if n == 0 {
		t.Fatal("no control route was driven")
	}
	// A refusal before any route: a Host that is not allowed, and a path no route has.
	for _, host := range []string{"evil.example:9080", "panel.example:9080"} {
		req := httptest.NewRequest(http.MethodGet, "/nope", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, req)
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Referrer-Policy") != "same-origin" {
			t.Errorf("unrouted GET /nope on %s (%d) lacks the headers", host, rec.Code)
		}
	}
}

// HSTS only where the panel is known to be https: a request over TLS, the first
// X-Forwarded-Proto element, or auth_panel_https. Never over plain http.
func TestHSTSOnlyWhenThePanelIsKnownHTTPS(t *testing.T) {
	plain := newScopedFixture(t, "")
	cfgHTTPS := newScopedFixture(t, `,"auth_panel_https":true`)
	for _, tc := range []struct {
		name string
		fx   *scopedFixture
		mod  func(*http.Request)
		want bool
	}{
		{"plain http", plain, func(*http.Request) {}, false},
		{"forwarded http", plain, func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "http") }, false},
		{"forwarded https", plain, func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") }, true},
		{"forwarded list, first https", plain, func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https, http") }, true},
		{"forwarded list, first http", plain, func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "http, https") }, false},
		{"TLS", plain, func(r *http.Request) { r.TLS = &tls.ConnectionState{} }, true},
		{"auth_panel_https", cfgHTTPS, func(*http.Request) {}, true},
	} {
		for _, path := range []string{"/control", "/api/live", "/nope"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Host = "panel.example:9080"
			tc.mod(req)
			rec := httptest.NewRecorder()
			tc.fx.h.ServeHTTP(rec, req)
			got := rec.Header().Get("Strict-Transport-Security")
			if tc.want && got != "max-age=31536000" {
				t.Errorf("%s %s: HSTS = %q, want max-age=31536000", tc.name, path, got)
			}
			if !tc.want && got != "" {
				t.Errorf("%s %s: HSTS = %q over plain http", tc.name, path, got)
			}
		}
	}
}

// The panel keeps its frame refusal next to the new headers, through the real gate.
func TestPanelKeepsFrameHeadersThroughTheGate(t *testing.T) {
	s := newScopedFixture(t, "")
	stub := func(http.ResponseWriter, *http.Request) {}
	h := fillStubs(controlHandlers{}, stub)
	h.Panel = handlePanel
	routes := apiauth.NewRegistrar()
	registerControlRoutes(routes, h)
	gate := newControlGate(routes, s.fx.Tokens, nil, gc())
	req := httptest.NewRequest(http.MethodGet, "/control", nil)
	req.Host = "panel.example:9080"
	rec := httptest.NewRecorder()
	gate.ServeHTTP(rec, req)
	hd := rec.Header()
	if hd.Get("Content-Security-Policy") != "frame-ancestors 'none'" || hd.Get("X-Frame-Options") != "DENY" ||
		hd.Get("X-Content-Type-Options") != "nosniff" || hd.Get("Referrer-Policy") != "same-origin" {
		t.Errorf("panel headers: %v", hd)
	}
}
