package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/anacrolix/torrent"
	"github.com/gin-gonic/gin"

	"flodni/internal/gostorm/torr"
	"flodni/internal/gostorm/torr/state"
	"flodni/internal/gostorm/web/api/utils"
)

const unknownHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const storedHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type streamCalls struct{ add, load, fetch int }

// streamRig swaps the engine seams for fakes and returns a router with GET and HEAD
// /stream. peek knows only storedHash, as a stored (closed) record.
func streamRig(t *testing.T, tokenEnforced bool) (*gin.Engine, *streamCalls) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	calls := &streamCalls{}
	pPeek, pAdd, pLoad, pParse := peekTorrent, streamAdd, loadStored, parseLink
	t.Cleanup(func() { peekTorrent, streamAdd, loadStored, parseLink = pPeek, pAdd, pLoad, pParse })
	peekTorrent = func(h string) *torr.Torrent {
		if h == storedHash {
			return &torr.Torrent{Stat: state.TorrentInDB}
		}
		return nil
	}
	streamAdd = func(*torrent.TorrentSpec, string, string, string, string) (*torr.Torrent, error) {
		calls.add++
		return nil, errors.New("fake add")
	}
	loadStored = func(string) (*torr.Torrent, error) {
		calls.load++
		return nil, torr.ErrNotStored
	}
	// A magnet or hash parses for real; an http(s) or file: link is the fetch.
	parseLink = func(l string) (*torrent.TorrentSpec, error) {
		if u, err := url.Parse(l); err == nil && (u.Scheme == "http" || u.Scheme == "https" || u.Scheme == "file") {
			calls.fetch++
			return nil, errors.New("fake fetch")
		}
		return utils.ParseLink(l)
	}
	r := gin.New()
	if tokenEnforced {
		r.Use(func(c *gin.Context) { c.Set(TokenEnforcedKey, true) })
	}
	r.GET("/stream", stream)
	r.HEAD("/stream", stream)
	return r, calls
}

func doStream(r *gin.Engine, method, link, extra string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/stream?link="+url.QueryEscape(link)+extra, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestStreamGetRefusesAddsWithoutToken(t *testing.T) {
	for _, tc := range []struct{ name, link, extra string }{
		{"unknown magnet", "magnet:?xt=urn:btih:" + unknownHash, "&index=1&play"},
		{"unknown bare hash", unknownHash, "&stat"},
		{"http link", "http://127.0.0.1:1/x.torrent", "&play&index=1"},
		{"https link", "https://example.invalid/x.torrent", "&stat"},
		{"file link", "file:///etc/passwd", "&stat"},
		{"save on stored", "magnet:?xt=urn:btih:" + storedHash, "&save&stat"},
		{"preload on stored", "magnet:?xt=urn:btih:" + storedHash, "&preload&index=1&play"},
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			r, calls := streamRig(t, false)
			rec := doStream(r, method, tc.link, tc.extra)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s: status %d, want 403", method, tc.name, rec.Code)
			}
			if *calls != (streamCalls{}) {
				t.Errorf("%s %s: engine touched: %+v", method, tc.name, *calls)
			}
		}
	}
}

// A stored hash plays as before, but wakes from its record, never from the link.
func TestStreamGetStoredHashPassesWithoutToken(t *testing.T) {
	r, calls := streamRig(t, false)
	rec := doStream(r, http.MethodGet, "magnet:?xt=urn:btih:"+storedHash+"&tr=http://evil.invalid/a", "&index=1&play")
	if rec.Code == http.StatusForbidden {
		t.Fatalf("stored hash refused: %d %s", rec.Code, rec.Body.String())
	}
	if calls.load != 1 || calls.add != 0 {
		t.Errorf("calls %+v, want one LoadStored and no AddTorrent", *calls)
	}
}

// With a token enforced, the handler keeps its behaviour: the add runs.
func TestStreamGetWithTokenKeepsAdd(t *testing.T) {
	r, calls := streamRig(t, true)
	rec := doStream(r, http.MethodGet, "magnet:?xt=urn:btih:"+unknownHash, "&index=1&play")
	if rec.Code == http.StatusForbidden || calls.add != 1 {
		t.Errorf("status %d, calls %+v: want the add to run", rec.Code, *calls)
	}
	r, calls = streamRig(t, true)
	doStream(r, http.MethodGet, "http://x.invalid/a.torrent", "&stat")
	if calls.fetch != 1 {
		t.Errorf("fetch calls %d, want 1 with a token", calls.fetch)
	}
}

// A stored torrent never takes the link's trackers, token or not: it wakes from its record.
func TestStreamGetStoredHashWithTokenWakesFromRecord(t *testing.T) {
	r, calls := streamRig(t, true)
	// A record the engine cannot load fails the request; it is not re-added from the link.
	loadStored = func(string) (*torr.Torrent, error) {
		calls.load++
		return nil, errors.New("fake load failure")
	}
	doStream(r, http.MethodGet, "magnet:?xt=urn:btih:"+storedHash+"&tr=http://evil.invalid/a", "&index=1&play")
	if calls.load != 1 || calls.add != 0 {
		t.Errorf("calls %+v, want one LoadStored and no AddTorrent", *calls)
	}
}
