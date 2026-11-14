package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"flodni/internal/gostorm/native"
	"flodni/internal/metadb"
)

type discardLogger struct{}

func (discardLogger) Printf(string, ...interface{}) {}

const rHash = "0123456789abcdef0123456789abcdef01234567"

// readabilityEnv wires a real SQLite file to the recorder and fixes the engine
// answers the session layer asks for.
func readabilityEnv(t *testing.T, peers int, live bool) *metadb.DB {
	t.Helper()
	resetSessionState(t)
	db, err := metadb.New(filepath.Join(t.TempDir(), "state.db"), discardLogger{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	prevOutcome, prevPeers, prevLive, prevLogger := native.ReachabilityOutcome, sessionActivePeers, torrentLive, logger
	t.Cleanup(func() {
		native.ReachabilityOutcome, sessionActivePeers, torrentLive, logger = prevOutcome, prevPeers, prevLive, prevLogger
	})
	logger = log.New(io.Discard, "", 0)
	native.ReachabilityOutcome = newReachabilityRecorder(db, logger)
	sessionActivePeers = func(string) int { return peers }
	torrentLive = func(string) bool { return live }
	return db
}

func newTestSession(age time.Duration) *TTFFSession {
	s := &TTFFSession{path: "/m/x.mkv", size: 1 << 30, hash: rHash}
	s.openedAt.Store(time.Now().Add(-age).UnixNano())
	s.lastOff.Store(-1)
	sessions.Store(s.path, s)
	return s
}

func records(t *testing.T, db *metadb.DB) []metadb.Readability {
	t.Helper()
	rows, err := db.ListReadability(0)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestSessionVerdictWritesTheReadabilityRecord(t *testing.T) {
	const ms = time.Millisecond
	tests := []struct {
		name      string
		peers     int
		age       time.Duration
		run       func(s *TTFFSession)
		wantRows  int
		wantFails int64
		wantStamp bool
		wantWhy   string
	}{
		{"swarm-served read acquits and stamps", 3, time.Minute, func(s *TTFFSession) {
			s.recordRead(srcFetchBlock, ms, 4<<20, 0)
		}, 1, 0, true, ""},
		{"probe-sized swarm read acquits", 3, time.Minute, func(s *TTFFSession) {
			s.recordRead(srcFetchBlock, ms, 100, 0)
		}, 1, 0, true, ""},
		{"cache-only session writes nothing", 3, time.Minute, func(s *TTFFSession) {
			s.recordRead(srcRACacheHit, ms, 4<<20, 0)
		}, 0, 0, false, ""},
		{"cache bytes then a failed read past the floor condemns", 0, time.Minute, func(s *TTFFSession) {
			s.recordRead(srcWarmupHead, ms, 4<<20, 0)
			ttffReadFailed(s.path)
		}, 1, 1, false, failReasonNoPeers},
		{"failed read under the floor writes nothing", 0, 5 * time.Second, func(s *TTFFSession) {
			ttffReadFailed(s.path)
		}, 0, 0, false, ""},
		{"fetch-sourced read with no peers does not acquit", 0, time.Minute, func(s *TTFFSession) {
			s.recordRead(srcFetchBlock, ms, 38, 0)
			ttffReadFailed(s.path)
		}, 1, 1, false, failReasonNoPeers},
		{"failed then swarm-served in one session acquits", 3, time.Minute, func(s *TTFFSession) {
			ttffReadFailed(s.path)
			s.recordRead(srcFetchBlock, ms, 1<<20, 0)
		}, 1, 0, true, ""},
		{"no failed-read call writes nothing", 0, time.Minute, func(s *TTFFSession) {
			s.recordRead(srcWarmupHead, ms, 4<<20, 0)
		}, 0, 0, false, ""},
		{"peers connected but the read gave up is a timeout", 3, time.Minute, func(s *TTFFSession) {
			ttffReadFailed(s.path)
		}, 1, 1, false, failReasonTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := readabilityEnv(t, tt.peers, true)
			s := newTestSession(tt.age)
			tt.run(s)
			s.closeSession()
			rows := records(t, db)
			if len(rows) != tt.wantRows {
				t.Fatalf("rows = %+v, want %d", rows, tt.wantRows)
			}
			if tt.wantRows == 0 {
				return
			}
			r := rows[0]
			if r.Hash != rHash || r.FailCount != tt.wantFails || (r.LastSwarmRead != 0) != tt.wantStamp {
				t.Fatalf("row = %+v", r)
			}
			if tt.wantWhy != "" && (r.LastReason != tt.wantWhy || r.LastSource != failSourceMount) {
				t.Fatalf("reason/source = %q/%q, want %q/mount", r.LastReason, r.LastSource, tt.wantWhy)
			}
		})
	}
}

func TestClassifyFailure(t *testing.T) {
	tests := []struct {
		name  string
		live  bool
		peers int
		hash  string
		want  string
	}{
		{"not running", false, 5, rHash, failReasonNotActive},
		{"running without connections", true, 0, rHash, failReasonNoPeers},
		{"running with connections", true, 2, rHash, failReasonTimeout},
		{"no hash", true, 2, "", failReasonUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readabilityEnv(t, tt.peers, tt.live)
			if got := classifyFailure(tt.hash); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClassifyFailureBoundsAHungEngine(t *testing.T) {
	readabilityEnv(t, 0, true)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	sessionActivePeers = func(string) int { <-release; return 0 }
	start := time.Now()
	if got := classifyFailure(rHash); got != failReasonUnknown {
		t.Fatalf("got %q, want unknown", got)
	}
	if d := time.Since(start); d > 5*classifyTimeout {
		t.Fatalf("classification took %s", d)
	}
}

func TestMetainfoOutcomeResetsWithoutStamp(t *testing.T) {
	db := readabilityEnv(t, 0, true)
	now := time.Now()
	if err := db.RecordMetadataFailure(rHash, failReasonTimeout, failSourceMount, now); err != nil {
		t.Fatal(err)
	}
	native.ReachabilityOutcome(native.Outcome{Hash: rHash, Kind: native.OutcomeMetainfo})
	rows := records(t, db)
	if len(rows) != 1 || rows[0].FailCount != 0 || rows[0].LastSwarmRead != 0 {
		t.Fatalf("rows = %+v", rows)
	}
	native.ReachabilityOutcome(native.Outcome{Hash: "ffff", Kind: native.OutcomeMetainfo})
	if got := records(t, db); len(got) != 1 {
		t.Fatalf("a metainfo outcome inserted a row: %+v", got)
	}
}

type fakeLister struct {
	rows []metadb.Readability
	err  error
	min  int
}

func (f *fakeLister) ListReadability(min int) ([]metadb.Readability, error) {
	f.min = min
	if f.err != nil {
		return nil, f.err
	}
	var out []metadb.Readability
	for _, r := range f.rows {
		if r.FailCount >= int64(min) {
			out = append(out, r)
		}
	}
	return out, nil
}

func getHealth(h http.HandlerFunc, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestTorrentsHealthHandler(t *testing.T) {
	quiet := log.New(io.Discard, "", 0)
	store := &fakeLister{rows: []metadb.Readability{
		{Hash: "aa", FailCount: 2, FirstFail: 1_700_000_000, LastFail: 1_700_003_600,
			LastReason: "no_peers", LastSource: "mount", LastSwarmRead: 0},
		{Hash: "bb", FailCount: 0, LastSwarmRead: 1_700_007_200},
	}}
	h := newTorrentsHealthHandler(store, nil, quiet)

	rec := getHealth(h, http.MethodGet, "/api/torrents/health")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d, cache-control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	var body struct {
		Total    int                          `json:"total"`
		Torrents []map[string]json.RawMessage `json:"torrents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 2 || len(body.Torrents) != 2 {
		t.Fatalf("body = %s", rec.Body.String())
	}
	wantKeys := []string{"consecutive_failures", "files", "first_failure_at", "hash", "last_failure_at",
		"last_failure_reason", "last_failure_source", "last_swarm_read_at", "swarm"}
	for _, tor := range body.Torrents {
		var keys []string
		for k := range tor {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, wantKeys) {
			t.Fatalf("keys = %v, want %v", keys, wantKeys)
		}
	}
	if string(body.Torrents[0]["first_failure_at"]) != `"2023-11-14T22:13:20Z"` ||
		string(body.Torrents[0]["last_swarm_read_at"]) != "null" ||
		string(body.Torrents[1]["last_failure_reason"]) != "null" ||
		string(body.Torrents[1]["last_swarm_read_at"]) != `"2023-11-15T00:13:20Z"` {
		t.Fatalf("values = %s", rec.Body.String())
	}

	if rec := getHealth(h, http.MethodGet, "/api/torrents/health?failing=1"); rec.Code != 200 || store.min != 1 {
		t.Fatalf("failing=1: status %d, min %d", rec.Code, store.min)
	}
	if rec := getHealth(h, http.MethodGet, "/api/torrents/health?failing=1&min_failures=2"); rec.Code != 200 || store.min != 2 {
		t.Fatalf("min_failures=2: status %d, min %d", rec.Code, store.min)
	}
	for _, q := range []string{"failing=yes", "min_failures=-1", "min_failures=x"} {
		if rec := getHealth(h, http.MethodGet, "/api/torrents/health?"+q); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, rec.Code)
		}
	}
	if rec := getHealth(h, http.MethodPost, "/api/torrents/health"); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
		t.Errorf("POST: status %d, allow %q", rec.Code, rec.Header().Get("Allow"))
	}
	if rec := getHealth(newTorrentsHealthHandler(nil, nil, quiet), http.MethodGet, "/api/torrents/health"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("nil store: status %d, want 503", rec.Code)
	}
	failing := newTorrentsHealthHandler(&fakeLister{err: errors.New("disk on fire")}, nil, quiet)
	rec = getHealth(failing, http.MethodGet, "/api/torrents/health")
	if rec.Code != http.StatusInternalServerError || rec.Body.String() != "cannot read the readability record\n" {
		t.Errorf("store error: status %d, body %q", rec.Code, rec.Body.String())
	}
}

func fileSnapshot(m map[string][]fileHealth) func() map[string][]fileHealth {
	return func() map[string][]fileHealth { return m }
}

// files comes last in the object (the e2e grep depends on the order), is always an
// array, times are RFC 3339 or null, and a torrent with file rows but no failure row
// is listed with a zero count.
func TestTorrentsHealthFilesArray(t *testing.T) {
	quiet := log.New(io.Discard, "", 0)
	store := &fakeLister{rows: []metadb.Readability{
		{Hash: "aa", FailCount: 2, FirstFail: 1_700_000_000, LastFail: 1_700_003_600, LastReason: "no_peers", LastSource: "mount"},
		{Hash: "bb", FailCount: 0, LastSwarmRead: 1_700_007_200},
	}}
	files := map[string][]fileHealth{
		"aa": {{FileIndex: 1, Readability: "warmed", Cache: "cached", LastSwarmDeliveryAt: unixOrNil(1_700_000_000)}},
		"cc": {{FileIndex: 0, Readability: "unknown", Cache: "evicted"}},
	}
	h := newTorrentsHealthHandler(store, fileSnapshot(files), quiet)
	rec := getHealth(h, http.MethodGet, "/api/torrents/health")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	// Key order: files is the last key of every torrent object.
	for _, hash := range []string{"aa", "bb", "cc"} {
		re := regexp.MustCompile(`"hash":"` + hash + `"[^}]*"last_swarm_read_at":(null|"[^"]*"),"files":\[`)
		if !re.MatchString(body) {
			t.Errorf("hash %s: files does not follow last_swarm_read_at in %s", hash, body)
		}
	}
	var out struct {
		Total    int `json:"total"`
		Torrents []struct {
			Hash                string `json:"hash"`
			ConsecutiveFailures int64  `json:"consecutive_failures"`
			FirstFailureAt      *string
			Files               []map[string]json.RawMessage `json:"files"`
		} `json:"torrents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Total != 3 || out.Torrents[0].Hash != "aa" || out.Torrents[1].Hash != "bb" || out.Torrents[2].Hash != "cc" {
		t.Fatalf("torrents = %s", body)
	}
	if out.Torrents[2].ConsecutiveFailures != 0 {
		t.Fatalf("files-only torrent: %+v", out.Torrents[2])
	}
	if out.Torrents[1].Files == nil || len(out.Torrents[1].Files) != 0 {
		t.Fatalf("bb files must be []: %s", body)
	}
	wantKeys := []string{"cache", "failed_attempts", "file_index", "first_failed_at", "last_attempt_at",
		"last_swarm_delivery_at", "readability"}
	var keys []string
	for k := range out.Torrents[0].Files[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Fatalf("file keys = %v, want %v", keys, wantKeys)
	}
	f := out.Torrents[0].Files[0]
	if string(f["last_swarm_delivery_at"]) != `"2023-11-14T22:13:20Z"` || string(f["first_failed_at"]) != "null" ||
		string(f["readability"]) != `"warmed"` || string(f["cache"]) != `"cached"` {
		t.Fatalf("file = %v", f)
	}
}

// A failure filter keeps its per-torrent meaning: files-only torrents are not listed.
func TestTorrentsHealthFilterIgnoresFilesOnlyTorrents(t *testing.T) {
	quiet := log.New(io.Discard, "", 0)
	store := &fakeLister{rows: []metadb.Readability{{Hash: "aa", FailCount: 2}}}
	files := map[string][]fileHealth{"cc": {{FileIndex: 0, Readability: "unknown", Cache: "cached"}}}
	h := newTorrentsHealthHandler(store, fileSnapshot(files), quiet)
	for _, q := range []string{"failing=1", "min_failures=1", "min_failures=2"} {
		rec := getHealth(h, http.MethodGet, "/api/torrents/health?"+q)
		if strings.Contains(rec.Body.String(), `"cc"`) {
			t.Errorf("%s listed a files-only torrent: %s", q, rec.Body.String())
		}
	}
	// A nil snapshot function is allowed and gives empty arrays.
	rec := getHealth(newTorrentsHealthHandler(store, nil, quiet), http.MethodGet, "/api/torrents/health")
	if !strings.Contains(rec.Body.String(), `"files":[]`) {
		t.Errorf("nil snapshot: %s", rec.Body.String())
	}
}
