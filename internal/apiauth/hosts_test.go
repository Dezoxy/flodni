package apiauth

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

type wrapper func(string, *slog.Logger, http.Handler, ...Option) http.Handler

var wrappers = map[string]wrapper{"control": Middleware, "gostorm": GoStorm}

func do(t *testing.T, wrap wrapper, token string, opts []Option, host string, hdr map[string]string) (int, bool, string) {
	t.Helper()
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	req := httptest.NewRequest("POST", "/torrents", strings.NewReader(`{}`))
	req.Host = host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	var logs bytes.Buffer
	rec := httptest.NewRecorder()
	wrap(token, slog.New(slog.NewTextHandler(&logs, nil)), next, opts...).ServeHTTP(rec, req)
	return rec.Code, called, logs.String()
}

func TestHostAllowlist(t *testing.T) {
	own, _ := os.Hostname()
	tests := []struct {
		name  string
		hosts []string
		host  string
		want  int
	}{
		{"listed host passes", []string{"x.example"}, "x.example", 200},
		{"unlisted host refused", []string{"x.example"}, "evil.example", 403},
		{"subdomain of listed refused", []string{"x.example"}, "a.x.example", 403},
		{"port stripped", []string{"x.example"}, "x.example:9080", 200},
		{"port stripped on unlisted", []string{"x.example"}, "evil.example:9080", 403},
		{"request case ignored", []string{"x.example"}, "X.Example", 200},
		{"config case ignored", []string{"X.Example"}, "x.example", 200},
		{"request trailing dot ignored", []string{"x.example"}, "x.example.", 200},
		{"request trailing dot and port", []string{"x.example"}, "x.example.:9080", 200},
		{"config trailing dot and port ignored", []string{"x.example.:80"}, "x.example", 200},
		{"ipv4 literal passes", []string{"x.example"}, "192.168.1.5:9080", 200},
		{"ipv6 literal passes", []string{"x.example"}, "[::1]:9080", 200},
		{"ipv6 literal no port passes", []string{"x.example"}, "[fd00::5]", 200},
		{"localhost passes", []string{"x.example"}, "localhost:9080", 200},
		{"LOCALHOST passes", []string{"x.example"}, "LOCALHOST", 200},
		{"own hostname passes", []string{"x.example"}, own + ":9080", 200},
		{"empty host refused", []string{"x.example"}, "", 403},
		{"empty list is off", nil, "evil.example", 200},
		{"blank entries are off", []string{"", "  "}, "evil.example", 200},
	}
	for hname, wrap := range wrappers {
		for _, tt := range tests {
			t.Run(hname+"/"+tt.name, func(t *testing.T) {
				code, called, logs := do(t, wrap, "", []Option{WithAllowedHosts(tt.hosts)}, tt.host, nil)
				if code != tt.want {
					t.Fatalf("status = %d, want %d", code, tt.want)
				}
				if tt.want == 403 {
					if called {
						t.Error("handler ran on a refused request")
					}
					if !strings.Contains(logs, "403") || !strings.Contains(logs, "/torrents") {
						t.Errorf("refusal not logged: %q", logs)
					}
				}
			})
		}
	}
}

// The allowlist applies with a token too, before the token check, so even the
// open routes (panel page, readiness probe) refuse an unlisted Host.
func TestHostAllowlistWithToken(t *testing.T) {
	opts := []Option{WithAllowedHosts([]string{"x.example"})}
	for hname, wrap := range wrappers {
		auth := map[string]string{"Authorization": "Bearer " + tok}
		if c, _, _ := do(t, wrap, tok, opts, "x.example", auth); c != 200 {
			t.Errorf("%s: listed host with token = %d, want 200", hname, c)
		}
		if c, _, _ := do(t, wrap, tok, opts, "evil.example", auth); c != 403 {
			t.Errorf("%s: unlisted host with a valid token = %d, want 403", hname, c)
		}
		if c, _, _ := do(t, wrap, tok, opts, "x.example", nil); c != 401 {
			t.Errorf("%s: listed host without token = %d, want 401", hname, c)
		}
		if c, _, _ := do(t, wrap, tok, nil, "evil.example", auth); c != 200 {
			t.Errorf("%s: allowlist off, token = %d, want 200", hname, c)
		}
	}
}

func TestSecFetchSite(t *testing.T) {
	tests := []struct {
		name, val string
		set       bool
		want      int
	}{
		{"cross-site refused", "cross-site", true, 403},
		{"cross-site mixed case refused", "Cross-Site", true, 403},
		{"same-origin passes", "same-origin", true, 200},
		{"same-site passes", "same-site", true, 200},
		{"none passes", "none", true, 200},
		{"missing passes", "", false, 200},
	}
	for hname, wrap := range wrappers {
		for _, method := range []string{"GET", "POST"} {
			for _, tt := range tests {
				t.Run(hname+"/"+method+"/"+tt.name, func(t *testing.T) {
					req := httptest.NewRequest(method, "/torrents", nil)
					req.Host = "x.example"
					if tt.set {
						req.Header.Set("Sec-Fetch-Site", tt.val)
					}
					rec := httptest.NewRecorder()
					var logs bytes.Buffer
					wrap("", slog.New(slog.NewTextHandler(&logs, nil)), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, req)
					if rec.Code != tt.want {
						t.Fatalf("status = %d, want %d", rec.Code, tt.want)
					}
					if tt.want == 403 && !strings.Contains(logs.String(), "403") {
						t.Errorf("refusal not logged: %q", logs.String())
					}
				})
			}
		}
	}
}

// With a token set Sec-Fetch-Site is not applied: the token is the gate.
func TestSecFetchSiteIgnoredWithToken(t *testing.T) {
	for hname, wrap := range wrappers {
		c, _, _ := do(t, wrap, tok, nil, "x.example", map[string]string{"Authorization": "Bearer " + tok, "Sec-Fetch-Site": "cross-site"})
		if c != 200 {
			t.Errorf("%s: status = %d, want 200", hname, c)
		}
	}
}

// A TLS tunnel keeps the original Host and the browser's fetches are
// same-origin: with the name listed, nothing refuses the panel.
func TestTunnelCasePasses(t *testing.T) {
	hdr := map[string]string{"Origin": "https://x.example", "Sec-Fetch-Site": "same-origin"}
	opts := []Option{WithAllowedHosts([]string{"x.example"})}
	for hname, wrap := range wrappers {
		if c, called, _ := do(t, wrap, "", opts, "x.example", hdr); c != 200 || !called {
			t.Errorf("%s: tunnel request = %d (called=%v), want 200", hname, c, called)
		}
	}
}

// The rebinding case: the attacker's name is in both Origin and Host, so the
// Origin check passes, and only the allowlist stops it.
func TestRebindingStoppedByAllowlist(t *testing.T) {
	hdr := map[string]string{"Origin": "http://evil.example:9080"}
	for hname, wrap := range wrappers {
		if c, _, _ := do(t, wrap, "", nil, "evil.example:9080", hdr); c != 200 {
			t.Errorf("%s: without allowlist = %d, want 200 (the documented gap)", hname, c)
		}
		if c, _, _ := do(t, wrap, "", []Option{WithAllowedHosts([]string{"x.example"})}, "evil.example:9080", hdr); c != 403 {
			t.Errorf("%s: with allowlist = %d, want 403", hname, c)
		}
	}
}

// An all-invalid list fails closed in the daemon wiring by passing the
// always-allowed hosts only: a named Host is refused, an IP literal passes.
func TestOnlyAlwaysAllowedHostsRefusesNames(t *testing.T) {
	opts := []Option{WithAllowedHosts([]string{"127.0.0.1"})}
	for hname, wrap := range wrappers {
		if c, _, _ := do(t, wrap, "", opts, "x.example", nil); c != 403 {
			t.Errorf("%s: named host = %d, want 403", hname, c)
		}
		if c, _, _ := do(t, wrap, "", opts, "192.168.1.5:9080", nil); c != 200 {
			t.Errorf("%s: ip literal = %d, want 200", hname, c)
		}
	}
}

// Navigations to the routes the gate treats as open (the panel page, the health
// probe, the engine's /echo) are not refused as cross-site; data routes still are.
func TestCrossSiteNavigationToOpenRoutes(t *testing.T) {
	hdr := map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate"}
	cases := []struct {
		wrap         string
		method, path string
		want         int
	}{
		{"control", "GET", "/control", 200},
		{"control", "GET", "/api/health", 200},
		{"control", "POST", "/api/config", 403},
		{"control", "GET", "/api/config", 403},
		{"control", "POST", "/control", 403},
		{"gostorm", "GET", "/echo", 200},
		{"gostorm", "GET", "/stream", 403},
		{"gostorm", "POST", "/echo", 403},
	}
	for _, tc := range cases {
		called := false
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Host = "x.example"
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		wrappers[tc.wrap]("", nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).ServeHTTP(rec, req)
		if rec.Code != tc.want || called != (tc.want == 200) {
			t.Errorf("%s %s %s: status %d called=%v, want %d", tc.wrap, tc.method, tc.path, rec.Code, called, tc.want)
		}
	}
}

// The refusal log names the Host, quoted: the only way to see which name to list.
func TestHostRefusalLogsTheHost(t *testing.T) {
	for hname, wrap := range wrappers {
		_, _, logs := do(t, wrap, "", []Option{WithAllowedHosts([]string{"x.example"})}, "evil.example:9080", nil)
		if !strings.Contains(logs, "evil.example:9080") {
			t.Errorf("%s: refusal does not name the Host: %q", hname, logs)
		}
	}
}
