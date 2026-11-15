package library

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"flodni/internal/gostorm/torr/utils"
	"flodni/internal/metadb"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

const privacyTracker = "http://seeder.example:6969/announce"

// torrentFile builds a one-file .torrent announcing on privacyTracker.
func torrentFile(t *testing.T, private bool) []byte {
	t.Helper()
	info := metainfo.Info{
		Name:        "release",
		Length:      16384,
		PieceLength: 16384,
		Pieces:      make([]byte, 20),
	}
	if private {
		info.Private = &private
	}
	ib, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	b, err := bencode.Marshal(metainfo.MetaInfo{InfoBytes: ib, Announce: privacyTracker})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func hasPublicTracker(magnet string) bool {
	for _, tr := range DefaultTrackers() {
		if strings.Contains(magnet, escapeTracker(tr)) {
			return true
		}
	}
	return false
}

func escapeTracker(tr string) string {
	return BuildMagnet("", "", []string{tr})[len("magnet:?xt=urn:btih:&tr="):]
}

// A private torrent keeps only its own trackers; a public one, or one named by hash
// alone, is not given the public fallback list for a hash that may turn out private.
func TestReleaseMagnetKeepsPrivateTorrentsPrivate(t *testing.T) {
	cases := []struct {
		name       string
		file       []byte
		wantPublic bool
		wantOwn    bool
	}{
		{"private torrent file", torrentFile(t, true), false, true},
		{"public torrent file", torrentFile(t, false), true, true},
		{"hash only, privacy unknown", nil, false, false},
	}
	for _, c := range cases {
		hash := strings.Repeat("ab", 20)
		file, err := releaseFile(c.file, hash)
		if len(c.file) > 0 {
			src, perr := ParseTorrentFile(c.file)
			if perr != nil {
				t.Fatal(perr)
			}
			hash = src.Hash
			file, err = releaseFile(c.file, hash)
		}
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		magnet := releaseMagnet(hash, "Title", file)
		if got := hasPublicTracker(magnet); got != c.wantPublic {
			t.Errorf("%s: public trackers in magnet = %v, want %v: %s", c.name, got, c.wantPublic, magnet)
		}
		if got := strings.Contains(magnet, "seeder.example"); got != c.wantOwn {
			t.Errorf("%s: own tracker in magnet = %v, want %v", c.name, got, c.wantOwn)
		}
	}
}

// fakeEngine records the magnets the library hands the engine.
type fakeEngine struct{ magnets []string }

func (f *fakeEngine) AddTorrent(_ context.Context, magnet, _ string) (string, error) {
	f.magnets = append(f.magnets, magnet)
	return HashFromMagnet(magnet), nil
}
func (f *fakeEngine) GetTorrentInfo(context.Context, string, int) (*TorrentStats, error) {
	return nil, context.DeadlineExceeded
}
func (f *fakeEngine) RemoveTorrent(context.Context, string) error            { return nil }
func (f *fakeEngine) ReadFileHead(context.Context, string, int, int64) error { return nil }
func (f *fakeEngine) ListTorrents(context.Context) ([]TorrentStats, error) {
	return nil, nil
}

func TestInspectDoesNotGivePrivateTorrentsPublicTrackers(t *testing.T) {
	private := torrentFile(t, true)
	src, err := ParseTorrentFile(private)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		hash       string
		file       []byte
		wantPublic bool
	}{
		{"private torrent file", src.Hash, private, false},
		{"hash only", strings.Repeat("cd", 20), nil, false},
		{"public torrent file", "", torrentFile(t, false), true},
	}
	for _, c := range cases {
		fe := &fakeEngine{}
		m := New(Config{GoStorm: fe})
		hash := c.hash
		if hash == "" {
			s, _ := ParseTorrentFile(c.file)
			hash = s.Hash
		}
		_, _ = m.Inspect(context.Background(), InspectRequest{Hash: hash, Title: "T", TorrentFile: c.file, MetadataWait: 1})
		if len(fe.magnets) != 1 {
			t.Fatalf("%s: engine got %d adds, want 1", c.name, len(fe.magnets))
		}
		if got := hasPublicTracker(fe.magnets[0]); got != c.wantPublic {
			t.Errorf("%s: public trackers in magnet = %v, want %v: %s", c.name, got, c.wantPublic, fe.magnets[0])
		}
	}
}

// The wake path strips GoStorm's public lists from a private torrent; the library's
// fallback list must be among them or its trackers would survive a wake.
func TestDefaultTrackersAreInGoStormBuiltinList(t *testing.T) {
	builtin := map[string]bool{}
	for _, tr := range utils.BuiltinTrackers() {
		builtin[tr] = true
	}
	for _, tr := range DefaultTrackers() {
		if !builtin[tr] {
			t.Errorf("%s is not in gostorm's built-in tracker list", tr)
		}
	}
}

// recEngine records every call the library makes into the engine, so a test can say a
// path made none. It also takes a .torrent upload, which is engine contact too.
type recEngine struct {
	replaceEngine
	cmu   sync.Mutex
	calls []string
}

func (r *recEngine) rec(name string) {
	r.cmu.Lock()
	r.calls = append(r.calls, name)
	r.cmu.Unlock()
}

func (r *recEngine) seen() []string {
	r.cmu.Lock()
	defer r.cmu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *recEngine) AddTorrent(ctx context.Context, magnet, title string) (string, error) {
	r.rec("AddTorrent")
	return r.replaceEngine.AddTorrent(ctx, magnet, title)
}
func (r *recEngine) GetTorrentInfo(ctx context.Context, hash string, wait int) (*TorrentStats, error) {
	r.rec("GetTorrentInfo")
	return r.replaceEngine.GetTorrentInfo(ctx, hash, wait)
}
func (r *recEngine) RemoveTorrent(ctx context.Context, hash string) error {
	r.rec("RemoveTorrent")
	return r.replaceEngine.RemoveTorrent(ctx, hash)
}
func (r *recEngine) ListTorrents(ctx context.Context) ([]TorrentStats, error) {
	r.rec("ListTorrents")
	return r.replaceEngine.ListTorrents(ctx)
}
func (r *recEngine) ReadFileHead(ctx context.Context, hash string, id int, n int64) error {
	r.rec("ReadFileHead")
	return r.replaceEngine.ReadFileHead(ctx, hash, id, n)
}
func (r *recEngine) UploadTorrent(context.Context, []byte, string) (string, error) {
	r.rec("UploadTorrent")
	return "", nil
}

// An add the replace policy refuses never reaches the engine: no upload, no AddTorrent,
// no metadata wait, no read. A private torrent is therefore not announced anywhere,
// whether the caller sent its .torrent or a magnet (hard rule 3).
func TestRefusedAddMakesNoEngineCall(t *testing.T) {
	private := torrentFile(t, true)
	src, err := ParseTorrentFile(private)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		policy string
		score  int
		file   []byte
		magnet string
	}{
		{"never, private .torrent", "never", 9, private, ""},
		{"never, magnet", "never", 9, nil, "magnet:?xt=urn:btih:" + src.Hash + "&tr=http%3A%2F%2Fseeder.example%3A6969%2Fannounce"},
		{"if-better with no score, private .torrent", "if-better", 0, private, ""},
		{"if-better with a lower score, magnet", "if-better", 2, nil, "magnet:?xt=urn:btih:" + src.Hash},
	} {
		t.Run(c.name, func(t *testing.T) {
			eng := &recEngine{}
			eng.files = []FileStat{{ID: 1, Path: "Show.S01E01.mkv", Length: 1 << 20}}
			reg := &memRegistry{rows: map[string]metadb.EpisodeEntry{}}
			tv := t.TempDir()
			m := New(Config{TVDir: tv, GoStorm: eng, Registry: reg, GoStormURL: "http://engine"})
			oldStub := filepath.Join(tv, "Show", "Season.01", EpisodeFilename("Show", 1, 1, oldHash[:8]))
			if err := WriteStub(oldStub, m.streamURL(oldHash, 1), 1000, oldHash, ""); err != nil {
				t.Fatal(err)
			}
			key := EpisodeKey("Show", 1, 1)
			reg.rows[key] = metadb.EpisodeEntry{EpisodeKey: key, Hash: oldHash, FilePath: oldStub, QualityScore: 5}

			resp, err := m.Add(context.Background(), AddRequest{
				Type: "tv", Title: "Show", Season: 1, Episode: 1, Hash: src.Hash, Magnet: c.magnet,
				TorrentFile: c.file, Replace: c.policy, QualityScore: c.score, MetadataWait: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			if resp.Decision != DecisionKept || len(resp.Files) != 0 {
				t.Fatalf("response = %+v, want kept with no files", resp)
			}
			if calls := eng.seen(); len(calls) != 0 {
				t.Fatalf("a refused add called the engine: %v", calls)
			}
		})
	}
}

// A dry run asks the engine nothing: no upload, no AddTorrent, no metadata wait, no
// read. Asking what an add would decide must not announce a private torrent, whether
// the caller sent its .torrent or a magnet (hard rule 3).
func TestDryRunMakesNoEngineCall(t *testing.T) {
	private := torrentFile(t, true)
	src, err := ParseTorrentFile(private)
	if err != nil {
		t.Fatal(err)
	}
	magnet := "magnet:?xt=urn:btih:" + src.Hash + "&tr=http%3A%2F%2Fseeder.example%3A6969%2Fannounce"
	for _, c := range []struct {
		name   string
		typ    string
		policy string
		file   []byte
		magnet string
		prior  bool
	}{
		{"private .torrent, tv, nothing filed", "tv", "", private, "", false},
		{"magnet, tv, nothing filed", "tv", "", nil, magnet, false},
		{"private .torrent, tv, would replace", "tv", "always", private, "", true},
		{"magnet, tv, would be kept", "tv", "never", nil, magnet, true},
		{"private .torrent, movie", "movie", "", private, "", false},
		{"magnet, movie", "movie", "", nil, magnet, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			eng := &recEngine{}
			reg := &memRegistry{rows: map[string]metadb.EpisodeEntry{}}
			root := t.TempDir()
			m := New(Config{MoviesDir: filepath.Join(root, "movies"), TVDir: filepath.Join(root, "tv"),
				GoStorm: eng, Registry: reg, GoStormURL: "http://engine"})
			if c.prior {
				p := filepath.Join(root, "tv", "Show", "Season.01", EpisodeFilename("Show", 1, 1, oldHash[:8]))
				if err := WriteStub(p, m.streamURL(oldHash, 1), 1000, oldHash, ""); err != nil {
					t.Fatal(err)
				}
				key := EpisodeKey("Show", 1, 1)
				reg.rows[key] = metadb.EpisodeEntry{EpisodeKey: key, Hash: oldHash, FilePath: p}
			}
			resp, err := m.Add(context.Background(), AddRequest{
				Type: c.typ, Title: "Show", Season: 1, Episode: 1, Hash: src.Hash, Magnet: c.magnet,
				TorrentFile: c.file, Replace: c.policy, DryRun: true, MetadataWait: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !resp.DryRun || resp.Readability != ReadabilityUnchecked {
				t.Fatalf("response = %+v, want a dry run with unchecked readability", resp)
			}
			if calls := eng.seen(); len(calls) != 0 {
				t.Fatalf("a dry run called the engine: %v", calls)
			}
		})
	}
}
