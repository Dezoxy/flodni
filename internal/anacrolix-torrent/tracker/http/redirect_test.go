package httpTracker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

const redirectBody = "d8:intervali1800ee"

func announceTo(t *testing.T, raw string, opts NewClientOpts) (AnnounceResponse, error) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return NewClient(u, opts).Announce(context.Background(), AnnounceRequest{}, AnnounceOpt{})
}

// An https tracker that redirects to plain http on the same host is not followed, and the http
// hop never receives the request: the passkey in its query must not travel in clear text.
func TestAnnounceRefusesAnHTTPSToHTTPRedirect(t *testing.T) {
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		plainHits.Add(1)
		w.Write([]byte(redirectBody))
	}))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/announce?passkey=PASSKEYX", http.StatusFound)
	}))
	defer secure.Close()
	// same host (127.0.0.1) and a different port: a scheme downgrade is refused regardless of the port
	if _, err := announceTo(t, secure.URL+"/announce?passkey=PASSKEYX", NewClientOpts{InsecureSkipVerify: true}); err == nil {
		t.Fatal("announce followed an https to http redirect")
	}
	if n := plainHits.Load(); n != 0 {
		t.Fatalf("the http hop received %d request(s)", n)
	}
}

// A redirect to another host:port is refused.
func TestAnnounceRefusesACrossHostRedirect(t *testing.T) {
	var otherHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		otherHits.Add(1)
		w.Write([]byte(redirectBody))
	}))
	defer other.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/announce?passkey=PASSKEYX", http.StatusFound)
	}))
	defer first.Close()
	if _, err := announceTo(t, first.URL+"/announce?passkey=PASSKEYX", NewClientOpts{}); err == nil {
		t.Fatal("announce followed a redirect to another host:port")
	}
	if n := otherHits.Load(); n != 0 {
		t.Fatalf("the other host received %d request(s)", n)
	}
}

// A redirect that keeps the scheme and host:port is followed, up to 3 hops.
func TestAnnounceFollowsASameOriginRedirectUpToThreeHops(t *testing.T) {
	for _, tc := range []struct {
		hops int
		ok   bool
	}{{1, true}, {3, true}, {4, false}} {
		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var n int
			if i := strings.LastIndex(r.URL.Path, "/hop"); i >= 0 {
				for _, c := range r.URL.Path[i+4:] {
					n = n*10 + int(c-'0')
				}
			}
			if n < tc.hops {
				http.Redirect(w, r, srv.URL+"/announce/hop"+string(rune('0'+n+1)), http.StatusFound)
				return
			}
			w.Write([]byte(redirectBody))
		}))
		res, err := announceTo(t, srv.URL+"/announce/hop0", NewClientOpts{})
		srv.Close()
		if tc.ok && (err != nil || res.Interval != 1800) {
			t.Errorf("%d hops: %v, interval %d", tc.hops, err, res.Interval)
		}
		if !tc.ok && err == nil {
			t.Errorf("%d hops were followed", tc.hops)
		}
	}
}
