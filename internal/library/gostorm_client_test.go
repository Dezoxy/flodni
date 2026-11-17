package library

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// authRecorder answers every route with just enough for the client to succeed
// and records the Authorization header per path.
func authRecorder(t *testing.T) (*httptest.Server, func() map[string]string) {
	t.Helper()
	var mu sync.Mutex
	seen := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
		switch r.URL.Path {
		case "/torrents":
			_, _ = w.Write([]byte(`{"hash":"` + testHash + `"}`))
		case "/torrent/upload":
			_, _ = w.Write([]byte(`{"hash":"` + testHash + `"}`))
		case "/stream":
			_, _ = w.Write([]byte("x"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() map[string]string {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]string{}
		for k, v := range seen {
			out[k] = v
		}
		return out
	}
}

const testHash = "0123456789abcdef0123456789abcdef01234567"

func callAll(t *testing.T, c *GoStormClient) {
	t.Helper()
	ctx := context.Background()
	if _, err := c.AddTorrent(ctx, "magnet:?xt=urn:btih:"+testHash, "t"); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	if _, err := c.UploadTorrent(ctx, []byte("d4:infod4:name1:xee"), "t"); err != nil {
		t.Fatalf("UploadTorrent: %v", err)
	}
	if err := c.RemoveTorrent(ctx, testHash); err != nil {
		t.Fatalf("RemoveTorrent: %v", err)
	}
	if err := c.ReadFileHead(ctx, testHash, 1, 1); err != nil {
		t.Fatalf("ReadFileHead: %v", err)
	}
}

func TestGoStormClientSendsToken(t *testing.T) {
	srv, seen := authRecorder(t)
	callAll(t, NewGoStormClient(srv.URL, "s3cret"))
	got := seen()
	for _, path := range []string{"/torrents", "/torrent/upload", "/stream"} {
		if got[path] != "Bearer s3cret" {
			t.Errorf("%s: Authorization = %q, want %q", path, got[path], "Bearer s3cret")
		}
	}
}

func TestGoStormClientNoTokenNoHeader(t *testing.T) {
	srv, seen := authRecorder(t)
	callAll(t, NewGoStormClient(srv.URL, ""))
	got := seen()
	if len(got) != 3 {
		t.Fatalf("paths seen = %v, want /torrents, /torrent/upload, /stream", got)
	}
	for path, h := range got {
		if h != "" {
			t.Errorf("%s: Authorization = %q, want none", path, h)
		}
	}
}

// A magnet add must ask the engine to store the record: the handler reads
// save_to_db, so any other key leaves the torrent unsaved until its first idle
// close or mount open.
func TestAddTorrentAsksTheEngineToSave(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(`{"hash":"` + testHash + `"}`))
	}))
	defer srv.Close()

	c := NewGoStormClient(srv.URL, "")
	if _, err := c.AddTorrent(context.Background(), "magnet:?xt=urn:btih:"+testHash, "t"); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	if body["action"] != "add" || body["link"] != "magnet:?xt=urn:btih:"+testHash {
		t.Errorf("body = %v, want action add and the magnet", body)
	}
	if body["save_to_db"] != true {
		t.Errorf("save_to_db = %v, want true (body %v)", body["save_to_db"], body)
	}
	if _, stale := body["save"]; stale {
		t.Errorf("body carries the unread key \"save\": %v", body)
	}
}

// The engine's add now answers once the record is saved, which can take the whole metadata
// wait: the client's general 30 s limit must not cut that short.
func TestAddTorrentWaitsLongerThanTheGeneralTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_, _ = w.Write([]byte(`{"hash":"` + testHash + `"}`))
	}))
	defer srv.Close()
	c := NewGoStormClient(srv.URL, "")
	c.httpClient.Timeout = 100 * time.Millisecond // the general limit, shrunk
	c.addTimeout = 5 * time.Second
	if _, confirmed, err := c.AddTorrentConfirmed(context.Background(), "magnet:?xt=urn:btih:"+testHash, "t"); err != nil || !confirmed {
		t.Fatalf("confirmed %v, err %v", confirmed, err)
	}
	c.addTimeout = 100 * time.Millisecond
	if _, _, err := c.AddTorrentConfirmed(context.Background(), "magnet:?xt=urn:btih:"+testHash, "t"); err == nil {
		t.Fatal("an add that outlasts its own limit succeeded")
	}
}
