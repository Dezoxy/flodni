package httpTracker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// The announce response keeps the tracker's "min interval" next to its interval, and a tracker
// that sends none leaves it 0.
func TestAnnounceKeepsTheMinInterval(t *testing.T) {
	for _, tc := range []struct {
		name, body    string
		interval, min int32
	}{
		{"both", "d8:intervali1800e12:min intervali300ee", 1800, 300},
		{"none", "d8:intervali1800ee", 1800, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			u, _ := url.Parse(srv.URL)
			res, err := NewClient(u, NewClientOpts{}).Announce(context.Background(), AnnounceRequest{}, AnnounceOpt{})
			if err != nil {
				t.Fatal(err)
			}
			if res.Interval != tc.interval || res.MinInterval != tc.min {
				t.Errorf("interval=%d min interval=%d, want %d and %d", res.Interval, res.MinInterval, tc.interval, tc.min)
			}
		})
	}
}
