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

// A malformed "min interval" (the key is unofficial) never fails the announce: the interval
// stays and the min interval reads as 0, or as negative, which the engine never forces on.
func TestAnnounceMinIntervalMalformedKeepsTheAnnounce(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		min        int32
		wantNeg    bool
	}{
		{"string", "d8:intervali1800e12:min interval3:abce", 0, false},
		{"list", "d8:intervali1800e12:min intervalli1ei2eee", 0, false},
		{"beyond int32", "d8:intervali1800e12:min intervali99999999999ee", 0, false},
		{"negative", "d8:intervali1800e12:min intervali-5ee", -5, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			u, _ := url.Parse(srv.URL)
			res, err := NewClient(u, NewClientOpts{}).Announce(context.Background(), AnnounceRequest{}, AnnounceOpt{})
			if err != nil {
				t.Fatalf("a malformed min interval failed the announce: %v", err)
			}
			if res.Interval != 1800 {
				t.Errorf("interval = %d, want 1800", res.Interval)
			}
			if res.MinInterval != tc.min || (res.MinInterval < 0) != tc.wantNeg {
				t.Errorf("min interval = %d, want %d", res.MinInterval, tc.min)
			}
		})
	}
}
