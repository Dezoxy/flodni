package torrent

import (
	"context"
	"net/http"
	"net/http/httptest"
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
