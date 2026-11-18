package torrent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/tracker"
)

// The tracker scraper announces with the client's tracker TLS setting: a self-signed
// certificate fails the announce by default and passes with TrackerInsecureSkipVerify.
func TestScraperAnnounceHonoursTrackerTLSConfig(t *testing.T) {
	for _, tc := range []struct {
		name     string
		insecure bool
		wantErr  bool
	}{
		{"default verifies", false, true},
		{"opt out skips", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte("d8:intervali1800e5:peers0:e"))
			}))
			defer srv.Close()

			cfg := NewDefaultClientConfig()
			cfg.DataDir = t.TempDir()
			cfg.ListenHost = func(string) string { return "127.0.0.1" }
			cfg.ListenPort = 0
			cfg.NoDefaultPortForwarding = true
			cfg.DisableIPv6 = true
			cfg.NoDHT = true
			cfg.TrackerInsecureSkipVerify = tc.insecure
			cl, err := NewClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer cl.Close()

			info := metainfo.Info{Name: "tls", Length: 16384, PieceLength: 16384, Pieces: make([]byte, 20)}
			b, err := bencode.Marshal(info)
			if err != nil {
				t.Fatal(err)
			}
			tor, _, err := cl.AddTorrentSpec(&TorrentSpec{
				InfoHash:  metainfo.HashBytes(b),
				InfoBytes: b,
				Trackers:  [][]string{{srv.URL + "/announce"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			res := scraperOf(t, tor).announce(context.Background(), tracker.None)
			if (res.Err != nil) != tc.wantErr {
				t.Fatalf("announce error = %v, want error: %v", res.Err, tc.wantErr)
			}
		})
	}
}

// A failed announce is handed to OnTrackerAnnounceError with the tracker's host:port and the
// error, never its URL, so the daemon can log and count a certificate failure.
func TestScraperAnnounceReportsFailureToTheHook(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("d8:intervali1800e5:peers0:e"))
	}))
	defer srv.Close()

	// The hook runs on the test's direct announce and on the client's own background scraper
	// (started by AddTorrentSpec), so the captured values need a lock.
	var mu sync.Mutex
	var hostport string
	var hookErr error
	cfg := NewDefaultClientConfig()
	cfg.DataDir = t.TempDir()
	cfg.ListenHost = func(string) string { return "127.0.0.1" }
	cfg.ListenPort = 0
	cfg.NoDefaultPortForwarding = true
	cfg.DisableIPv6 = true
	cfg.NoDHT = true
	cfg.OnTrackerAnnounceError = func(hp string, err error) {
		mu.Lock()
		defer mu.Unlock()
		hostport, hookErr = hp, err
	}
	cl, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	info := metainfo.Info{Name: "tls", Length: 16384, PieceLength: 16384, Pieces: make([]byte, 20)}
	b, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	tor, _, err := cl.AddTorrentSpec(&TorrentSpec{InfoHash: metainfo.HashBytes(b), InfoBytes: b, Trackers: [][]string{{srv.URL + "/announce/PASSKEYX?k=PASSKEYX"}}})
	if err != nil {
		t.Fatal(err)
	}
	res := scraperOf(t, tor).announce(context.Background(), tracker.None)
	mu.Lock()
	defer mu.Unlock()
	if res.Err == nil || hookErr == nil {
		t.Fatalf("announce error %v, hook error %v: want both", res.Err, hookErr)
	}
	if want := strings.TrimPrefix(srv.URL, "https://"); hostport != want {
		t.Fatalf("hook host:port %q, want %q", hostport, want)
	}
}
