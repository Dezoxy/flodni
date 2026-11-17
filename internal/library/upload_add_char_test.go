package library

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"flodni/internal/metadb"
)

// Characterization of the sequence a library add makes for a release that comes with its
// .torrent (durable records 18 and 19 must not change it): POST /torrent/upload stores the
// bytes, the add (POST /torrents) follows, and the metadata poll finds the files the
// upload brought. A fake engine plays the engine's side of that contract over HTTP, so the
// real GoStormClient and the real handler are in the path.
type uploadEngine struct {
	mu     sync.Mutex
	calls  []string
	stored map[string]bool // hashes whose .torrent was uploaded
	files  []FileStat
	failUp bool
}

func (u *uploadEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch r.URL.Path {
	case "/torrent/upload":
		u.calls = append(u.calls, "upload")
		if u.failUp {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		data, _ := io.ReadAll(f)
		src, err := ParseTorrentFile(data)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		u.stored[src.Hash] = true
		_ = json.NewEncoder(w).Encode(map[string]string{"hash": src.Hash})
	case "/torrents":
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req["action"] {
		case "add":
			u.calls = append(u.calls, "add")
			_ = json.NewEncoder(w).Encode(map[string]string{"hash": HashFromMagnet(req["link"].(string))})
		case "get":
			u.calls = append(u.calls, "get")
			h, _ := req["hash"].(string)
			if !u.stored[h] {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(TorrentStats{Hash: h, FileStats: u.files})
		}
	}
}

func uploadAddEnv(t *testing.T, eng *uploadEngine) (*Manager, string) {
	t.Helper()
	srv := httptest.NewServer(eng)
	t.Cleanup(srv.Close)
	movies := filepath.Join(t.TempDir(), "movies")
	m := New(Config{
		MoviesDir: movies, TVDir: filepath.Join(t.TempDir(), "tv"),
		GoStorm: NewGoStormClient(srv.URL, ""), Registry: &memRegistry{rows: map[string]metadb.EpisodeEntry{}},
		GoStormURL: srv.URL,
	})
	return m, movies
}

func TestUploadThenLibraryAddFindsTheMetadata(t *testing.T) {
	file := torrentFile(t, false)
	src, err := ParseTorrentFile(file)
	if err != nil {
		t.Fatal(err)
	}
	eng := &uploadEngine{stored: map[string]bool{}, files: movieTorrent}
	m, movies := uploadAddEnv(t, eng)

	body, _ := json.Marshal(map[string]interface{}{
		"title": "Inception", "hash": src.Hash, "year": 2010, "metadata_wait": 5,
		"torrent_file": base64.StdEncoding.EncodeToString(file),
	})
	rec := httptest.NewRecorder()
	NewHandler(m).Add(rec, httptest.NewRequest(http.MethodPost, "/api/library/add", strings.NewReader(string(body))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body)
	}
	if got := strings.Join(eng.calls, ","); !strings.HasPrefix(got, "upload,add,get") {
		t.Fatalf("engine calls = %s, want upload, add, then the metadata poll", got)
	}
	name := "Inception_2010_1080p_" + src.Hash[len(src.Hash)-8:] + ".mkv"
	want := `{"hash":"` + src.Hash + `","title":"Inception","type":"movie","files":[{"path":"` +
		jsonPath(filepath.Join(movies, name)) + `","fuse_path":"movies/` + name +
		`","size":8589934592,"file_index":2}],"already_present":false}` + "\n"
	if rec.Body.String() != want {
		t.Fatalf("body\n got %s\nwant %s", rec.Body, want)
	}
}

// A failed upload still fails the add with a non-2xx and a message, and nothing is added.
func TestUploadFailureStillFailsTheLibraryAdd(t *testing.T) {
	file := torrentFile(t, false)
	src, _ := ParseTorrentFile(file)
	eng := &uploadEngine{stored: map[string]bool{}, files: movieTorrent, failUp: true}
	m, _ := uploadAddEnv(t, eng)

	body, _ := json.Marshal(map[string]interface{}{
		"title": "Inception", "hash": src.Hash, "year": 2010, "metadata_wait": 1,
		"torrent_file": base64.StdEncoding.EncodeToString(file),
	})
	rec := httptest.NewRecorder()
	NewHandler(m).Add(rec, httptest.NewRequest(http.MethodPost, "/api/library/add", strings.NewReader(string(body))))
	if rec.Code < 400 || !strings.Contains(rec.Body.String(), ".torrent upload") {
		t.Fatalf("status %d body %s, want a non-2xx naming the upload", rec.Code, rec.Body)
	}
	for _, c := range eng.calls {
		if c == "add" {
			t.Fatalf("engine add ran after a failed upload: %v", eng.calls)
		}
	}
}
