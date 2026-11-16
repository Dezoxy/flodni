package apiauth

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A token parameter where it is not allowed is refused before the lookup: no hash is
// compared, so the answer cannot be an oracle for a valid token.
func TestQueryTokenOffTheWebhooksDoesNoLookup(t *testing.T) {
	good := fakeToken('a')
	tk, err := loadFile(t, `{"tokens":[`+entryJSON("a", `"admin"`, hashHex(good))+`]}`)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	tk.cmp = cmpCounter(&n)
	reg := NewRegistrar()
	reg.Handle("/api/sessions", func(http.ResponseWriter, *http.Request) {})
	reg.Handle("/webhook", func(http.ResponseWriter, *http.Request) {})
	ctl := Middleware("", nil, reg, WithTokens(tk), WithPattern(reg.Pattern))
	eng := GoStorm("", nil, http.NotFoundHandler(), WithTokens(tk))
	for _, tc := range []struct {
		h              http.Handler
		method, target string
		bearer         string
	}{
		{ctl, "GET", "/api/sessions?token=" + good, ""},
		{ctl, "GET", "/api/sessions?token=" + good, good},
		{ctl, "GET", "/api/sessions?token=nonsense", good},
		{ctl, "GET", "/api/sessions?token=", ""},
		{eng, "POST", "/torrents?token=" + good, good},
		{eng, "POST", "/torrents?token=" + good, ""},
	} {
		before := n
		req := httptest.NewRequest(tc.method, tc.target, nil)
		if tc.bearer != "" {
			req.Header.Set("Authorization", "Bearer "+tc.bearer)
		}
		rec := httptest.NewRecorder()
		tc.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", tc.target, rec.Code)
		}
		if n != before {
			t.Errorf("%s: %d hashes compared, want none", tc.target, n-before)
		}
	}
	// The webhook route does look the value up.
	before := n
	ctl.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/webhook?token="+good, nil))
	if n == before {
		t.Error("the webhook route did not look the token up")
	}
}

// The open sets the gate uses are the table's open rows, so the two cannot drift.
func TestOpenSetMatchesTheTable(t *testing.T) {
	for _, row := range routeTable {
		var open func(*http.Request) bool
		switch row.Server {
		case Control:
			open = controlOpen
		case Engine:
			open = gostormOpen
		}
		req := httptest.NewRequest(row.Method, row.Pattern, nil)
		if got := open(req); got != (row.Scope == ScopeOpen) {
			t.Errorf("%s %s: gate open=%v, table scope %s", row.Method, row.Pattern, got, row.Scope)
		}
	}
	// The webhook routes the gate lets carry ?token= are the table's webhook rows.
	for _, row := range routeTable {
		if row.Server != Control {
			continue
		}
		req := httptest.NewRequest(row.Method, row.Pattern, nil)
		if got := controlWebhook(req); got != (row.Scope == ScopeWebhook) {
			t.Errorf("%s %s: gate webhook=%v, table scope %s", row.Method, row.Pattern, got, row.Scope)
		}
	}
}

// The recorder that feeds the success log line keeps the writer's contract: a handler
// that only writes still logs the token's name (the status defaults to 200), and the
// wrapper is an http.Flusher and unwraps for http.NewResponseController (handleRestart
// asserts http.Flusher to get its reply out before the exit).
func TestRecorderKeepsTheWriterContract(t *testing.T) {
	good := fakeToken('a')
	tk, err := loadFile(t, `{"tokens":[`+entryJSON("a", `"library"`, hashHex(good))+`]}`)
	if err != nil {
		t.Fatal(err)
	}
	var flusher bool
	var flushErr error
	reg := NewRegistrar()
	reg.Handle("/api/library/add", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok")) // no WriteHeader: the status is 200
		_, flusher = w.(http.Flusher)
		flushErr = http.NewResponseController(w).Flush()
	})
	var logs bytes.Buffer
	h := Middleware("", slog.New(slog.NewTextHandler(&logs, nil)), reg, WithTokens(tk), WithPattern(reg.Pattern))
	req := httptest.NewRequest("POST", "/api/library/add", nil)
	req.Header.Set("Authorization", "Bearer "+good)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !flusher || flushErr != nil || !rec.Flushed {
		t.Errorf("status %d flusher %v flush error %v flushed %v", rec.Code, flusher, flushErr, rec.Flushed)
	}
	if n := strings.Count(logs.String(), "token=a "); n != 1 {
		t.Errorf("the name was logged %d times, want once: %s", n, logs.String())
	}
	if strings.Contains(logs.String(), good) {
		t.Error("the token reached the log")
	}
}

// A closed gate (no credential store) answers 503 and says so: one error when it is
// built, and per request one log line and one count, never silence.
func TestClosedGateIsLoggedAndCountedOnce(t *testing.T) {
	for name, build := range map[string]func(*slog.Logger, Observer) http.Handler{
		"control": func(l *slog.Logger, o Observer) http.Handler {
			return Middleware("", l, http.NotFoundHandler(), WithTokens(nil), WithObserver(o))
		},
		"engine": func(l *slog.Logger, o Observer) http.Handler {
			return GoStorm("", l, http.NotFoundHandler(), WithTokens(nil), WithObserver(o))
		},
	} {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			var seen []string
			h := build(slog.New(slog.NewTextHandler(&logs, nil)), func(route string, _ Scope, reason string) {
				seen = append(seen, route+" "+reason)
			})
			if n := strings.Count(logs.String(), "level=ERROR"); n != 1 {
				t.Errorf("building a closed gate logged %d errors, want 1: %s", n, logs.String())
			}
			logs.Reset()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/echo", nil))
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status %d, want 503", rec.Code)
			}
			if len(seen) != 1 || seen[0] != RoutePreRoute+" "+ReasonClosed {
				t.Errorf("counted %q, want one %q", seen, RoutePreRoute+" "+ReasonClosed)
			}
			if n := strings.Count(logs.String(), "503"); n != 1 {
				t.Errorf("the request logged %d lines with 503, want 1: %s", n, logs.String())
			}
		})
	}
}

// In legacy mode a wrong bearer falls through to the query token only on a webhook route
// that carries one; it never reaches the session cookie. Each clause of that rule is
// pinned: a wrong bearer with no query token, and one off the webhook routes.
func TestLegacyWrongBearerNeverReachesTheCookie(t *testing.T) {
	cookieAsked := false
	cookie := func(*http.Request) (string, bool, error) { cookieAsked = true; return "someone", true, nil }
	h := Middleware("legacy-secret", nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), WithCookie(cookie))
	for name, target := range map[string]string{
		"webhook, no query token":    "/webhook",
		"webhook, empty query token": "/webhook?token=",
		"not a webhook, query token": "/api/sessions?token=legacy-secret",
		"not a webhook, no query":    "/api/sessions",
		"other webhook, bad token":   "/plex/webhook?token=nope",
	} {
		cookieAsked = false
		req := httptest.NewRequest("POST", target, nil)
		req.Header.Set("Authorization", "Bearer wrong")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || cookieAsked {
			t.Errorf("%s: status %d, cookie asked %v; want 401 and no cookie", name, rec.Code, cookieAsked)
		}
	}
}
