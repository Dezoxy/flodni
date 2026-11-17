package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	sets "flodni/internal/gostorm/settings"
	"flodni/internal/gostorm/torr"
)

func postSettings(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/settings", strings.NewReader(body))
	settings(c)
	c.Writer.WriteHeaderNow()
	return rec
}

// An engine restart under playback answers 409 and names the CLIENT fields and the
// session count; nothing is stored (the stored settings stay as they were).
func TestSettingsSetRefusedUnderPlaybackNamesFieldsAndSessions(t *testing.T) {
	old := sets.BTsets
	defer func() { sets.BTsets = old }()
	sets.BTsets = sets.DefaultBTSets()
	torr.SetPlaybackProbe(func() int { return 2 })
	defer torr.SetPlaybackProbe(func() int { return 0 })

	next := sets.DefaultBTSets()
	next.DisableDHT = true
	next.PeersListenPort = 51413
	raw, _ := json.Marshal(map[string]any{"action": "set", "sets": next})
	rec := postSettings(t, string(raw))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body)
	}
	var got struct {
		Error    string   `json:"error"`
		Fields   []string `json:"fields"`
		Sessions int      `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Sessions != 2 || !reflect.DeepEqual(got.Fields, []string{"DisableDHT", "PeersListenPort"}) {
		t.Errorf("body = %+v", got)
	}
	for _, want := range []string{"DisableDHT", "PeersListenPort", "2 playing session", "force"} {
		if !strings.Contains(got.Error, want) {
			t.Errorf("message %q lacks %q", got.Error, want)
		}
	}
	if sets.BTsets.DisableDHT {
		t.Error("a refused change was stored")
	}
}

func TestSettingsSetWithoutSetsIs400(t *testing.T) {
	if rec := postSettings(t, `{"action":"set"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}
