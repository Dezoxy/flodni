package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The panel page cannot be framed: a page on another origin that frames it could draw a
// button over the panel's own (clickjacking). The Content-Security-Policy is the
// standard; X-Frame-Options covers an older browser.
func TestPanelRefusesToBeFramed(t *testing.T) {
	rec := httptest.NewRecorder()
	handlePanel(rec, httptest.NewRequest(http.MethodGet, "/control", nil))
	if got := rec.Header().Get("Content-Security-Policy"); got != "frame-ancestors 'none'" {
		t.Errorf("Content-Security-Policy = %q, want frame-ancestors 'none'", got)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 || rec.Header().Get("Content-Type") != "text/html" {
		t.Errorf("the page itself changed: %d, %d bytes, %q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Type"))
	}
}
