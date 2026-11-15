package daemon

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// A GET carries the payload in the query string, which a page can send with an
// <img> tag and no Origin header, so the webhook takes POST only.
func TestWebhookRefusesGet(t *testing.T) {
	_, key := playedEnv(t)
	starts := playbackStarts.Load()
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodPut} {
		req := httptest.NewRequest(m, "/plex/webhook?payload="+url.QueryEscape(evPayload("media.play")), nil)
		rec := httptest.NewRecorder()
		handlePlexWebhook(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status %d, want 405", m, rec.Code)
		}
		if rec.Header().Get("Allow") != http.MethodPost {
			t.Errorf("%s: Allow = %q, want POST", m, rec.Header().Get("Allow"))
		}
	}
	if fileStates.IsPlayed(key) || playbackStarts.Load() != starts {
		t.Fatal("a refused request was matched or counted")
	}
}
