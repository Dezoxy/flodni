package library

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"flodni/internal/metadb"
)

const (
	oldHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	newHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// replaceEngine serves one pack and records what the manager asks of it.
type replaceEngine struct {
	files    []FileStat
	readErr  error         // returned by ReadFileHead
	readWait time.Duration // when set, ReadFileHead blocks until ctx ends
	failFile map[int]error // per file index; checked before readErr

	mu      sync.Mutex
	reads   int
	readIDs []int
	removed []string
}

func (e *replaceEngine) AddTorrent(_ context.Context, magnet, _ string) (string, error) {
	return HashFromMagnet(magnet), nil
}
func (e *replaceEngine) GetTorrentInfo(_ context.Context, hash string, _ int) (*TorrentStats, error) {
	return &TorrentStats{Hash: hash, FileStats: e.files}, nil
}
func (e *replaceEngine) RemoveTorrent(_ context.Context, hash string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.removed = append(e.removed, hash)
	return nil
}
func (e *replaceEngine) ListTorrents(context.Context) ([]TorrentStats, error) { return nil, nil }
func (e *replaceEngine) ReadFileHead(ctx context.Context, _ string, id int, _ int64) error {
	e.mu.Lock()
	e.reads++
	e.readIDs = append(e.readIDs, id)
	e.mu.Unlock()
	if err := e.failFile[id]; err != nil {
		return err
	}
	if e.readWait > 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	return e.readErr
}

type memRegistry struct {
	rows map[string]metadb.EpisodeEntry
}

func (r *memRegistry) UpsertEpisode(k string, e metadb.EpisodeEntry) error { r.rows[k] = e; return nil }
func (r *memRegistry) GetEpisode(k string) (*metadb.EpisodeEntry, bool, error) {
	e, ok := r.rows[k]
	if !ok {
		return nil, false, nil
	}
	return &e, true, nil
}
func (r *memRegistry) DeleteEpisode(k string) error { delete(r.rows, k); return nil }
func (r *memRegistry) EpisodesByFilePath(p string) ([]metadb.EpisodeEntry, error) {
	var out []metadb.EpisodeEntry
	for _, e := range r.rows {
		if e.FilePath == p {
			out = append(out, e)
		}
	}
	return out, nil
}

// replaceFixture files S01E01 from an old release, then returns a manager whose
// next pack add (S01E01 and S01E02) would replace it.
func replaceFixture(t *testing.T, eng *replaceEngine) (m *Manager, oldStub string, reg *memRegistry) {
	t.Helper()
	tv := t.TempDir()
	reg = &memRegistry{rows: map[string]metadb.EpisodeEntry{}}
	m = New(Config{TVDir: tv, GoStorm: eng, Registry: reg, GoStormURL: "http://engine"})

	oldStub = filepath.Join(tv, "Show", "Season.01", EpisodeFilename("Show", 1, 1, oldHash[:8]))
	if err := WriteStub(oldStub, m.streamURL(oldHash, 1), 1000, "magnet:?xt=urn:btih:"+oldHash, ""); err != nil {
		t.Fatal(err)
	}
	key := EpisodeKey("Show", 1, 1)
	reg.rows[key] = metadb.EpisodeEntry{EpisodeKey: key, Hash: oldHash, FilePath: oldStub, Source: "api"}

	eng.files = []FileStat{
		{ID: 1, Path: "Show.S01/Show.S01E01.mkv", Length: 1 << 30},
		{ID: 2, Path: "Show.S01/Show.S01E02.mkv", Length: 1 << 30},
	}
	return m, oldStub, reg
}

func addPack(m *Manager) (*AddResponse, error) {
	return m.Add(context.Background(), AddRequest{
		Type: "tv", Title: "Show", Hash: newHash, Season: 1, MetadataWait: 1,
	})
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestReplacementThatCannotBeReadKeepsExistingStubs(t *testing.T) {
	eng := &replaceEngine{readErr: errors.New("no peers")}
	m, oldStub, reg := replaceFixture(t, eng)
	before, _ := os.ReadFile(oldStub)

	_, err := addPack(m)

	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnprocessableEntity {
		t.Fatalf("err = %v, want a 422 library error", err)
	}
	after, rerr := os.ReadFile(oldStub)
	if rerr != nil || string(after) != string(before) {
		t.Fatalf("old stub changed or gone: %v", rerr)
	}
	if got := reg.rows[EpisodeKey("Show", 1, 1)]; got.Hash != oldHash || got.FilePath != oldStub {
		t.Fatalf("registry row changed: %+v", got)
	}
	if _, ok := reg.rows[EpisodeKey("Show", 1, 2)]; ok {
		t.Fatal("registry row written for the new release")
	}
	newStub := filepath.Join(filepath.Dir(oldStub), EpisodeFilename("Show", 1, 1, newHash[:8]))
	if exists(newStub) || exists(filepath.Join(filepath.Dir(oldStub), EpisodeFilename("Show", 1, 2, newHash[:8]))) {
		t.Fatal("a new stub was written")
	}
	if len(eng.removed) != 1 || eng.removed[0] != newHash {
		t.Fatalf("removed torrents = %v, want only the new one", eng.removed)
	}
}

func TestReplacementThatDeliversReplacesStubs(t *testing.T) {
	eng := &replaceEngine{}
	m, oldStub, reg := replaceFixture(t, eng)

	resp, err := addPack(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Files) != 2 || eng.reads != 2 {
		t.Fatalf("files = %d, reads = %d, want 2 and 2", len(resp.Files), eng.reads)
	}
	if exists(oldStub) {
		t.Fatal("old stub still there")
	}
	if got := reg.rows[EpisodeKey("Show", 1, 1)]; got.Hash != newHash {
		t.Fatalf("registry row = %+v, want the new release", got)
	}
	if len(eng.removed) != 1 || eng.removed[0] != oldHash {
		t.Fatalf("removed torrents = %v, want only the old one", eng.removed)
	}
}

func TestAddThatReplacesNothingNeverReads(t *testing.T) {
	eng := &replaceEngine{readErr: errors.New("would fail")}
	m, _, reg := replaceFixture(t, eng)
	delete(reg.rows, EpisodeKey("Show", 1, 1))

	if _, err := addPack(m); err != nil {
		t.Fatal(err)
	}
	if eng.reads != 0 {
		t.Fatalf("reads = %d, want 0", eng.reads)
	}
}

func TestReaddingTheSameReleaseNeverReads(t *testing.T) {
	eng := &replaceEngine{readErr: errors.New("would fail")}
	m, _, reg := replaceFixture(t, eng)
	// The row already points at the new release's own stub.
	stub := filepath.Join(m.cfg.TVDir, "Show", "Season.01", EpisodeFilename("Show", 1, 1, newHash[:8]))
	reg.rows[EpisodeKey("Show", 1, 1)] = metadb.EpisodeEntry{Hash: newHash, FilePath: stub}

	if _, err := addPack(m); err != nil {
		t.Fatal(err)
	}
	if eng.reads != 0 {
		t.Fatalf("reads = %d, want 0", eng.reads)
	}
}

func TestReplacementReadTimeoutKeepsExistingStubs(t *testing.T) {
	old := replacementReadTimeout
	replacementReadTimeout = 50 * time.Millisecond
	t.Cleanup(func() { replacementReadTimeout = old })

	eng := &replaceEngine{readWait: time.Hour}
	m, oldStub, _ := replaceFixture(t, eng)

	start := time.Now()
	_, err := addPack(m)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnprocessableEntity {
		t.Fatalf("err = %v, want a 422 library error", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the read did not time out")
	}
	if !exists(oldStub) {
		t.Fatal("old stub gone")
	}
}

// bigPack serves five episodes plus a sample clip that names an episode and an
// extra that names none.
func bigPack(eng *replaceEngine) {
	eng.files = []FileStat{
		{ID: 1, Path: "Show.S01/Show.S01E01.mkv", Length: 1 << 30},
		{ID: 2, Path: "Show.S01/Show.S01E02.mkv", Length: 1 << 30},
		{ID: 3, Path: "Show.S01/Show.S01E03.mkv", Length: 1 << 30},
		{ID: 4, Path: "Show.S01/Show.S01E04.mkv", Length: 1 << 30},
		{ID: 5, Path: "Show.S01/Show.S01E05.mkv", Length: 1 << 30},
		{ID: 6, Path: "Show.S01/Sample/show.s01e01.sample.mkv", Length: 1 << 20},
		{ID: 7, Path: "Show.S01/Extras/Featurette.mkv", Length: 1 << 20},
	}
}

func TestPackWhoseSampledOtherEpisodeDoesNotReadKeepsStubs(t *testing.T) {
	eng := &replaceEngine{failFile: map[int]error{5: errors.New("no peers")}}
	m, oldStub, reg := replaceFixture(t, eng)
	bigPack(eng)

	_, err := addPack(m)

	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnprocessableEntity {
		t.Fatalf("err = %v, want a 422 library error", err)
	}
	if !exists(oldStub) {
		t.Fatal("old stub gone")
	}
	if got := reg.rows[EpisodeKey("Show", 1, 1)]; got.Hash != oldHash {
		t.Fatalf("registry row changed: %+v", got)
	}
	if _, ok := reg.rows[EpisodeKey("Show", 1, 2)]; ok {
		t.Fatal("registry row written for the new release")
	}
}

func TestPackWhoseSampledFilesAllReadReplaces(t *testing.T) {
	// Episode 3 is outside the sample and dead: the documented gap in the check.
	eng := &replaceEngine{failFile: map[int]error{3: errors.New("no peers")}}
	m, oldStub, reg := replaceFixture(t, eng)
	bigPack(eng)

	resp, err := addPack(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Files) != 5 {
		t.Fatalf("files = %d, want 5", len(resp.Files))
	}
	if exists(oldStub) {
		t.Fatal("old stub still there")
	}
	if got := reg.rows[EpisodeKey("Show", 1, 1)]; got.Hash != newHash {
		t.Fatalf("registry row = %+v, want the new release", got)
	}
}

func TestSampleIsFirstAndLastEpisodeNeverSamplesOrExtras(t *testing.T) {
	eng := &replaceEngine{}
	m, _, _ := replaceFixture(t, eng)
	bigPack(eng)

	if _, err := addPack(m); err != nil {
		t.Fatal(err)
	}
	got := append([]int(nil), eng.readIDs...)
	sort.Ints(got)
	// E01 is both the replaced and the first episode: two distinct reads.
	if len(got) != 2 || got[0] != 1 || got[1] != 5 {
		t.Fatalf("read file ids = %v, want [1 5] (no sample clip, no extra)", got)
	}
}

func TestSingleFileReleaseReadsOnce(t *testing.T) {
	eng := &replaceEngine{}
	m, _, _ := replaceFixture(t, eng)
	eng.files = eng.files[:1]

	if _, err := addPack(m); err != nil {
		t.Fatal(err)
	}
	if eng.reads != 1 {
		t.Fatalf("reads = %d, want 1", eng.reads)
	}
}

func TestSampleSizeNeverExceedsTheConstant(t *testing.T) {
	// The replaced episode sits in the middle: three distinct reads, no more.
	if got := replacementSample(5, 2); len(got) != replacementSampleSize {
		t.Fatalf("sample = %v, want %d indexes", got, replacementSampleSize)
	}
	if got := replacementSample(1, 0); len(got) != 1 {
		t.Fatalf("sample = %v, want one index", got)
	}
}
