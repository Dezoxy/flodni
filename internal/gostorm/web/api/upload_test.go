package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"flodni/internal/gostorm/torr"
	"flodni/internal/gostorm/torr/state"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/gin-gonic/gin"
)

// fakeEngine stands in for the torrent engine behind the add handlers: it keeps
// the specs it was given, by info hash, and returns a closed torrent so the
// background metadata wait ends at once.
type fakeEngine struct {
	mu    sync.Mutex
	calls map[string]int
}

func (f *fakeEngine) add(spec *torrent.TorrentSpec, title, _, _, _ string) (*torr.Torrent, error) {
	f.mu.Lock()
	f.calls[spec.InfoHash.HexString()]++
	f.mu.Unlock()
	return &torr.Torrent{TorrentSpec: spec, Title: title, Stat: state.TorrentClosed}, nil
}

func (f *fakeEngine) hashes() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int, len(f.calls))
	for h, n := range f.calls {
		out[h] = n
	}
	return out
}

func stubEngine(t *testing.T) *fakeEngine {
	t.Helper()
	f := &fakeEngine{calls: map[string]int{}}
	prev := addTorrentToEngine
	addTorrentToEngine = f.add
	t.Cleanup(func() { addTorrentToEngine = prev })
	return f
}

// uploadRouter serves the real upload and add routes. The multipart memory limit is
// small so that a file part of a megabyte or more spills to a temp file, as a big
// upload does in production.
func uploadRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.MaxMultipartMemory = 1 << 20
	SetupRoute(r)
	return r
}

// smallTorrent returns a real single-file .torrent and its info hash.
func smallTorrent(t *testing.T) ([]byte, string) {
	t.Helper()
	info := metainfo.Info{
		Name:        "upload-cap-sample.bin",
		Length:      16384,
		PieceLength: 16384,
		Pieces:      make([]byte, 20),
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: infoBytes}
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), mi.HashInfoBytes().HexString()
}

const uploadBoundary = "flodnitestboundary"

// uploadBody builds a multipart body of a fixed boundary: a file part "file" that
// holds the .torrent and a file part "pad" of padLen bytes. padFirst puts the pad
// part before the torrent, so a big pad spills to disk before the torrent is read.
func uploadBody(t *testing.T, torrentBytes []byte, padLen int, padFirst bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.SetBoundary(uploadBoundary); err != nil {
		t.Fatal(err)
	}
	writeTorrent := func() {
		if err := w.WriteField("title", "Sample"); err != nil {
			t.Fatal(err)
		}
		fw, err := w.CreateFormFile("file", "sample.torrent")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(torrentBytes); err != nil {
			t.Fatal(err)
		}
	}
	writePad := func() {
		fw, err := w.CreateFormFile("pad", "pad.bin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.CopyN(fw, zeroReader{}, int64(padLen)); err != nil {
			t.Fatal(err)
		}
	}
	if padFirst {
		writePad()
		writeTorrent()
	} else {
		writeTorrent()
		writePad()
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// bodyOfSize returns an upload body of exactly total bytes, found by measuring the
// framing with no padding (the framing does not depend on the padding length).
func bodyOfSize(t *testing.T, torrentBytes []byte, total int, padFirst bool) []byte {
	t.Helper()
	framing := len(uploadBody(t, torrentBytes, 0, padFirst))
	body := uploadBody(t, torrentBytes, total-framing, padFirst)
	if len(body) != total {
		t.Fatalf("built a body of %d bytes, want %d", len(body), total)
	}
	return body
}

func postUpload(r *gin.Engine, body io.Reader, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/torrent/upload", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func multipartType() string { return "multipart/form-data; boundary=" + uploadBoundary }

// A body over the cap answers 413 and adds nothing, whether the length is declared
// or not, and the parser leaves no temp file behind.
func TestUploadOverTheCapAnswers413AndAddsNothing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	engine := stubEngine(t)
	r := uploadRouter()
	torrentBytes, _ := smallTorrent(t)

	// Control: with this memory limit a big file part does spill to a temp file, so
	// an empty temp folder below means something removed it.
	control := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(bodyOfSize(t, torrentBytes, 3<<20, true)))
	control.Header.Set("Content-Type", multipartType())
	if err := control.ParseMultipartForm(1 << 20); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) == 0 {
		t.Fatal("control: a 3 MiB file part left no temp file, so the leak check below proves nothing")
	}
	if err := control.MultipartForm.RemoveAll(); err != nil {
		t.Fatal(err)
	}

	body := bodyOfSize(t, torrentBytes, maxTorrentUploadBody+1, true)
	for _, c := range []struct {
		name string
		body io.Reader
	}{
		{"declared length", bytes.NewReader(body)},
		{"unknown length", io.NopCloser(bytes.NewReader(body))},
	} {
		rec := postUpload(r, c.body, multipartType())
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: status %d, want 413 (%s)", c.name, rec.Code, rec.Body)
		}
	}
	if got := engine.hashes(); len(got) != 0 {
		t.Errorf("an oversized upload added %v, want nothing", got)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("an aborted oversized upload left %d temp file(s) behind", len(entries))
	}
}

// A body of exactly the cap is not refused for its size: the torrent in it is added.
// One byte more is refused (the test above).
func TestUploadOfExactlyTheCapIsNotRefusedForSize(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	engine := stubEngine(t)
	r := uploadRouter()
	torrentBytes, hash := smallTorrent(t)

	body := bodyOfSize(t, torrentBytes, maxTorrentUploadBody, false)
	rec := postUpload(r, bytes.NewReader(body), multipartType())
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if engine.hashes()[hash] != 1 {
		t.Errorf("the torrent in a body of exactly the cap was not added: %v", engine.hashes())
	}
}

// A real upload under the cap succeeds as before, and the torrent is then found
// through the add handler: the same hash comes back and the engine holds one torrent.
func TestUploadThenAddFindsTheTorrent(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	engine := stubEngine(t)
	r := uploadRouter()
	torrentBytes, hash := smallTorrent(t)

	rec := postUpload(r, bytes.NewReader(uploadBody(t, torrentBytes, 0, false)), multipartType())
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: status %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var up struct {
		Hash  string `json:"hash"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &up); err != nil {
		t.Fatalf("upload answer is not JSON: %v (%s)", err, rec.Body)
	}
	if up.Hash != hash || up.Title != "Sample" {
		t.Errorf("upload answered hash %q title %q, want %q and %q", up.Hash, up.Title, hash, "Sample")
	}

	req := httptest.NewRequest(http.MethodPost, "/torrents",
		strings.NewReader(`{"action":"add","link":"magnet:?xt=urn:btih:`+hash+`"}`))
	req.Header.Set("Content-Type", "application/json")
	addRec := httptest.NewRecorder()
	r.ServeHTTP(addRec, req)
	if addRec.Code != http.StatusOK {
		t.Fatalf("add: status %d, want 200 (%s)", addRec.Code, addRec.Body)
	}
	var added struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(addRec.Body.Bytes(), &added); err != nil || added.Hash != hash {
		t.Errorf("add answered %q (%v), want hash %q", addRec.Body, err, hash)
	}
	if got := engine.hashes(); len(got) != 1 || got[hash] != 2 {
		t.Errorf("engine holds %v, want only %s, added by the upload and by the add", got, hash)
	}
}

// A malformed multipart body under the cap keeps today's 400, not 413.
func TestUploadMalformedMultipartStays400(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	engine := stubEngine(t)
	r := uploadRouter()
	torrentBytes, _ := smallTorrent(t)
	whole := uploadBody(t, torrentBytes, 0, false)

	for _, c := range []struct {
		name        string
		body        []byte
		contentType string
	}{
		{"not multipart at all", []byte("garbage"), multipartType()},
		{"no boundary", whole, "multipart/form-data"},
		{"truncated mid part", whole[:len(whole)/2], multipartType()},
	} {
		rec := postUpload(r, bytes.NewReader(c.body), c.contentType)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%s)", c.name, rec.Code, rec.Body)
		}
	}
	if got := engine.hashes(); len(got) != 0 {
		t.Errorf("a malformed upload added %v, want nothing", got)
	}
}

// An upload that yields no torrent answers an error instead of panicking on a nil
// torrent: 400 when the client sent nothing usable, 500 when the engine refused it.
func TestUploadWithNoUsableTorrent(t *testing.T) {
	torrentBytes, _ := smallTorrent(t)
	for _, c := range []struct {
		name      string
		file      []byte
		engineErr error
		want      int
	}{
		{"no file part", nil, nil, http.StatusBadRequest},
		{"file is not a torrent", []byte("not a torrent"), nil, http.StatusBadRequest},
		{"engine refuses it", torrentBytes, errors.New("engine closed"), http.StatusInternalServerError},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())
			engine := stubEngine(t)
			if c.engineErr != nil {
				addTorrentToEngine = func(*torrent.TorrentSpec, string, string, string, string) (*torr.Torrent, error) {
					return nil, c.engineErr
				}
			}
			var body bytes.Buffer
			w := multipart.NewWriter(&body)
			if err := w.WriteField("title", "t"); err != nil {
				t.Fatal(err)
			}
			if c.file != nil {
				part, err := w.CreateFormFile("file", "x.torrent")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := part.Write(c.file); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			rec := postUpload(uploadRouter(), &body, w.FormDataContentType())
			if rec.Code != c.want {
				t.Fatalf("status %d, want %d", rec.Code, c.want)
			}
			if got := engine.hashes(); len(got) != 0 {
				t.Fatalf("engine got %v, want nothing added", got)
			}
		})
	}
}
