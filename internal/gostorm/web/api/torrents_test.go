package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// "wipe" used to remove every stored torrent. It is gone, so it falls through
// to the same refusal as any unknown action (400) and touches nothing. The
// torrent store is not initialised here: the old handler would have reached it.
func TestTorrentsRefusesWipe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.POST("/torrents", torrents)

	for _, action := range []string{"wipe", "nonsense"} {
		req := httptest.NewRequest(http.MethodPost, "/torrents", strings.NewReader(`{"action":"`+action+`"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("action %q: status = %d, want 400", action, rec.Code)
		}
	}
}

// /shutdown stops the process, so it must not answer GET: a page can send a
// GET with an <img> tag and no Origin header. POST goes through the Origin gate.
func TestShutdownIsPostOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	SetupRoute(r)
	post := map[string]bool{}
	for _, rt := range r.Routes() {
		if rt.Path == "/shutdown" || rt.Path == "/shutdown/*reason" {
			if rt.Method != http.MethodPost {
				t.Errorf("%s %s registered, want POST only", rt.Method, rt.Path)
			}
			post[rt.Path] = true
		}
	}
	if len(post) != 2 {
		t.Errorf("POST routes registered: %v, want /shutdown and /shutdown/*reason", post)
	}
	req := httptest.NewRequest(http.MethodGet, "/shutdown", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /shutdown: status %d, want 404 or 405", rec.Code)
	}
}
