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

// A poster host that never answers must not hold a save for ever.
func TestCheckImgUrlTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	prev := imageCheckTimeout
	imageCheckTimeout = 200 * time.Millisecond
	defer func() { imageCheckTimeout = prev }()

	done := make(chan bool, 1)
	go func() { done <- CheckImgUrl(srv.URL + "/p.png") }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("a stalled host passed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the poster check did not time out")
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
