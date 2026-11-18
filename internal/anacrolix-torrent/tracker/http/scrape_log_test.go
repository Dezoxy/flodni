package httpTracker

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/types/infohash"
)

// A private tracker's scrape URL carries the passkey, and the stdlib logger bypasses the
// engine logger that redacts. A scrape must never print its URL through it, whether the
// tracker answers or not.
func TestScrapeNeverPrintsTheURLThroughTheStdlibLogger(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"answers", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("d5:filesdee")) }},
		{"fails", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusInternalServerError) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			u, err := url.Parse(srv.URL + "/PASSKEYX/announce?k=PASSKEYX")
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			oldOut, oldFlags := log.Writer(), log.Flags()
			log.SetOutput(&buf)
			defer log.SetOutput(oldOut)
			defer log.SetFlags(oldFlags)
			_, _ = NewClient(u, NewClientOpts{}).Scrape(context.Background(), []infohash.T{{1}})
			if strings.Contains(buf.String(), "PASSKEYX") || strings.Contains(buf.String(), srv.URL) {
				t.Fatalf("the stdlib logger got the scrape URL: %q", buf.String())
			}
		})
	}
}
