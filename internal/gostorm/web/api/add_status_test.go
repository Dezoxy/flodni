package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"flodni/internal/gostorm/torr"

	"github.com/anacrolix/torrent"
	"github.com/gin-gonic/gin"
)

// An add that asked for a record answers 2xx only when the record exists: a metadata
// timeout and a failed save are non-2xx with a message.
func TestAddAnswersOnlyAfterTheSave(t *testing.T) {
	gin.SetMode(gin.TestMode)
	prevAdd, prevSave := addTorrentToEngine, saveAdded
	t.Cleanup(func() { addTorrentToEngine, saveAdded = prevAdd, prevSave })
	addTorrentToEngine = func(*torrent.TorrentSpec, string, string, string, string) (*torr.Torrent, error) {
		return &torr.Torrent{}, nil
	}
	const hash = "0123456789abcdef0123456789abcdef01234567"
	for _, c := range []struct {
		name string
		err  error
		want int
	}{
		{"saved", nil, http.StatusOK},
		{"no metadata", torr.ErrNoMetadata, http.StatusGatewayTimeout},
		{"not saved", torr.ErrNotSaved, http.StatusInternalServerError},
	} {
		saveAdded = func(*torr.Torrent, string) error { return c.err }
		r := gin.New()
		r.POST("/torrents", torrents)
		body, _ := json.Marshal(map[string]interface{}{"action": "add", "link": "magnet:?xt=urn:btih:" + hash, "save_to_db": true})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/torrents", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d (%s)", c.name, rec.Code, c.want, rec.Body)
		}
		if c.err != nil && !strings.Contains(rec.Body.String(), c.err.Error()) {
			t.Errorf("%s: body %q does not carry the reason", c.name, rec.Body)
		}
	}
}
