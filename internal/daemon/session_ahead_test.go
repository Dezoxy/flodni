package daemon

import (
	"encoding/json"
	"flodni/internal/gostorm/native"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Piece length 100, the file starts 50 bytes into the torrent: file bytes [-50,50) are
// piece 0, [50,150) piece 1, [150,250) piece 2, [250,350) piece 3.
func aheadPieces(complete ...bool) *native.FilePieceMap {
	return &native.FilePieceMap{PieceLen: 100, FileOffset: 50, First: 0, Complete: complete}
}

func TestComputeAhead(t *testing.T) {
	none := func(off, limit int64) int64 { return 0 }
	cacheRange := func(a, b int64) func(off, limit int64) int64 {
		return func(off, limit int64) int64 {
			if off < a || off >= b {
				return 0
			}
			return min(b, limit) - off
		}
	}
	tests := []struct {
		name            string
		from, limit     int64
		pm              *native.FilePieceMap
		cache           func(off, limit int64) int64
		ahead, frontier int64
	}{
		{"engine only, stops at the gap", 30, 1000, aheadPieces(true, true, false, true), none, 120, 150},
		{"cache bridges part of the gap", 30, 1000, aheadPieces(true, true, false, true), cacheRange(150, 200), 170, 150},
		{"cache fills the gap, engine continues", 30, 1000, aheadPieces(true, true, false, true), cacheRange(150, 250), 320, 150},
		{"end of file clamps", 30, 300, aheadPieces(true, true, true, true), none, 270, 300},
		{"piece under the read offset missing", 30, 1000, aheadPieces(false, true), none, 0, 30},
		{"cache only", 30, 1000, aheadPieces(false), cacheRange(0, 90), 60, 30},
		{"scan stops at the cap", 30, 130, aheadPieces(true, true, true), none, 100, 130},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ahead, frontier := computeAhead(tt.from, tt.limit, tt.pm, tt.cache)
			if ahead != tt.ahead || frontier != tt.frontier {
				t.Fatalf("ahead %d frontier %d, want %d %d", ahead, frontier, tt.ahead, tt.frontier)
			}
		})
	}
}

func withAheadSources(t *testing.T, pm *native.FilePieceMap, cache func(path string, off, limit int64) int64) {
	t.Helper()
	oldP, oldC := sessionFilePieces, sessionCacheRun
	sessionFilePieces = func(string, int, int64, int64) *native.FilePieceMap { return pm }
	sessionCacheRun = cache
	t.Cleanup(func() { sessionFilePieces, sessionCacheRun = oldP, oldC })
}

func openAheadSession(path string, size int64, fileIndex int, lastOff int64, age time.Duration) *TTFFSession {
	s := &TTFFSession{path: path, size: size, hash: "h", fileIndex: fileIndex, id: 1}
	s.openedAt.Store(time.Now().Add(-age).UnixNano())
	s.lastOff.Store(lastOff)
	sessions.Store(path, s)
	return s
}

func TestOpenSessionViewsAhead(t *testing.T) {
	resetSessionState(t)
	withAheadSources(t, aheadPieces(true, true, false, true), func(string, int64, int64) int64 { return 0 })
	openAheadSession("/m/a.mkv", 1000, 2, 30, 90*time.Second)
	openAheadSession("/m/unread.mkv", 1000, 2, -1, 5*time.Second)
	openAheadSession("/m/noindex.mkv", 1000, 0, 30, 5*time.Second)

	byPath := map[string]sessionView{}
	for _, v := range openSessionViews(time.Now()) {
		byPath[v.Path] = v
	}
	a := byPath["/m/a.mkv"]
	if a.LastReadOffset == nil || *a.LastReadOffset != 30 || a.AheadBytes == nil || *a.AheadBytes != 120 ||
		a.FrontierOffset == nil || *a.FrontierOffset != 150 || a.SessionAgeSeconds == nil || *a.SessionAgeSeconds < 90 {
		t.Fatalf("read session: %+v", a)
	}
	u := byPath["/m/unread.mkv"]
	if u.LastReadOffset != nil || u.AheadBytes != nil || u.FrontierOffset != nil || u.SessionAgeSeconds == nil {
		t.Fatalf("unread session: %+v", u)
	}
	n := byPath["/m/noindex.mkv"]
	if n.LastReadOffset == nil || n.AheadBytes != nil || n.FrontierOffset != nil {
		t.Fatalf("session without a file index: %+v", n)
	}
}

func TestOpenSessionViewsAheadCap(t *testing.T) {
	resetSessionState(t)
	var gotLimit int64
	oldP, oldC := sessionFilePieces, sessionCacheRun
	sessionFilePieces = func(_ string, _ int, from, limit int64) *native.FilePieceMap {
		gotLimit = limit
		return &native.FilePieceMap{PieceLen: 1 << 20}
	}
	sessionCacheRun = func(string, int64, int64) int64 { return 0 }
	t.Cleanup(func() { sessionFilePieces, sessionCacheRun = oldP, oldC })
	openAheadSession("/m/big.mkv", 50<<30, 1, 10<<20, time.Minute)
	openSessionViews(time.Now())
	if want := int64(10<<20) + aheadScanCap; gotLimit != want {
		t.Fatalf("scan limit %d, want %d", gotLimit, want)
	}
}

func TestMinAheadBytesGrace(t *testing.T) {
	if aheadWarmupGrace != 60*time.Second {
		t.Fatalf("grace is %v, the deployment's alert assumes 60s", aheadWarmupGrace)
	}
	i := func(v int64) *int64 { return &v }
	view := func(age, ahead int64, confirmed bool) sessionView {
		v := sessionView{SessionAgeSeconds: i(age), AheadBytes: i(ahead)}
		v.Confirmed = confirmed
		return v
	}
	if _, ok := minAheadBytes(nil); ok {
		t.Fatal("no sessions must give no value, not 0")
	}
	if _, ok := minAheadBytes([]sessionView{view(59, 1, true)}); ok {
		t.Fatal("a session 59s old is still in its grace period")
	}
	if got, ok := minAheadBytes([]sessionView{view(60, 5, true)}); !ok || got != 5 {
		t.Fatalf("a session 60s old counts: %d %v", got, ok)
	}
	if _, ok := minAheadBytes([]sessionView{view(300, 1, false)}); ok {
		t.Fatal("an unconfirmed session is not a play")
	}
	noAhead := sessionView{SessionAgeSeconds: i(300)}
	noAhead.Confirmed = true
	got, ok := minAheadBytes([]sessionView{view(300, 9, true), view(10, 1, true), noAhead, view(400, 7, true)})
	if !ok || got != 7 {
		t.Fatalf("minimum over the eligible ones: %d %v", got, ok)
	}
}

func TestAheadMetrics(t *testing.T) {
	resetSessionState(t)
	body := scrape(t).Body.String()
	if !strings.Contains(body, "# TYPE flodni_sessions_min_ahead_bytes gauge") ||
		!strings.Contains(body, "# TYPE flodni_session_ahead_bytes histogram") {
		t.Fatalf("families missing:\n%s", body)
	}
	if strings.Contains(body, "\nflodni_sessions_min_ahead_bytes ") {
		t.Fatal("the gauge has a sample with no eligible session")
	}
	if !strings.Contains(body, `flodni_session_ahead_bytes_bucket{le="1048576"} 0`) ||
		!strings.Contains(body, `flodni_session_ahead_bytes_bucket{le="1073741824"} 0`) ||
		!strings.Contains(body, "flodni_session_ahead_bytes_count 0") {
		t.Fatalf("empty histogram:\n%s", body)
	}

	withAheadSources(t, aheadPieces(true, true, true, true, true, true, true, true, true, true, true, true), func(string, int64, int64) int64 { return 0 })
	s := openAheadSession("/m/p.mkv", 1<<30, 1, 0, 2*time.Minute)
	s.confirmed.Store(true)
	// Piece length 100: 12 complete pieces from file offset 0 is 1150 bytes ahead.
	body = scrape(t).Body.String()
	if !strings.Contains(body, "\nflodni_sessions_min_ahead_bytes 1150\n") ||
		!strings.Contains(body, `flodni_session_ahead_bytes_bucket{le="1048576"} 1`) ||
		!strings.Contains(body, "flodni_session_ahead_bytes_sum 1150\n") ||
		!strings.Contains(body, "flodni_session_ahead_bytes_count 1") {
		t.Fatalf("one session:\n%s", body)
	}
}

func TestAheadHistogramBuckets(t *testing.T) {
	want := []float64{1 << 20, 4 << 20, 16 << 20, 32 << 20, 64 << 20, 128 << 20, 256 << 20, 512 << 20, 768 << 20, 1024 << 20}
	h := aheadHistogram(nil)
	if len(h.Bounds) != len(want) || len(h.Counts) != len(want)+1 {
		t.Fatalf("bounds %v counts %v", h.Bounds, h.Counts)
	}
	for k := range want {
		if h.Bounds[k] != want[k] {
			t.Fatalf("bound %d: %v want %v", k, h.Bounds[k], want[k])
		}
	}
}

// The deployment parses /api/sessions: every existing key keeps its value and its place,
// and the new keys come after them.
func TestSessionsJSONContractAhead(t *testing.T) {
	resetSessionState(t)
	withAheadSources(t, aheadPieces(true, true, false, true), func(string, int64, int64) int64 { return 0 })
	s := openAheadSession("/m/a.mkv", 1000, 2, 30, 90*time.Second)
	closedSessions.add(sessionRecord{Path: "/m/old.mkv", Outcome: outcomeServed})

	rec := httptest.NewRecorder()
	handleSessions(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var out struct {
		Open   []map[string]json.RawMessage `json:"open"`
		Closed []map[string]json.RawMessage `json:"closed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	old, _ := json.Marshal(s.record(time.Time{}, false))
	var oldKeys map[string]json.RawMessage
	_ = json.Unmarshal(old, &oldKeys)
	if len(out.Open) != 1 {
		t.Fatalf("open: %d", len(out.Open))
	}
	for k := range oldKeys {
		if _, ok := out.Open[0][k]; !ok {
			t.Fatalf("existing key %q missing", k)
		}
	}
	for _, k := range []string{"session_age_seconds", "last_read_offset", "ahead_bytes", "frontier_offset"} {
		if _, ok := out.Open[0][k]; !ok {
			t.Fatalf("new key %q missing", k)
		}
		if _, ok := out.Closed[0][k]; ok {
			t.Fatalf("closed session carries %q", k)
		}
	}
	body := rec.Body.String()
	if strings.Index(body, `"resumed_from"`) > strings.Index(body, `"ahead_bytes"`) {
		t.Fatal("new keys must come after the existing ones")
	}
	if string(out.Open[0]["ahead_bytes"]) != "120" || string(out.Open[0]["frontier_offset"]) != "150" ||
		string(out.Open[0]["last_read_offset"]) != "30" {
		t.Fatalf("values: %s", body)
	}
	if string(out.Open[0]["id"]) != "1" || string(out.Open[0]["path"]) != `"/m/a.mkv"` {
		t.Fatalf("existing values changed: %s", body)
	}
}
