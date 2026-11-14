package daemon

import (
	"encoding/json"
	"flodni/internal/gostorm/native"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSessionOutcome(t *testing.T) {
	tests := []struct {
		name       string
		dead       bool
		bytes      int64
		tailOrDeep bool
		want       string
	}{
		{"dead swarm wins over bytes", true, 38, false, outcomeDeadSwarm},
		{"dead swarm, nothing served", true, 0, false, outcomeDeadSwarm},
		{"nothing served", false, 0, false, outcomeUnserved},
		{"1MB is a real read", false, ttffProbeMinRead, false, outcomeServed},
		{"deep read over 64KB is real", false, ttffTailMinRead + 1, true, outcomeServed},
		{"deep read of exactly 64KB is a probe", false, ttffTailMinRead, true, outcomeProbe},
		{"small header read is a probe", false, 4096, false, outcomeProbe},
		{"64KB+ without deep read is a probe", false, ttffTailMinRead + 1, false, outcomeProbe},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sessionOutcome(tt.dead, tt.bytes, tt.tailOrDeep); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSessionRecord(t *testing.T) {
	opened := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	s := &TTFFSession{path: "/m/a.mkv", size: 100, hash: "h"}
	s.openedAt.Store(opened.UnixNano())

	r := s.record(time.Time{}, false)
	if r.Outcome != outcomeOpen || r.ClosedAt != "" || r.FirstByteMS != nil {
		t.Fatalf("open, unread: %+v", r)
	}

	s.firstDataAt.Store(opened.Add(250 * time.Millisecond).UnixNano())
	s.bytesRead.Store(2 << 20)
	s.reads.Store(3)
	s.failedReads.Store(1)
	s.fileIndex = 4
	s.bytesFetchBlock.Store(1 << 20)
	s.bytesRACacheHit.Store(1 << 20)
	s.servedByNet.Store(true)
	reason := "timeout"
	s.failReason.Store(&reason)
	r = s.record(opened.Add(time.Minute), false)
	if r.Outcome != outcomeServed || r.OpenedAt != "2026-11-11T10:00:00Z" ||
		r.ClosedAt != "2026-11-11T10:01:00Z" || r.FirstByteMS == nil || *r.FirstByteMS != 250 ||
		r.BytesServed != 2<<20 || r.Reads != 3 || r.FailedReads != 1 {
		t.Fatalf("closed: %+v", r)
	}
	if r.FileIndex != 4 || r.BytesFetchBlock != 1<<20 || r.BytesRACacheHit != 1<<20 ||
		!r.SwarmServed || r.DurationMS != 60000 || r.FailureReason == nil || *r.FailureReason != "timeout" {
		t.Fatalf("closed, new fields: %+v", r)
	}
}

func TestSessionRingWrapsKeepsNewest(t *testing.T) {
	var ring sessionRing
	for i := 0; i < sessionRingSize+25; i++ {
		ring.add(sessionRecord{Path: fmt.Sprint(i)})
	}
	got := ring.newestFirst()
	if len(got) != sessionRingSize {
		t.Fatalf("len %d, want %d", len(got), sessionRingSize)
	}
	if got[0].Path != fmt.Sprint(sessionRingSize+24) || got[len(got)-1].Path != "25" {
		t.Fatalf("newest %s oldest %s", got[0].Path, got[len(got)-1].Path)
	}
}

func TestSessionRingPartial(t *testing.T) {
	var ring sessionRing
	if n := len(ring.newestFirst()); n != 0 {
		t.Fatalf("empty ring returned %d", n)
	}
	ring.add(sessionRecord{Path: "a"})
	ring.add(sessionRecord{Path: "b"})
	got := ring.newestFirst()
	if len(got) != 2 || got[0].Path != "b" || got[1].Path != "a" {
		t.Fatalf("got %+v", got)
	}
}

func TestFilterSessions(t *testing.T) {
	in := []sessionRecord{
		{Path: "1", Outcome: outcomeServed},
		{Path: "2", Outcome: outcomeUnserved},
		{Path: "3", Outcome: outcomeServed},
		{Path: "4", Outcome: outcomeServed},
	}
	tests := []struct {
		name    string
		outcome string
		limit   int
		want    string
	}{
		{"no filter", "", 10, "1,2,3,4"},
		{"filter", outcomeServed, 10, "1,3,4"},
		{"limit", "", 2, "1,2"},
		{"filter and limit", outcomeServed, 2, "1,3"},
		{"no match", outcomeProbe, 10, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var paths []string
			for _, r := range filterSessions(in, tt.outcome, tt.limit) {
				paths = append(paths, r.Path)
			}
			if got := strings.Join(paths, ","); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// resetSessionState isolates the package-level registry and ring for a test.
func resetSessionState(t *testing.T) {
	t.Helper()
	clear := func() {
		sessions.Range(func(k, _ any) bool { sessions.Delete(k); return true })
		lastClosed.Range(func(k, _ any) bool { lastClosed.Delete(k); return true })
		closedSessions.mu.Lock()
		closedSessions.next, closedSessions.n = 0, 0
		closedSessions.mu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func TestHandleSessions(t *testing.T) {
	resetSessionState(t)
	open := &TTFFSession{path: "/m/open.mkv", hash: "h1"}
	open.openedAt.Store(time.Now().UnixNano())
	sessions.Store(open.path, open)
	closedSessions.add(sessionRecord{Path: "/m/old.mkv", Outcome: outcomeUnserved})
	closedSessions.add(sessionRecord{Path: "/m/new.mkv", Outcome: outcomeServed})

	type resp struct {
		Open   []sessionRecord `json:"open"`
		Closed []sessionRecord `json:"closed"`
	}
	get := func(query string) (*httptest.ResponseRecorder, resp) {
		rec := httptest.NewRecorder()
		handleSessions(rec, httptest.NewRequest(http.MethodGet, "/api/sessions"+query, nil))
		var out resp
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("invalid JSON: %v: %s", err, rec.Body)
			}
		}
		return rec, out
	}

	rec, out := get("")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if len(out.Open) != 1 || out.Open[0].Outcome != outcomeOpen || out.Open[0].ClosedAt != "" ||
		len(out.Closed) != 2 || out.Closed[0].Path != "/m/new.mkv" {
		t.Fatalf("unexpected: %+v", out)
	}
	if _, out = get("?limit=1"); len(out.Closed) != 1 || out.Closed[0].Path != "/m/new.mkv" {
		t.Fatalf("limit: %+v", out)
	}
	if _, out = get("?outcome=unserved"); len(out.Open) != 0 || len(out.Closed) != 1 || out.Closed[0].Path != "/m/old.mkv" {
		t.Fatalf("outcome filter: %+v", out)
	}
	if _, out = get("?outcome=open"); len(out.Open) != 1 || len(out.Closed) != 0 {
		t.Fatalf("open filter: %+v", out)
	}
	if _, out = get("?limit=9999"); len(out.Closed) != 2 {
		t.Fatalf("limit clamp: %+v", out)
	}
	for _, q := range []string{"?limit=0", "?limit=x", "?outcome=bogus"} {
		if rec, _ = get(q); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d", q, rec.Code)
		}
	}
}

func TestHandleSessionsMethod(t *testing.T) {
	rec := httptest.NewRecorder()
	handleSessions(rec, httptest.NewRequest(http.MethodPost, "/api/sessions", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", rec.Code)
	}
}

func TestCloseSessionOutcome(t *testing.T) {
	tests := []struct {
		name    string
		verdict bool // ReachabilityOutcome injected
		age     time.Duration
		fail    bool
		want    string
	}{
		{"failed read, long enough: dead swarm", true, time.Minute, true, outcomeDeadSwarm},
		{"failed read, below the floor", true, 5 * time.Second, true, outcomeUnserved},
		{"no failed read", true, time.Minute, false, outcomeUnserved},
		{"verdict not wired", false, time.Minute, true, outcomeUnserved},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetSessionState(t)
			prev := native.ReachabilityOutcome
			t.Cleanup(func() { native.ReachabilityOutcome = prev })
			native.ReachabilityOutcome = nil
			if tt.verdict {
				native.ReachabilityOutcome = func(native.Outcome) {}
			}
			s := &TTFFSession{path: "/m/x.mkv", size: 1, hash: "h"}
			s.openedAt.Store(time.Now().Add(-tt.age).UnixNano())
			s.lastOff.Store(-1)
			sessions.Store(s.path, s)
			s.recordRead(srcWarmupHead, time.Millisecond, 0, 0)
			if tt.fail {
				ttffReadFailed(s.path)
			}
			s.closeSession()
			got := closedSessions.newestFirst()
			if len(got) != 1 || got[0].Outcome != tt.want || got[0].Reads != 1 {
				t.Fatalf("got %+v, want outcome %q", got, tt.want)
			}
			if tt.fail && got[0].FailedReads != 1 {
				t.Fatalf("failed_reads %d", got[0].FailedReads)
			}
		})
	}
}

// The new session fields: stalls and the slowest one come from the stall tracking,
// reopens from the read path, and confirmed is sticky.
func TestSessionRecordKeepaliveFields(t *testing.T) {
	resetKeepaliveState(t)
	s := &TTFFSession{path: "/m/k.mkv", size: 100, hash: kaHashA}
	s.openedAt.Store(time.Now().UnixNano())
	sessions.Store(s.path, s)
	playbackRegistry.Store(s.path, &PlaybackState{Path: s.path, Hash: kaHashA})

	s.recordRead(srcFetchBlock, 2500*time.Millisecond, 4096, 0)
	s.recordRead(srcFetchBlock, 1500*time.Millisecond, 4096, 4096)
	countReopen(s.path, kaHashA)
	r := s.record(time.Time{}, false)
	if r.Stalls != 2 || r.MaxStallMS != 2500 || r.Reopens != 1 || r.Confirmed {
		t.Fatalf("record = %+v", r)
	}

	// A webhook confirms the path while the session is open: the flag sticks, also
	// after a stop webhook clears the state.
	ps, _ := playbackRegistry.Load(s.path)
	ps.(*PlaybackState).SetHealthy(true)
	noteConfirmedLate(s.path)
	ps.(*PlaybackState).IsHealthy = false
	ps.(*PlaybackState).IsStopped = true
	if r = s.record(time.Time{}, false); !r.Confirmed {
		t.Fatalf("confirmed not sticky: %+v", r)
	}
}

// A session registered for a path that is already confirmed starts confirmed.
func TestSessionConfirmedAtRegister(t *testing.T) {
	resetKeepaliveState(t)
	playbackRegistry.Store("/m/c.mkv", &PlaybackState{Path: "/m/c.mkv", Hash: kaHashA, IsHealthy: true})
	ttffRegister("/m/c.mkv", 100, kaHashA, 0, false, false)
	ttffRegister("/m/u.mkv", 100, kaHashB, 0, false, false)
	c, _ := sessions.Load("/m/c.mkv")
	u, _ := sessions.Load("/m/u.mkv")
	if !c.(*TTFFSession).record(time.Time{}, false).Confirmed || u.(*TTFFSession).record(time.Time{}, false).Confirmed {
		t.Fatal("confirmed at register wrong")
	}
}
