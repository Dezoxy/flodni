package httpTracker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// trackerCap is the 4 MiB cap on a tracker response body.
const trackerCap = 4 << 20

// A tracker response is a few KB. A broken or hostile tracker must not make us buffer more
// than the cap (adapted from upstream Tiramisu fd976eb).
func TestMalformedTrackerResponseBodyIsCapped(t *testing.T) {
	// A valid response followed by padding: the decoder tolerates trailing bytes, so only
	// the size cap can refuse it.
	body := "d8:intervali60ee" + strings.Repeat("\x00", trackerCap+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewClient(u, NewClientOpts{}).Announce(context.Background(), AnnounceRequest{}, AnnounceOpt{})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("an oversized tracker response was accepted: err=%v", err)
	}
}

func TestMalformedTrackerResponseUnderTheCapIsRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("d8:intervali60ee"))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	resp, err := NewClient(u, NewClientOpts{}).Announce(context.Background(), AnnounceRequest{}, AnnounceOpt{})
	if err != nil || resp.Interval != 60 {
		t.Fatalf("a small response: interval=%d err=%v", resp.Interval, err)
	}
}
