package tracker

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Announce hands its InsecureSkipVerify to the HTTP client: off by default, so a forged
// certificate fails the announce, and on only when the caller sets it.
func TestAnnounceDoVerifiesTLSUnlessAsked(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("d8:intervali1800ee"))
	}))
	defer srv.Close()

	if _, err := (Announce{TrackerUrl: srv.URL}).Do(); err == nil {
		t.Fatal("announce to a self-signed certificate succeeded with verification on")
	}
	res, err := (Announce{TrackerUrl: srv.URL, InsecureSkipVerify: true}).Do()
	if err != nil {
		t.Fatalf("announce with InsecureSkipVerify: %v", err)
	}
	if res.Interval != 1800 {
		t.Fatalf("interval %d, want 1800", res.Interval)
	}
}
