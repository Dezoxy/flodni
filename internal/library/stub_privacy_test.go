package library

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const passkeyTracker = "http://seeder.example:6969/announce?passkey=0123456789abcdef0123456789abcdef"

var reLongHex = regexp.MustCompile(`[0-9a-f]{20,}`)

// trackerMagnet is a request magnet with the dn, tr and xs parameters a private
// tracker's magnet carries.
func trackerMagnet(hash string) string {
	return BuildMagnet(hash, "Some Title", []string{passkeyTracker}) + "&xs=" + passkeyTracker
}

// assertCleanStub fails when the stub at path says anything but the bare info hash:
// no tracker, no name, no passkey-looking run of hex besides the hash itself.
func assertCleanStub(t *testing.T, path, hash string) {
	t.Helper()
	raw := readFile(t, path)
	var got struct {
		Magnet string `json:"magnet"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if want := "magnet:?xt=urn:btih:" + hash; got.Magnet != want {
		t.Errorf("%s: magnet = %q, want %q", filepath.Base(path), got.Magnet, want)
	}
	for _, bad := range []string{"tr=", "tr%3D", "dn=", "xs=", "passkey", "announce", "seeder"} {
		if strings.Contains(raw, bad) {
			t.Errorf("%s: stub holds %q: %s", filepath.Base(path), bad, raw)
		}
	}
	if m := reLongHex.FindString(strings.ReplaceAll(raw, hash, "")); m != "" {
		t.Errorf("%s: stub holds a passkey-looking string %q", filepath.Base(path), m)
	}
}

func TestStubMagnetIsTheBareHash(t *testing.T) {
	got, ok := StubMagnet(strings.ToUpper(charHash))
	if !ok || got != "magnet:?xt=urn:btih:"+charHash {
		t.Fatalf("StubMagnet = %q, %v", got, ok)
	}
	for _, bad := range []string{"", "m", charHash[:39], charHash + "0", strings.Repeat("g", 40)} {
		if m, ok := StubMagnet(bad); ok {
			t.Errorf("StubMagnet(%q) = %q, want it refused", bad, m)
		}
	}
	if err := WriteStub(filepath.Join(t.TempDir(), "x.mkv"), "http://e/stream", 1, "not-a-hash", ""); err == nil {
		t.Error("WriteStub accepted a value that is not an info hash")
	}
}

// Every way a stub is written drops the request's trackers: a magnet request for a
// movie and an episode, a .torrent upload, a season pack and a replacement.
func TestEveryAddPathWritesATrackerlessStub(t *testing.T) {
	t.Run("movie by magnet", func(t *testing.T) {
		e := newCharEnv(t, FileStat{ID: 1, Path: "Movie.2020.mkv", Length: 1 << 20})
		resp, err := e.add(AddRequest{Type: "movie", Title: "Movie", Year: 2020, Hash: charHash, Magnet: trackerMagnet(charHash)})
		if err != nil || len(resp.Files) != 1 {
			t.Fatalf("add: %v %+v", err, resp)
		}
		assertCleanStub(t, resp.Files[0].Path, charHash)
	})
	t.Run("episode by magnet", func(t *testing.T) {
		e := newCharEnv(t, FileStat{ID: 1, Path: "Show.S01E01.mkv", Length: 1 << 20})
		resp, err := e.add(AddRequest{Type: "tv", Title: "Show", Season: 1, Episode: 1, Hash: charHash, Magnet: trackerMagnet(charHash)})
		if err != nil || len(resp.Files) != 1 {
			t.Fatalf("add: %v %+v", err, resp)
		}
		assertCleanStub(t, resp.Files[0].Path, charHash)
	})
	t.Run("movie by .torrent upload", func(t *testing.T) {
		file := torrentFile(t, true)
		src, err := ParseTorrentFile(file)
		if err != nil {
			t.Fatal(err)
		}
		e := newCharEnv(t, FileStat{ID: 1, Path: "Movie.2020.mkv", Length: 1 << 20})
		resp, err := e.add(AddRequest{Type: "movie", Title: "Movie", Year: 2020, Hash: src.Hash, TorrentFile: file})
		if err != nil || len(resp.Files) != 1 {
			t.Fatalf("add: %v %+v", err, resp)
		}
		assertCleanStub(t, resp.Files[0].Path, src.Hash)
	})
	t.Run("season pack", func(t *testing.T) {
		e := newCharEnv(t, seasonPack...)
		resp, err := e.add(AddRequest{Type: "tv", Title: "Show", Hash: charPackHash, Season: 1, Magnet: trackerMagnet(charPackHash)})
		if err != nil || len(resp.Files) == 0 {
			t.Fatalf("add: %v %+v", err, resp)
		}
		for _, f := range resp.Files {
			assertCleanStub(t, f.Path, charPackHash)
		}
	})
	t.Run("replacement", func(t *testing.T) {
		e := newCharEnv(t,
			FileStat{ID: 1, Path: "Show.S01/Show.S01E01.mkv", Length: 1 << 20},
			FileStat{ID: 2, Path: "Show.S01/Show.S01E02.mkv", Length: 1 << 20},
		)
		old := scatter(t, e)
		resp, err := e.add(AddRequest{Type: "tv", Title: "Show", Hash: charThirdHash, Season: 1, Replace: ReplaceAlways, Magnet: trackerMagnet(charThirdHash)})
		if err != nil || len(resp.Files) != 2 {
			t.Fatalf("add: %v %+v", err, resp)
		}
		for _, p := range old {
			if _, err := os.Stat(p); err == nil {
				t.Errorf("replaced stub %s still there", p)
			}
		}
		for _, f := range resp.Files {
			assertCleanStub(t, f.Path, charThirdHash)
		}
	})
}
