package api

import (
	"bytes"
	"encoding/json"
	"io"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// A private tracker's announce URL is the user's account. The add path logs the link
// and the errors built from it; none of those lines may carry a path passkey, a query
// value or userinfo.
func TestAddTorrentLogsHoldNoTrackerCredentials(t *testing.T) {
	const key = "abcdef0123456789abcdef0123456789"
	hash := "0123456789abcdef0123456789abcdef01234567"
	// 127.0.0.1:1 refuses at once; the http client error embeds the whole URL.
	httpLink := "http://u:" + key + "@127.0.0.1:1/" + key + "/announce?passkey=" + key
	links := map[string]string{
		"path magnet":  "magnet:?xt=urn:btih:" + hash + "&tr=" + url.QueryEscape("https://tracker.example/announce/"+key),
		"path magnet2": "magnet:?xt=urn:btih:" + hash + "&tr=" + url.QueryEscape("https://tracker.example/"+key+"/announce"),
		"query magnet": "magnet:?xt=urn:btih:" + hash + "&tr=" + url.QueryEscape("udp://tracker.example:6969/announce?passkey="+key),
		"raw magnet":   "magnet:?xt=urn:btih:" + hash + "&tr=https://tracker.example/" + key + "/announce",
		"bad magnet":   "magnet:?xt=urn:btih:zz&tr=" + url.QueryEscape("https://tracker.example/"+key+"/announce"),
		"http link":    httpLink,
		"userinfo":     "http://alice:" + key + "@127.0.0.1:1/dl.torrent",
		"torrs":        "torrs://" + key + key,
	}

	var buf bytes.Buffer
	prevW, prevF := stdlog.Writer(), stdlog.Flags()
	stdlog.SetOutput(&buf)
	t.Cleanup(func() { stdlog.SetOutput(prevW); stdlog.SetFlags(prevF) })

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(gin.RecoveryWithWriter(io.Discard)) // valid magnets reach the uninitialised store and panic
	r.POST("/torrents", torrents)
	for name, link := range links {
		body, _ := json.Marshal(map[string]string{"action": "add", "link": link})
		buf.Reset()
		req := httptest.NewRequest(http.MethodPost, "/torrents", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(httptest.NewRecorder(), req)

		out := buf.String()
		if !strings.Contains(out, "add torrent") {
			t.Errorf("%s: the add line is missing: %q", name, out)
		}
		for _, leak := range []string{key, "passkey", "alice"} {
			if strings.Contains(out, leak) {
				t.Errorf("%s: log holds %q:\n%s", name, leak, out)
			}
		}
	}
}
