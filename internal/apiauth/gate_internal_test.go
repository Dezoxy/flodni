package apiauth

import (
	"net/http"
	"net/http/httptest"
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
