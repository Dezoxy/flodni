package library

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"flodni/internal/metadb"
)

// noMetadataEngine answers the list and rem calls and handles an add as the test says.
func noMetadataEngine(t *testing.T, add func(w http.ResponseWriter, r *http.Request, body map[string]interface{})) *Manager {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body["action"] {
		case "list":
			_, _ = w.Write([]byte(`[]`))
		case "add":
			add(w, r, body)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewGoStormClient(srv.URL, "")
	c.addMargin = 300 * time.Millisecond
	root := t.TempDir()
	return New(Config{
		MoviesDir: filepath.Join(root, "movies"), TVDir: filepath.Join(root, "tv"),
		GoStorm: c, Registry: &memRegistry{rows: map[string]metadb.EpisodeEntry{}}, GoStormURL: srv.URL,
	})
}

func engine504(w http.ResponseWriter, _ *http.Request, _ map[string]interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusGatewayTimeout)
	_, _ = w.Write([]byte(`{"error":"no metadata before the timeout"}`))
}

// The engine's "no metadata" is the library's 504 with its message, as before the engine
// add began to wait: not a 502 "rejected the torrent".
func TestEngineNoMetadataIsALibrary504(t *testing.T) {
	m := noMetadataEngine(t, engine504)
	_, err := m.Add(context.Background(), AddRequest{Title: "Inception", Hash: charPackHash, Year: 2010, MetadataWait: 5})
	if e, ok := err.(*Error); !ok || e.Status != http.StatusGatewayTimeout || !strings.Contains(e.Message, "no metadata") {
		t.Fatalf("Add err = %#v, want a 504 naming no metadata", err)
	}
	_, err = m.Inspect(context.Background(), InspectRequest{Title: "Inception", Hash: charPackHash, MetadataWait: 5})
	if e, ok := err.(*Error); !ok || e.Status != http.StatusGatewayTimeout || !strings.Contains(e.Message, "no metadata") {
		t.Fatalf("Inspect err = %#v, want a 504 naming no metadata", err)
	}
}

// metadata_wait bounds the engine add: it is sent to the engine, and an engine that
// outlasts wait plus the margin is cut off with a 504.
func TestMetadataWaitBoundsTheEngineAdd(t *testing.T) {
	var sent atomic.Int64
	m := noMetadataEngine(t, func(w http.ResponseWriter, r *http.Request, body map[string]interface{}) {
		if v, ok := body["metadata_wait"].(float64); ok {
			sent.Store(int64(v))
		}
		select {
		case <-time.After(15 * time.Second):
		case <-r.Context().Done():
		}
	})
	start := time.Now()
	_, err := m.Add(context.Background(), AddRequest{Title: "Inception", Hash: charPackHash, Year: 2010, MetadataWait: 1})
	elapsed := time.Since(start)
	if e, ok := err.(*Error); !ok || e.Status != http.StatusGatewayTimeout {
		t.Fatalf("err = %#v, want a 504", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("answered after %v, want about metadata_wait (1 s) plus the margin", elapsed)
	}
	if sent.Load() != 1 {
		t.Fatalf("engine was sent metadata_wait %d, want 1", sent.Load())
	}
}
