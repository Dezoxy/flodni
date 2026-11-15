package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"

	"flodni/internal/gostorm/torr"
	"flodni/internal/gostorm/torr/state"
)

func playRig(t *testing.T) (*gin.Engine, *streamCalls) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	calls := &streamCalls{}
	pGet, pLoad := playGet, loadStored
	t.Cleanup(func() { playGet, loadStored = pGet, pLoad })
	playGet = func(h string) *torr.Torrent {
		if h == storedHash {
			return &torr.Torrent{Stat: state.TorrentInDB}
		}
		return nil
	}
	loadStored = func(string) (*torr.Torrent, error) {
		calls.load++
		return nil, torr.ErrNotStored
	}
	r := gin.New()
	r.GET("/play/:hash/:id", play)
	r.HEAD("/play/:hash/:id", play)
	return r, calls
}

// :hash is a hash and nothing else: a URL or a path is refused before any fetch or
// file read, and the engine is not asked.
func TestPlayRefusesNonHash(t *testing.T) {
	for _, v := range []string{"http:x.invalid", "https:x.invalid", "file:etc", "zz", "nothash", "magnet:?xt=urn:btih:" + storedHash, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "torrs:" + storedHash} {
		r, calls := playRig(t)
		req := httptest.NewRequest(http.MethodGet, "/play/"+url.PathEscape(v)+"/1", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400", v, rec.Code)
		}
		if calls.load != 0 {
			t.Errorf("%q: engine asked", v)
		}
	}
}

// A stored hash (40 hex, as the library's URLs carry) still wakes from its record.
func TestPlayStoredHashStillPlays(t *testing.T) {
	r, calls := playRig(t)
	req := httptest.NewRequest(http.MethodGet, "/play/"+storedHash+"/1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code == http.StatusBadRequest || calls.load != 1 {
		t.Errorf("status %d, load calls %d: want past the parser and one LoadStored", rec.Code, calls.load)
	}
}

// 32-char base32 is accepted as a hash, and a stored one still wakes.
func TestPlayAcceptsBase32Hash(t *testing.T) {
	r, calls := playRig(t)
	playGet = func(string) *torr.Torrent { return &torr.Torrent{Stat: state.TorrentInDB} }
	req := httptest.NewRequest(http.MethodGet, "/play/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA/1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code == http.StatusBadRequest || calls.load != 1 {
		t.Errorf("status %d, load calls %d", rec.Code, calls.load)
	}
}

func TestPlayUnknownHashIs404(t *testing.T) {
	r, calls := playRig(t)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/play/"+unknownHash+"/1", nil))
	if rec.Code != http.StatusNotFound || calls.load != 0 {
		t.Errorf("status %d, load %d, want 404 and none", rec.Code, calls.load)
	}
}
