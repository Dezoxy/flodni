package apiauth

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const tok = "s3cret-token"

func serve(token, method, target, auth string) *httptest.ResponseRecorder {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(method, target, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	Middleware(token, nil, next).ServeHTTP(rec, req)
	return rec
}

func TestMiddleware(t *testing.T) {
	tests := []struct {
		name, token, method, target, auth string
		want                              int
	}{
		{"off passes api", "", "POST", "/api/restart", "", 200},
		{"off passes bad header", "", "GET", "/metrics", "Bearer nope", 200},
		{"valid header", tok, "GET", "/api/config", "Bearer " + tok, 200},
		{"missing header", tok, "GET", "/api/config", "", 401},
		{"wrong header", tok, "GET", "/api/config", "Bearer wrong", 401},
		{"wrong length header", tok, "GET", "/api/config", "Bearer " + tok + "x", 401},
		{"basic scheme", tok, "GET", "/api/config", "Basic " + tok, 401},
		{"control page open", tok, "GET", "/control", "", 200},
		{"control POST not open", tok, "POST", "/control", "", 401},
		{"health open", tok, "GET", "/api/health", "", 200},
		{"health POST not open", tok, "POST", "/api/health", "", 401},
		{"health subpath not open", tok, "GET", "/api/health/x", "", 401},
		{"metrics needs token", tok, "GET", "/metrics", "", 401},
		{"metrics subroute needs token", tok, "GET", "/metrics/ttff", "", 401},
		{"restart needs token", tok, "POST", "/api/restart", "", 401},
		{"plex webhook query", tok, "POST", "/plex/webhook?token=" + tok, "", 200},
		{"plex webhook wrong query", tok, "POST", "/plex/webhook?token=bad", "", 401},
		{"plex webhook no token", tok, "POST", "/plex/webhook", "", 401},
		{"webhook query", tok, "POST", "/webhook?token=" + tok, "", 200},
		{"webhook header", tok, "POST", "/webhook", "Bearer " + tok, 200},
		{"torrents health needs token", tok, "GET", "/api/torrents/health", "", 401},
		{"torrents health with header", tok, "GET", "/api/torrents/health", "Bearer " + tok, 200},
		{"query token rejected on torrents health", tok, "GET", "/api/torrents/health?token=" + tok, "", 401},
		{"query token rejected on api", tok, "GET", "/api/sessions?token=" + tok, "", 401},
		{"query token rejected on metrics", tok, "GET", "/metrics?token=" + tok, "", 401},
		{"prometheus needs token", tok, "GET", "/metrics/prometheus", "", 401},
		{"prometheus with header", tok, "GET", "/metrics/prometheus", "Bearer " + tok, 200},
		{"query token rejected on prometheus", tok, "GET", "/metrics/prometheus?token=" + tok, "", 401},
		{"warmup evict needs token", tok, "POST", "/api/warmup/evict", "", 401},
		{"warmup evict wrong token", tok, "POST", "/api/warmup/evict", "Bearer wrong", 401},
		{"warmup evict with header", tok, "POST", "/api/warmup/evict", "Bearer " + tok, 200},
		{"query token rejected on warmup evict", tok, "POST", "/api/warmup/evict?token=" + tok, "", 401},
		{"query token rejected on library", tok, "POST", "/api/library/add?token=" + tok, "", 401},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(tt.token, tt.method, tt.target, tt.auth)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			if tt.want == 401 && rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want Bearer", rec.Header().Get("WWW-Authenticate"))
			}
		})
	}
}

func serveGoStorm(token, method, target, auth string) *httptest.ResponseRecorder {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(method, target, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	GoStorm(token, nil, next).ServeHTTP(rec, req)
	return rec
}

func TestGoStorm(t *testing.T) {
	tests := []struct {
		name, token, method, target, auth string
		want                              int
	}{
		{"off passes torrents", "", "POST", "/torrents", "", 200},
		{"off passes shutdown", "", "GET", "/shutdown", "Bearer nope", 200},
		{"echo open", tok, "GET", "/echo", "", 200},
		{"echo POST not open", tok, "POST", "/echo", "", 401},
		{"echo subpath not open", tok, "GET", "/echo/x", "", 401},
		{"torrents valid header", tok, "POST", "/torrents", "Bearer " + tok, 200},
		{"torrents missing header", tok, "POST", "/torrents", "", 401},
		{"torrents wrong header", tok, "POST", "/torrents", "Bearer wrong", 401},
		{"basic scheme", tok, "POST", "/torrents", "Basic " + tok, 401},
		{"stream needs token", tok, "GET", "/stream?link=abc&index=1&play", "", 401},
		{"stream with header", tok, "GET", "/stream?link=abc&index=1&play", "Bearer " + tok, 200},
		{"play needs token", tok, "GET", "/play/abc/1", "", 401},
		{"shutdown needs token", tok, "GET", "/shutdown", "", 401},
		{"settings needs token", tok, "POST", "/settings", "", 401},
		{"query token refused on stream", tok, "GET", "/stream?link=abc&token=" + tok, "", 401},
		{"query token refused on torrents", tok, "POST", "/torrents?token=" + tok, "", 401},
		{"query token refused on webhook path", tok, "POST", "/webhook?token=" + tok, "", 401},
		{"query token refused on plex webhook path", tok, "POST", "/plex/webhook?token=" + tok, "", 401},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serveGoStorm(tt.token, tt.method, tt.target, tt.auth)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			if tt.want == 401 && rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want Bearer", rec.Header().Get("WWW-Authenticate"))
			}
		})
	}
}

// Refusal lines quote the path (%q), so a control character in it cannot forge
// or split a log line, and carry no query.
func TestRefusalLinesQuotePath(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

	// 401: a token is set and none presented.
	req := httptest.NewRequest(http.MethodGet, "/api/x%0Aforged?token=SECRET", nil)
	Middleware(tok, logger, next).ServeHTTP(httptest.NewRecorder(), req)
	// 403: open port, cross-origin.
	req = httptest.NewRequest(http.MethodPost, "/api/y%0Aforged?token=SECRET", nil)
	req.Header.Set("Origin", "https://evil.example")
	Middleware("", logger, next).ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if strings.Count(out, "\n") != 2 {
		t.Errorf("want two log lines, got %q", out)
	}
	for _, want := range []string{`401 GET \"/api/x\\nforged\"`, `403 cross-origin POST \"/api/y\\nforged\"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %q", want, out)
		}
	}
	if strings.Contains(out, "SECRET") {
		t.Errorf("query token in log: %q", out)
	}
}
