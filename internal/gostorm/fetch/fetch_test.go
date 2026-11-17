package fetch

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The redirect rule, on its own: same host up to 3 hops, nothing across hosts.
func TestClientRedirectRule(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the other host was contacted")
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cross":
			http.Redirect(w, r, other.URL, http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		}
	}))
	defer srv.Close()
	c := NewClient(0)
	for _, p := range []string{"/cross", "/loop"} {
		if resp, err := c.Get(srv.URL + p); err == nil {
			resp.Body.Close()
			t.Errorf("%s: redirect followed", p)
		}
	}
}
