package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"flodni/internal/warmup"
)

const rbPrefix = "[ReaderBudget] "

// budgetEnv gives a test a fresh session at /m/x.mkv (1 GiB), the given margin and
// no live peers, and restores what it touched.
func budgetEnv(t *testing.T, margin int64) *TTFFSession {
	newTestApp(t)
	t.Helper()
	prevM, prevP := unconfirmedMargin, sessionActivePeers
	unconfirmedMargin = func() int64 { return margin }
	sessionActivePeers = func(string) int { return 0 }
	s := newTestSession(time.Minute)
	t.Cleanup(func() {
		unconfirmedMargin, sessionActivePeers = prevM, prevP
		sessions.Delete(s.path)
		app.playbackRegistry.Delete(s.path)
	})
	return s
}

func confirmPlayback(path string) {
	app.playbackRegistry.Store(path, &PlaybackState{Path: path, OpenedAt: time.Now()})
	v, _ := app.playbackRegistry.Load(path)
	v.(*PlaybackState).SetHealthy(true)
}

func rbCounts() (bytes, unconfirmed, late int64) {
	return app.unconfirmedReadBytes.Load(), app.unconfirmedSessions[0].Load(), app.unconfirmedSessions[1].Load()
}

func TestBytesOutsideWindows(t *testing.T) {
	const size, head, tail = 1000, 100, 50 // middle is [100, 950)
	tests := []struct {
		name         string
		off, n, size int64
		want         int64
	}{
		{"inside the head", 0, 10, size, 0},
		{"straddles the head end", 90, 20, size, 10},
		{"middle", 500, 100, size, 100},
		{"straddles the tail start", 940, 20, size, 10},
		{"inside the tail", 960, 10, size, 0},
		{"whole file", 0, 1000, size, 850},
		{"head and tail overlap (short file)", 0, 120, 120, 0},
		{"zero length", 500, 0, size, 0},
		{"unknown size", 500, 10, 0, 0},
		{"past the end", 2000, 10, size, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bytesOutsideWindows(tt.off, tt.n, tt.size, head, tail); got != tt.want {
				t.Fatalf("bytesOutsideWindows(%d, %d, %d) = %d, want %d", tt.off, tt.n, tt.size, got, tt.want)
			}
		})
	}
}

func TestCountUnconfirmedSourcesAndConfirmation(t *testing.T) {
	s := budgetEnv(t, 1<<40)
	b0, _, _ := rbCounts()

	s.countUnconfirmed(srcWarmupHead, 1<<20, 100<<20)
	s.countUnconfirmed(srcWarmupTail, 1<<20, 100<<20)
	s.countUnconfirmed(srcFetchBlock, 1<<20, 0) // inside the head window
	if b, _, _ := rbCounts(); b != b0 {
		t.Fatalf("SSD sources or window reads counted: +%d", b-b0)
	}

	s.countUnconfirmed(srcFetchBlock, 3<<20, 100<<20)
	s.countUnconfirmed(srcRACacheHit, 2<<20, 200<<20)
	b, _, _ := rbCounts()
	if b-b0 != 5<<20 {
		t.Fatalf("counter +%d, want 5 MiB (swarm and cache together)", b-b0)
	}
	if s.unconfirmedSwarm.Load() != 3<<20 || s.unconfirmedCache.Load() != 2<<20 {
		t.Fatalf("session tally swarm %d cache %d", s.unconfirmedSwarm.Load(), s.unconfirmedCache.Load())
	}

	confirmPlayback(s.path)
	s.countUnconfirmed(srcFetchBlock, 5<<20, 300<<20)
	if b2, _, _ := rbCounts(); b2 != b {
		t.Fatal("a read of a confirmed file counted")
	}

	s.closed.Store(true)
	app.playbackRegistry.Delete(s.path)
	s.recordRead(srcFetchBlock, time.Millisecond, 5<<20, 300<<20)
	if b3, _, _ := rbCounts(); b3 != b {
		t.Fatal("a closed session counted")
	}
}

func TestRecordReadFeedsTheBudget(t *testing.T) {
	s := budgetEnv(t, 1<<40)
	b0, _, _ := rbCounts()
	s.recordRead(srcFetchBlock, time.Millisecond, 4<<20, 100<<20)
	if b, _, _ := rbCounts(); b-b0 != 4<<20 {
		t.Fatalf("recordRead did not reach the budget: +%d", b-b0)
	}
}

func TestCrossingLogsOneLineUnderConcurrency(t *testing.T) {
	s := budgetEnv(t, 10<<20)
	buf := captureSessionLog(t)
	noteReaderCaller(s.path, 0, 1234)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				s.countUnconfirmed(srcFetchBlock, 1<<20, 200<<20)
			}
		}()
	}
	wg.Wait()

	lines := linesWith(buf, rbPrefix)
	if len(lines) != 1 {
		t.Fatalf("%d [ReaderBudget] lines, want 1: %v", len(lines), lines)
	}
	if strings.Contains(lines[0], "x.mkv") || strings.Contains(lines[0], "/m/") {
		t.Fatalf("line carries the path: %s", lines[0])
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[0], rbPrefix)), &rec); err != nil {
		t.Fatalf("line is not JSON: %v\n%s", err, lines[0])
	}
	for _, k := range []string{"event", "hash", "file_index", "size", "swarm_bytes", "cache_bytes",
		"margin", "open_age_ms", "comm", "pid", "uid", "enforced"} {
		if _, ok := rec[k]; !ok {
			t.Errorf("key %q missing: %s", k, lines[0])
		}
	}
	if rec["event"] != "over_margin" || rec["enforced"] != false || rec["margin"] != float64(10<<20) ||
		rec["uid"] != float64(1234) {
		t.Fatalf("unexpected record: %s", lines[0])
	}
	if !s.overUnconfirmedMargin() {
		t.Fatal("overUnconfirmedMargin false after crossing")
	}
	confirmPlayback(s.path)
	if s.overUnconfirmedMargin() {
		t.Fatal("overUnconfirmedMargin true for a confirmed file")
	}
}

func TestBelowMarginThenConfirmedStaysSilent(t *testing.T) {
	s := budgetEnv(t, 10<<20)
	buf := captureSessionLog(t)
	_, u0, l0 := rbCounts()
	s.countUnconfirmed(srcFetchBlock, 5<<20, 200<<20)
	confirmPlayback(s.path)
	noteConfirmedLate(s.path)
	s.countUnconfirmed(srcFetchBlock, 20<<20, 300<<20)
	if s.unconfirmedSwarm.Load() != 5<<20 {
		t.Fatalf("tally kept growing after confirmation: %d", s.unconfirmedSwarm.Load())
	}
	if strings.Contains(buf.String(), rbPrefix) {
		t.Fatalf("a line was written: %s", buf.String())
	}
	s.closeSession()
	if _, u, l := rbCounts(); u != u0 || l != l0 {
		t.Fatal("a session that never crossed was counted at close")
	}
}

func TestConfirmedLateLogsOnceAndSessionsResolveOnceAtClose(t *testing.T) {
	for _, late := range []bool{false, true} {
		name := "unconfirmed"
		if late {
			name = "confirmed_late"
		}
		t.Run(name, func(t *testing.T) {
			s := budgetEnv(t, 10<<20)
			buf := captureSessionLog(t)
			_, u0, l0 := rbCounts()
			s.countUnconfirmed(srcFetchBlock, 12<<20, 200<<20)
			if late {
				confirmPlayback(s.path)
				noteConfirmedLate(s.path)
				noteConfirmedLate(s.path)
			}
			// Nothing is counted before the close.
			if _, u, l := rbCounts(); u != u0 || l != l0 {
				t.Fatal("sessions counted before close")
			}
			wantLines := 1
			if late {
				wantLines = 2
			}
			if got := len(linesWith(buf, rbPrefix)); got != wantLines {
				t.Fatalf("%d lines, want %d:\n%s", got, wantLines, buf)
			}
			if late {
				var rec map[string]any
				l := linesWith(buf, rbPrefix)[1]
				if err := json.Unmarshal([]byte(strings.TrimPrefix(l, rbPrefix)), &rec); err != nil ||
					rec["event"] != "confirmed_late" || rec["swarm_bytes"] != float64(12<<20) ||
					rec["cache_bytes"] != float64(0) {
					t.Fatalf("late line wrong (%v): %s", err, l)
				}
				if _, ok := rec["late_ms"]; !ok {
					t.Fatalf("late_ms missing: %s", l)
				}
			}
			s.closeSession()
			s.closeSession()
			_, u, l := rbCounts()
			wantU, wantL := int64(1), int64(0)
			if late {
				wantU, wantL = 0, 1
			}
			if u-u0 != wantU || l-l0 != wantL {
				t.Fatalf("unconfirmed +%d confirmed_late +%d, want +%d +%d", u-u0, l-l0, wantU, wantL)
			}
		})
	}
}

func webhookCounts() (matched, unmatched int64) {
	return app.playbackWebhooksMatched.Load(), app.playbackWebhooksUnmatched.Load()
}

func TestWebhookCountsMatchedAndUnmatchedPlaysOnly(t *testing.T) {
	playedEnv(t)
	m0, u0 := webhookCounts()
	delta := func() (int64, int64) { m, u := webhookCounts(); return m - m0, u - u0 }

	postWebhook(t, evPayload("media.stop"))
	postWebhook(t, `{"event":"media.play","Metadata":{"title":"Song","librarySectionType":"artist"}}`)
	if m, u := delta(); m != 0 || u != 0 {
		t.Fatalf("stop or non-video play counted: matched +%d unmatched +%d", m, u)
	}
	postWebhook(t, evPayload("media.play"))
	postWebhook(t, evPayload("media.resume"))
	if m, u := delta(); m != 2 || u != 0 {
		t.Fatalf("matched plays: matched +%d unmatched +%d, want +2 +0", m, u)
	}
	postWebhook(t, `{"event":"media.play","Metadata":{"title":"Nothing Registered","librarySectionType":"movie"}}`)
	if m, u := delta(); m != 2 || u != 1 {
		t.Fatalf("unmatched play: matched +%d unmatched +%d, want +2 +1", m, u)
	}
}

func TestWebhookMarksTheCrossedSessionLate(t *testing.T) {
	path, _ := playedEnv(t)
	v, _ := sessions.Load(path)
	s := v.(*TTFFSession)
	prevM, prevP := unconfirmedMargin, sessionActivePeers
	unconfirmedMargin = func() int64 { return 1 << 20 }
	sessionActivePeers = func(string) int { return 0 }
	t.Cleanup(func() { unconfirmedMargin, sessionActivePeers = prevM, prevP })
	buf := captureSessionLog(t)

	s.countUnconfirmed(srcFetchBlock, 2<<20, 200<<20)
	postWebhook(t, evPayload("media.play"))
	postWebhook(t, evPayload("media.resume"))
	var events []string
	for _, l := range linesWith(buf, rbPrefix) {
		var rec struct{ Event string }
		_ = json.Unmarshal([]byte(strings.TrimPrefix(l, rbPrefix)), &rec)
		events = append(events, rec.Event)
	}
	if strings.Join(events, ",") != "over_margin,confirmed_late" {
		t.Fatalf("events = %v", events)
	}
}

func TestCrossingAfterCloseIsStillCountedOnce(t *testing.T) {
	s := budgetEnv(t, 1<<20)
	captureSessionLog(t)
	_, u0, _ := rbCounts()
	s.closeSession() // resolves with nothing crossed
	s.closed.Store(true)
	s.countUnconfirmed(srcFetchBlock, 2<<20, 200<<20) // a read that passed the closed check earlier
	s.resolveReaderBudget()
	if _, u, _ := rbCounts(); u-u0 != 1 {
		t.Fatalf("unconfirmed +%d, want 1", u-u0)
	}
}

func TestReadInnerCacheHitReachesTheBudget(t *testing.T) {
	useFreshReadPath(t)
	const path = "/test/budget-wire.mkv"
	prevM := unconfirmedMargin
	unconfirmedMargin = func() int64 { return 1 << 40 }
	t.Cleanup(func() { unconfirmedMargin = prevM; sessions.Delete(path) })
	ttffRegister(path, 10*gb, "abcdef0123", 1, true, true)
	start := int64(96) * mb
	raCache.Put(path, start, start+16*mb-1, pattern(int(16*mb)))
	h := readHandle(path, 10*gb)
	b0, _, _ := rbCounts()
	if _, errno := h.Read(context.Background(), make([]byte, 4096), start+mb); errno != 0 {
		t.Fatalf("errno = %v", errno)
	}
	if b, _, _ := rbCounts(); b-b0 != 4096 {
		t.Fatalf("counter +%d, want 4096", b-b0)
	}
}

// The tail window starts on a page boundary, so the page that straddles size-N is inside
// it: reading it counts nothing and is not refused.
func TestReaderBudgetTailExemptionUsesTheAlignedStart(t *testing.T) {
	s := budgetEnv(t, 0)
	s.size = 1<<30 + 1000
	start := warmup.TailStart(s.size)
	if start >= s.size-warmup.TailWarmupSize {
		t.Fatalf("TailStart %d is not below size-window %d", start, s.size-warmup.TailWarmupSize)
	}
	b0, _, _ := rbCounts()
	s.countUnconfirmed(srcFetchBlock, 1000, start) // the leading page of the window
	if b, _, _ := rbCounts(); b != b0 {
		t.Fatalf("a read inside the aligned tail window counted +%d", b-b0)
	}
	if s.blocks(start, 1000, s.size) {
		t.Fatal("a read inside the aligned tail window was refused")
	}
	// One page below the window is outside.
	s.countUnconfirmed(srcFetchBlock, 4096, start-4096)
	if b, _, _ := rbCounts(); b-b0 != 4096 {
		t.Fatalf("a read below the window counted +%d, want 4096", b-b0)
	}
}
