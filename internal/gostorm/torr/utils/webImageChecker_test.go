package utils

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func imageServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("png"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheckImgUrlAcceptsAnImage(t *testing.T) {
	if !CheckImgUrl(imageServer(t).URL + "/p.png") {
		t.Fatal("an image URL was refused")
	}
	if CheckImgUrl("") {
		t.Fatal("an empty URL passed")
	}
}

// A poster host that never answers must not hold a save for ever, and the HEAD and the
// GET fallback share one deadline: two slow requests cost the limit once, not twice.
func TestCheckImgUrlTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	prev := imageCheckTimeout
	imageCheckTimeout = 200 * time.Millisecond
	defer func() { imageCheckTimeout = prev }()

	start := time.Now()
	if CheckImgUrl(srv.URL + "/p.png") {
		t.Fatal("a stalled host passed")
	}
	if d := time.Since(start); d > 400*time.Millisecond {
		t.Fatalf("took %v for a 200 ms limit: the limit applied per request, not in total", d)
	}
}

// A HEAD the server refuses, then a GET that stalls: the fallback gets what is left.
func TestCheckImgUrlFallbackSharesTheDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			time.Sleep(150 * time.Millisecond)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		<-release
	}))
	defer srv.Close()
	defer close(release)
	prev := imageCheckTimeout
	imageCheckTimeout = 200 * time.Millisecond
	defer func() { imageCheckTimeout = prev }()
	start := time.Now()
	if CheckImgUrl(srv.URL + "/p.png") {
		t.Fatal("a stalled GET passed")
	}
	if d := time.Since(start); d > 350*time.Millisecond {
		t.Fatalf("took %v: HEAD and GET each had the full limit", d)
	}
}

// A poster URL that redirects to another host is not followed: the check would be a
// request the caller steers anywhere.
func TestCheckImgUrlRefusesACrossHostRedirect(t *testing.T) {
	other := imageServer(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/p.png", http.StatusFound)
	}))
	defer srv.Close()
	if CheckImgUrl(srv.URL + "/p.png") {
		t.Fatal("a cross-host redirect was followed")
	}
}

func TestCheckImgUrlRefusesOtherSchemes(t *testing.T) {
	for _, l := range []string{"file:///etc/hostname", "ftp://x/p.png", "gopher://x/"} {
		if CheckImgUrl(l) {
			t.Errorf("%q passed", l)
		}
	}
}
