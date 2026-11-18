package scrape

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"flodni/internal/tlsverify"
)

// A scrape to a tracker whose certificate fails verification reports host:port to the
// TLS failure log and counter, and nothing of the URL.
func TestHTTPScrapeReportsCertificateFailures(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	var lines []string
	prev := tlsverify.Warn
	tlsverify.Warn = func(s string) { lines = append(lines, s) }
	defer func() { tlsverify.Warn = prev }()
	before := tlsverify.Counts()["unknown_authority"]

	_, _, e := HTTPScrape(context.Background(), newHTTPClient(0, nil), srv.URL+"/PASSKEYX/scrape?k=PASSKEYX", [][20]byte{{1}}, DefaultMaxBody, "ua")
	if e == nil {
		t.Fatal("scrape of a self-signed certificate succeeded")
	}
	if strings.Contains(e.Msg, "PASSKEYX") {
		t.Fatalf("error leaks the URL: %q", e.Msg)
	}
	hp := strings.TrimPrefix(srv.URL, "https://")
	if len(lines) != 1 || lines[0] != "[Tracker] TLS verify failed for "+hp+": unknown authority" {
		t.Fatalf("warnings %q", lines)
	}
	if got := tlsverify.Counts()["unknown_authority"]; got != before+1 {
		t.Fatalf("counter %d, want %d", got, before+1)
	}
}
