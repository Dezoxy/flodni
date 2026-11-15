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
