package utils

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

func TestLinkHashNeverFetches(t *testing.T) {
	const h = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, l := range []string{"magnet:?xt=urn:btih:" + h + "&tr=http://x.invalid/a", h} {
		if got, ok := LinkHash(l); !ok || got != h {
			t.Errorf("LinkHash(%q) = %q, %v", l, got, ok)
		}
	}
	for _, l := range []string{"http://x.invalid/a.torrent", "https://x.invalid/a", "file:///etc/passwd", "ftp://x/a", "zz", ""} {
		if _, ok := LinkHash(l); ok {
			t.Errorf("LinkHash(%q) ok, want no hash", l)
		}
	}
}

func TestLinkHashRefusesWhatPanicsTheParser(t *testing.T) {
	for _, l := range []string{
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"magnet:?xt=urn:btih:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"magnet:?xt=urn:btmh:1220aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"torrs://" + strings.Repeat("A", maxTorrsToken+1),
	} {
		if _, ok := LinkHash(l); ok {
			t.Errorf("LinkHash(%.40q) ok", l)
		}
		if _, err := ParseLink(l); err == nil && !strings.HasPrefix(l, "torrs") {
			t.Errorf("ParseLink(%.40q) no error", l)
		}
	}
	if h, ok := StrictHash("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"); !ok || h != strings.Repeat("00", 20) {
		t.Errorf("StrictHash base32 = %q %v", h, ok)
	}
}

// A .torrent served over http: just enough for metainfo.Load and UnmarshalInfo.
func tinyTorrent(t *testing.T) []byte {
	t.Helper()
	info := metainfo.Info{Name: "x", Length: 16384, PieceLength: 16384, Pieces: make([]byte, 20)}
	ib, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	b, err := bencode.Marshal(metainfo.MetaInfo{InfoBytes: ib})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// file: is not an API link: the engine never reads a path a caller names.
func TestParseLinkRefusesFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.torrent")
	if err := os.WriteFile(p, tinyTorrent(t), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{"file://" + p, "FILE://" + p, "file:" + p} {
		if spec, err := ParseLink(l); err == nil {
			t.Errorf("ParseLink(%q) read the file: %+v", l, spec)
		}
	}
}

func TestParseLinkHTTPFetchesATorrent(t *testing.T) {
	body := tinyTorrent(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()
	spec, err := ParseLink(srv.URL + "/a.torrent")
	if err != nil || spec.DisplayName != "x" || len(spec.InfoBytes) == 0 {
		t.Fatalf("spec %+v, err %v", spec, err)
	}
}

func TestParseLinkHTTPRefusesAnOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A well-formed bencoded dict that never ends within the cap: a long string value.
		_, _ = w.Write([]byte("d1:a" + strconv.Itoa(maxHTTPBody+1024) + ":"))
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxHTTPBody+1024))
		_, _ = w.Write([]byte("e"))
	}))
	defer srv.Close()
	_, err := ParseLink(srv.URL + "/big.torrent")
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v, want a too-large refusal", err)
	}
}

func TestParseLinkHTTPRedirects(t *testing.T) {
	body := tinyTorrent(t)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the other host was contacted")
		_, _ = w.Write(body)
	}))
	defer other.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/cross", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/a.torrent", http.StatusFound)
	})
	mux.HandleFunc("/same", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/final", http.StatusFound) })
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) })
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/loop", http.StatusFound) })
	mux.HandleFunc("/hop", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		if n == 0 {
			_, _ = w.Write(body)
			return
		}
		http.Redirect(w, r, "/hop?n="+strconv.Itoa(n-1), http.StatusFound)
	})
	mux.HandleFunc("/magnet", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "magnet:?xt=urn:btih:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if _, err := ParseLink(srv.URL + "/cross"); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("cross-host redirect: err = %v, want a refusal", err)
	}
	if _, err := ParseLink(srv.URL + "/same"); err != nil {
		t.Errorf("same-host redirect: %v", err)
	}
	if _, err := ParseLink(srv.URL + "/hop?n=3"); err != nil {
		t.Errorf("3 same-host redirects: %v", err)
	}
	if _, err := ParseLink(srv.URL + "/hop?n=4"); err == nil {
		t.Errorf("4 redirects: want a refusal")
	}
	if _, err := ParseLink(srv.URL + "/loop"); err == nil {
		t.Errorf("redirect loop: want a refusal")
	}
	// A redirect to a magnet is a link, not a fetch: it keeps working.
	if spec, err := ParseLink(srv.URL + "/magnet"); err != nil || spec.InfoHash.HexString() != strings.Repeat("a", 40) {
		t.Errorf("redirect to a magnet: %+v, %v", spec, err)
	}
}

func TestParseLinkHTTPTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	prev := httpFetchTimeout
	httpFetchTimeout = 200 * time.Millisecond
	defer func() { httpFetchTimeout = prev }()
	start := time.Now()
	if _, err := ParseLink(srv.URL + "/slow"); err == nil {
		t.Fatal("a stalled server was waited out")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
}
