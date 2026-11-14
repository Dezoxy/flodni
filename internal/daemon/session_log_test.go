package daemon

import (
	"bytes"
	"encoding/json"
	"log"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// captureSessionLog redirects logger into a buffer for the test. readabilityEnv
// (or its own cleanup) restores it.
func captureSessionLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev := logger
	t.Cleanup(func() { logger = prev })
	var buf bytes.Buffer
	logger = log.New(&buf, "", 0)
	return &buf
}

func linesWith(buf *bytes.Buffer, prefix string) []string {
	var out []string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return out
}

func TestRecordReadCountsBytesPerSource(t *testing.T) {
	readabilityEnv(t, 0, false)
	s := newTestSession(time.Minute)
	for _, r := range []struct {
		src ttffSource
		n   int
	}{
		{srcFetchBlock, 100}, {srcFetchBlock, 0}, {srcRACacheHit, 2000},
		{srcWarmupHead, 30000}, {srcWarmupTail, 400000}, {srcRACacheHit, 5},
	} {
		s.recordRead(r.src, time.Millisecond, r.n, 0)
	}
	got := []int64{s.bytesFetchBlock.Load(), s.bytesRACacheHit.Load(),
		s.bytesWarmupHead.Load(), s.bytesWarmupTail.Load()}
	if want := []int64{100, 2005, 30000, 400000}; !reflect.DeepEqual(got, want) {
		t.Fatalf("per source = %v, want %v", got, want)
	}
	if sum := got[0] + got[1] + got[2] + got[3]; sum != s.bytesRead.Load() {
		t.Fatalf("sum %d != bytesRead %d", sum, s.bytesRead.Load())
	}
	s.closed.Store(true)
	s.recordRead(srcFetchBlock, time.Millisecond, 999, 0)
	if s.bytesFetchBlock.Load() != 100 {
		t.Fatal("a closed session counted a read")
	}
}

func TestSwarmServedNeedsPeers(t *testing.T) {
	for _, tt := range []struct {
		peers int
		src   ttffSource
		want  bool
	}{{2, srcFetchBlock, true}, {0, srcFetchBlock, false}, {2, srcRACacheHit, false}} {
		readabilityEnv(t, tt.peers, true)
		s := newTestSession(time.Minute)
		s.recordRead(tt.src, time.Millisecond, 10, 0)
		if got := s.record(time.Time{}, false).SwarmServed; got != tt.want {
			t.Fatalf("peers=%d src=%v: swarm_served = %v, want %v", tt.peers, tt.src, got, tt.want)
		}
	}
}

func TestSessionLogLineOncePerSessionAndMatchesRing(t *testing.T) {
	readabilityEnv(t, 1, true)
	buf := captureSessionLog(t)
	s := newTestSession(time.Minute)
	s.fileIndex = 2
	s.recordRead(srcFetchBlock, time.Millisecond, 2<<20, 0)
	s.closeSession()
	s.closeSession()

	lines := linesWith(buf, sessionLogPrefix)
	if len(lines) != 1 {
		t.Fatalf("got %d [Session] lines, want 1: %q", len(lines), buf.String())
	}
	ring, err := json.Marshal(closedSessions.newestFirst()[0])
	if err != nil {
		t.Fatal(err)
	}
	if payload := strings.TrimPrefix(lines[0], sessionLogPrefix); payload != string(ring) {
		t.Fatalf("log payload differs from the ring record:\n%s\n%s", payload, ring)
	}
	var rec sessionRecord
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[0], sessionLogPrefix)), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.FileIndex != 2 || rec.Hash != rHash || rec.Outcome != outcomeServed ||
		rec.BytesFetchBlock != 2<<20 || !rec.SwarmServed || rec.DurationMS < 60000 {
		t.Fatalf("record = %+v", rec)
	}
}

func TestSessionLogContractKeys(t *testing.T) {
	line, err := json.Marshal(sessionRecord{})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"bytes_fetch_block", "bytes_racache_hit", "bytes_served", "bytes_warmup_head",
		"bytes_warmup_tail", "confirmed", "duration_ms", "failed_reads", "failure_reason", "file_index",
		"first_byte_ms", "hash", "id", "max_stall_ms", "opened_at", "outcome", "path", "reads", "reopens",
		"resumed_from", "size", "stalls", "swarm_served"}
	// closed_at is omitted while a session is open; the log line always has it.
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
}

func TestSessionLogOutcomes(t *testing.T) {
	cases := []struct {
		name  string
		setup func(s *TTFFSession)
		close func(s *TTFFSession)
		want  string
		ttff  bool
	}{
		{"served", func(s *TTFFSession) { s.recordRead(srcFetchBlock, time.Millisecond, 2<<20, 0) },
			(*TTFFSession).closeSession, outcomeServed, true},
		{"probe", func(s *TTFFSession) { s.recordRead(srcWarmupHead, time.Millisecond, 4096, 0) },
			(*TTFFSession).closeSession, outcomeProbe, false},
		{"unserved", func(*TTFFSession) {},
			func(s *TTFFSession) { ttffReleaseClose(s.path) }, outcomeUnserved, false},
		{"dead_swarm", func(s *TTFFSession) { s.readFailed.Store(true) },
			(*TTFFSession).closeSession, outcomeDeadSwarm, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			readabilityEnv(t, 0, false)
			buf := captureSessionLog(t)
			s := newTestSession(time.Minute)
			tt.setup(s)
			tt.close(s)
			lines := linesWith(buf, sessionLogPrefix)
			if len(lines) != 1 || !strings.Contains(lines[0], `"outcome":"`+tt.want+`"`) {
				t.Fatalf("lines = %q, want one with outcome %s", lines, tt.want)
			}
			if got := len(linesWith(buf, "[TTFF]")); (got == 1) != tt.ttff {
				t.Fatalf("[TTFF] lines = %d, want present=%v", got, tt.ttff)
			}
		})
	}
}

func TestOldSessionLinesUnchanged(t *testing.T) {
	readabilityEnv(t, 0, false)
	buf := captureSessionLog(t)
	// Served session: only the fields the [TTFF] line prints are set.
	s := newTestSession(time.Minute)
	s.size = 1 << 30
	s.warmupHeadReady.Store(true)
	s.firstDataAt.Store(s.openedAt.Load() + int64(250*time.Millisecond))
	s.bytesRead.Store(2 << 20)
	s.closeSession()
	if got := linesWith(buf, "[TTFF]"); len(got) != 1 ||
		got[0] != "[TTFF] path=x.mkv head=1 tail=0 header=250ms deepread=- seek=0 stalls=0 bytes=2097152" {
		t.Fatalf("[TTFF] line = %q", got)
	}

	// Dead swarm: a failed read, nothing served, long enough to count.
	buf.Reset()
	d := newTestSession(time.Minute)
	d.readFailed.Store(true)
	d.closeSession()
	want := "[DeadSwarm] session of x.mkv ended after 1m0s with a failed read and nothing served"
	if got := linesWith(buf, "[DeadSwarm]"); len(got) != 1 || got[0] != want {
		t.Fatalf("[DeadSwarm] line = %q, want %q", got, want)
	}
}

func TestSessionLogLineIsOneLine(t *testing.T) {
	buf := captureSessionLog(t)
	logSessionRecord(sessionRecord{Path: "/m/a\nb \"q\".mkv", Hash: "h"})
	out := strings.TrimSuffix(buf.String(), "\n")
	if strings.Contains(out, "\n") {
		t.Fatalf("log line spans lines: %q", out)
	}
	var rec sessionRecord
	if err := json.Unmarshal([]byte(strings.TrimPrefix(out, sessionLogPrefix)), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Path != "/m/a\nb \"q\".mkv" {
		t.Fatalf("path = %q", rec.Path)
	}
}

func TestFileIndexStoredAndKeptOnReopen(t *testing.T) {
	resetSessionState(t)
	ttffRegister("/m/f.mkv", 100, rHash, 3, false, false)
	ttffRegister("/m/f.mkv", 100, rHash, 5, true, true)
	val, ok := sessions.Load("/m/f.mkv")
	if !ok {
		t.Fatal("session not registered")
	}
	if got := val.(*TTFFSession).fileIndex; got != 3 {
		t.Fatalf("fileIndex = %d, want 3", got)
	}
}

func TestSessionLogFailureReason(t *testing.T) {
	readabilityEnv(t, 3, true)
	buf := captureSessionLog(t)
	s := newTestSession(time.Minute)
	ttffReadFailed(s.path)
	s.recordRead(srcFetchBlock, time.Millisecond, 1<<20, 0)
	s.closeSession()
	lines := linesWith(buf, sessionLogPrefix)
	if len(lines) != 1 || !strings.Contains(lines[0], `"failure_reason":"timeout"`) ||
		!strings.Contains(lines[0], `"swarm_served":true`) || !strings.Contains(lines[0], `"outcome":"served"`) {
		t.Fatalf("acquitted session: %q", lines)
	}
	buf.Reset()
	newTestSession(time.Minute).closeSession()
	if lines = linesWith(buf, sessionLogPrefix); len(lines) != 1 || !strings.Contains(lines[0], `"failure_reason":null`) {
		t.Fatalf("no failed read: %q", lines)
	}
}
