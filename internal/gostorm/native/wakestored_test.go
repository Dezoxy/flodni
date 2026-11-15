package native

import (
	"context"
	"errors"
	"strings"
	"testing"

	sets "flodni/internal/gostorm/settings"
	"flodni/internal/gostorm/torr"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

// Hard rule 3. WakeStored is how warm on add revives a closed torrent. It may only do
// so for a hash the engine still stores: the stored record keeps the trackers and the
// private flag, and a bare magnet for a hash with no record would be added with public
// trackers and the DHT. These tests need no running engine: an engine-less BTServer
// holds no record.

func TestWakeStoredRefusesAHashTheEngineDoesNotStore(t *testing.T) {
	engineFixture(t)
	c := NewNativeClient()
	const hash = "0123456789abcdef0123456789abcdef01234567"
	err := c.WakeStored(context.Background(), hash)
	if !errors.Is(err, ErrNotStored) {
		t.Fatalf("WakeStored(unknown) = %v, want ErrNotStored", err)
	}
	if _, ok := c.activeHashes.Load(hash); ok {
		t.Fatal("a refused wake left the hash marked active")
	}
}

// engineFixture starts an engine-less BTServer on a temporary state database, so a
// wake reaches the real torr.LoadStored and its record lookup.
func engineFixture(t *testing.T) {
	t.Helper()
	oldPath, oldRO := sets.Path, sets.ReadOnly
	sets.Path = t.TempDir()
	sets.InitSets(false, false)
	torr.InitApiHelper(torr.NewBTS())
	t.Cleanup(func() {
		torr.InitApiHelper(nil)
		sets.Path, sets.ReadOnly = oldPath, oldRO
	})
}

// Wake is the one place a torrent is started for a hash, so it is Wake itself that
// refuses a hash with no stored record, whatever link it is given: a link's trackers
// and peers never reach a torrent, and a private one would get the DHT and the public
// trackers from them (hard rule 3).
func TestWakeRefusesAHashTheEngineDoesNotStore(t *testing.T) {
	engineFixture(t)
	const hash = "0123456789abcdef0123456789abcdef01234567"
	links := map[string]string{
		"bare magnet": "magnet:?xt=urn:btih:" + hash,
		"magnet with a tracker": "magnet:?xt=urn:btih:" + hash +
			"&tr=udp%3A%2F%2Fpublic.example%3A1337%2Fannounce&x.pe=203.0.113.9%3A6881",
	}
	for name, link := range links {
		t.Run(name, func(t *testing.T) {
			c := NewNativeClient()
			if err := c.Wake(context.Background(), link, -1); !errors.Is(err, ErrNotStored) {
				t.Fatalf("Wake = %v, want ErrNotStored", err)
			}
			if torr.PeekTorrent(hash) != nil {
				t.Fatal("a refused wake left a torrent behind")
			}
			if _, ok := c.activeHashes.Load(hash); ok {
				t.Fatal("a refused wake left the hash marked active")
			}
		})
	}
}

// A record that holds no metainfo would be added as a bare magnet, its private flag
// unknown: refused as well.
func TestWakeRefusesARecordWithoutMetainfo(t *testing.T) {
	engineFixture(t)
	const hash = "89abcdef0123456789abcdef0123456789abcdef"
	sets.AddTorrent(&sets.TorrentDB{TorrentSpec: &torrent.TorrentSpec{
		InfoHash: metainfo.NewHashFromHex(hash),
		Trackers: [][]string{{"http://seeder.example/announce"}},
	}})
	c := NewNativeClient()
	err := c.Wake(context.Background(), storedMagnet(hash), -1)
	if !errors.Is(err, ErrNotStored) {
		t.Fatalf("Wake = %v, want ErrNotStored", err)
	}
	if err := c.WakeStored(context.Background(), hash); !errors.Is(err, ErrNotStored) {
		t.Fatalf("WakeStored = %v, want ErrNotStored", err)
	}
}

func TestWakeStoredRefusesAMalformedHash(t *testing.T) {
	c := NewNativeClient()
	for _, h := range []string{"", "xyz", strings.Repeat("g", 40), "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"} {
		if err := c.WakeStored(context.Background(), h); !errors.Is(err, ErrNotStored) {
			t.Fatalf("WakeStored(%q) = %v, want ErrNotStored", h, err)
		}
	}
}

func TestStoredMagnetIsBare(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	if got, want := storedMagnet(hash), "magnet:?xt=urn:btih:"+hash; got != want {
		t.Fatalf("storedMagnet = %q, want %q: no tracker, no peer, no name", got, want)
	}
}

// A wake takes a magnet and nothing else. Any other link would be fetched by the link
// parser (an HTTP GET), and a wake must never fetch or add from a link (hard rule 3).
func TestWakeRefusesALinkThatIsNotAMagnet(t *testing.T) {
	engineFixture(t)
	c := NewNativeClient()
	for _, link := range []string{
		"http://127.0.0.1:1/stream?link=x&play",
		"https://example.invalid/a.torrent",
		"0123456789abcdef0123456789abcdef01234567",
	} {
		if err := c.Wake(context.Background(), link, -1); !errors.Is(err, ErrNotStored) {
			t.Errorf("Wake(%q) = %v, want ErrNotStored", link, err)
		}
	}
}

// Before the engine is up a wake is ErrEngineNotReady, which is not ErrNotStored: no
// caller may take it for a verdict on the hash.
func TestWakeBeforeTheEngineIsUpIsNotReadyNotUnstored(t *testing.T) {
	torr.InitApiHelper(nil)
	c := NewNativeClient()
	const hash = "0123456789abcdef0123456789abcdef01234567"
	for name, err := range map[string]error{
		"WakeStored": c.WakeStored(context.Background(), hash),
		"Wake":       c.Wake(context.Background(), storedMagnet(hash), -1),
	} {
		if !errors.Is(err, ErrEngineNotReady) || errors.Is(err, ErrNotStored) {
			t.Errorf("%s = %v, want ErrEngineNotReady and not ErrNotStored", name, err)
		}
	}
	if _, ok := c.activeHashes.Load(hash); ok {
		t.Fatal("a wake before ready left the hash marked active")
	}
}
