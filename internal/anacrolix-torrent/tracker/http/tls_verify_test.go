package httpTracker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// A tracker's certificate is verified unless the caller opts out: an on-path attacker with a
// forged certificate must not be able to answer an announce or read a passkey from its URL.
func tlsTracker(t *testing.T) *url.URL {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("d8:intervali1800ee"))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestAnnounceVerifiesTheCertificateByDefault(t *testing.T) {
	u := tlsTracker(t)
	if _, err := NewClient(u, NewClientOpts{}).Announce(context.Background(), AnnounceRequest{}, AnnounceOpt{}); err == nil {
		t.Fatal("announce to a self-signed certificate succeeded with verification on")
	}
}

func TestAnnounceSkipsVerificationOnlyWhenAsked(t *testing.T) {
	u := tlsTracker(t)
	res, err := NewClient(u, NewClientOpts{InsecureSkipVerify: true}).Announce(context.Background(), AnnounceRequest{}, AnnounceOpt{})
	if err != nil {
		t.Fatalf("announce with InsecureSkipVerify: %v", err)
	}
	if res.Interval != 1800 {
		t.Fatalf("interval %d, want 1800", res.Interval)
	}
}
