package scrape

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type logBuf struct {
	mu    sync.Mutex
	lines []string
}

func (l *logBuf) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, sprintf(format, args...))
}
func (l *logBuf) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func newTest(t *testing.T, targets []Target, mod func(*Config)) (*Scraper, *logBuf) {
	t.Helper()
	lb := &logBuf{}
	cfg := Config{
		Source:      func(only *[20]byte) []Target { return filterTargets(targets, only) },
		Pace:        time.Millisecond,
		Timeout:     10 * time.Second, // a liveness bound; tests that need a timeout set their own
		BackoffBase: time.Millisecond,
		BackoffMax:  10 * time.Millisecond,
		Logf:        lb.logf,
	}
	if mod != nil {
		mod(&cfg)
	}
	s := New(cfg)
	t.Cleanup(s.Stop)
	return s, lb
}

func waitDone(t *testing.T, s *Scraper) Pass {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if l, _ := s.Status(); l != nil && l.State == "done" {
			return *l
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("pass did not finish")
	return Pass{}
}

func TestPassAnswersAndBestAcrossTrackers(t *testing.T) {
	a, b := h20(1), h20(2)
	live := newFakeHTTP(t, map[[20]byte]Counts{a: {9, 3}, b: {0, 4}})
	low := newFakeHTTP(t, map[[20]byte]Counts{a: {2, 8}})
	s, _ := newTest(t, []Target{
		{Hash: a, Trackers: []string{low.announce(), live.announce()}},
		{Hash: b, Trackers: []string{live.announce()}},
	}, nil)
	p, err := s.StartPass("manual", nil)
	if err != nil || p.RunID != 1 {
		t.Fatalf("start = %+v, %v", p, err)
	}
	done := waitDone(t, s)
	if done.OK != 2 || done.Errors != 0 || done.Torrents != 2 {
		t.Fatalf("pass = %+v", done)
	}
	ea, _ := s.Entry(a)
	if ea.Seeders == nil || *ea.Seeders != 9 || *ea.Leechers != 3 || ea.Err != "" || ea.RunID != 1 {
		t.Fatalf("best answer = %+v", ea)
	}
	// A tracker that really answers 0 seeders gives 0, not null.
	eb, _ := s.Entry(b)
	if eb.Seeders == nil || *eb.Seeders != 0 || *eb.Leechers != 4 {
		t.Fatalf("a real 0 must stay 0: %+v", eb)
	}
	// both hashes on one host went in one request
	if n := len(live.requests()); n != 1 || len(live.requests()[0].hashes) != 2 {
		t.Fatalf("live requests = %+v", live.requests())
	}
	if z, f, m := s.SeederBuckets(); z != 1 || f != 0 || m != 1 {
		t.Fatalf("buckets = %d %d %d", z, f, m)
	}
}

func TestFailuresGiveNullCountsNeverZero(t *testing.T) {
	t.Parallel() // spends hangTimeout waiting for the hung tracker
	hang := hangTracker(t)
	bad := newFakeHTTP(t, nil)
	bad.answer = func(w http.ResponseWriter, r *http.Request, _ [][20]byte) { http.Error(w, "x", 503) }
	live := newFakeHTTP(t, map[[20]byte]Counts{h20(3): {6, 1}})
	hh, hb, hl := h20(1), h20(2), h20(3)
	s, _ := newTest(t, []Target{
		{Hash: hh, Trackers: []string{hang.announce()}},
		{Hash: hb, Trackers: []string{bad.announce()}},
		{Hash: hl, Trackers: []string{bad.announce(), live.announce()}}, // one dead, one live
	}, func(c *Config) { c.Timeout = hangTimeout })
	if _, err := s.StartPass("manual", nil); err != nil {
		t.Fatal(err)
	}
	p := waitDone(t, s)
	if p.OK != 1 || p.Errors != 2 {
		t.Fatalf("pass = %+v", p)
	}
	for _, c := range []struct {
		h    [20]byte
		host string
		want string
	}{{hh, hang.host(), "timeout"}, {hb, bad.host(), "http status 503"}} {
		e, ok := s.Entry(c.h)
		if !ok || e.Seeders != nil || e.Leechers != nil || e.RunID != 1 {
			t.Fatalf("failed entry must have null counts and the run id: %+v", e)
		}
		if e.Err != c.host+": "+c.want {
			t.Fatalf("error = %q, want host and class %q", e.Err, c.host+": "+c.want)
		}
	}
	e, _ := s.Entry(hl)
	if e.Seeders == nil || *e.Seeders != 6 || e.Err != "" {
		t.Fatalf("one dead and one live tracker must take the live answer: %+v", e)
	}
}

func TestPartialAnswerIsAnErrorNotZero(t *testing.T) {
	a, b := h20(1), h20(2)
	f := newFakeHTTP(t, map[[20]byte]Counts{a: {1, 1}}) // b omitted
	s, _ := newTest(t, []Target{{Hash: a, Trackers: []string{f.announce()}}, {Hash: b, Trackers: []string{f.announce()}}}, nil)
	_, _ = s.StartPass("manual", nil)
	waitDone(t, s)
	if e, _ := s.Entry(b); e.Seeders != nil || e.Err == "" {
		t.Fatalf("omitted hash: %+v", e)
	}
	if e, _ := s.Entry(a); e.Seeders == nil || *e.Seeders != 1 {
		t.Fatalf("answered hash: %+v", e)
	}
}

func TestUnreachedTorrentKeepsOldEntry(t *testing.T) {
	a, b, c := h20(1), h20(2), h20(3)
	f := newFakeHTTP(t, map[[20]byte]Counts{a: {5, 5}, b: {6, 6}, c: {7, 7}})
	targets := []Target{{Hash: a, Trackers: []string{f.announce()}}, {Hash: b, Trackers: []string{f.announce()}}}
	s, _ := newTest(t, targets, nil)
	_, _ = s.StartPass("manual", nil)
	waitDone(t, s)

	// pass 2 over b only: a is not reached and keeps its run 1 entry
	if _, err := s.StartPass("manual", &b); err != nil {
		t.Fatal(err)
	}
	waitDone(t, s)
	ea, _ := s.Entry(a)
	if ea.RunID != 1 || *ea.Seeders != 5 {
		t.Fatalf("unreached torrent: %+v", ea)
	}
	if eb, _ := s.Entry(b); eb.RunID != 2 {
		t.Fatalf("scraped torrent: %+v", eb)
	}

	// a torrent added mid-pass is not in the pass: no entry, nothing written
	targets = append(targets, Target{Hash: c, Trackers: []string{f.announce()}})
	if _, ok := s.Entry(c); ok {
		t.Fatal("no entry before a pass reaches it")
	}

	// a failing host in pass 3 does not leave a fresh zero over an old answer
	f.answer = func(w http.ResponseWriter, r *http.Request, _ [][20]byte) { http.Error(w, "x", 500) }
	_, _ = s.StartPass("manual", &a)
	waitDone(t, s)
	e, _ := s.Entry(a)
	if e.Seeders != nil || e.RunID != 3 || e.Err == "" {
		t.Fatalf("failed pass replaces numbers with an error, never a zero: %+v", e)
	}
}

func TestStoppedPassLeavesEntriesAlone(t *testing.T) {
	a := h20(1)
	f := newFakeHTTP(t, map[[20]byte]Counts{a: {5, 5}})
	s, _ := newTest(t, []Target{{Hash: a, Trackers: []string{f.announce()}}}, nil)
	_, _ = s.StartPass("manual", nil)
	waitDone(t, s)
	hang := newFakeHTTP(t, nil)
	hang.answer = func(w http.ResponseWriter, r *http.Request, _ [][20]byte) { time.Sleep(time.Second) }
	s.cfg.Source = func(*[20]byte) []Target { return []Target{{Hash: a, Trackers: []string{hang.announce()}}} }
	_, _ = s.StartPass("manual", nil)
	time.Sleep(50 * time.Millisecond)
	s.Stop()
	e, _ := s.Entry(a)
	if e.RunID != 1 || *e.Seeders != 5 {
		t.Fatalf("a cancelled pass must not write: %+v", e)
	}
	if p := s.LastFullPass(); p == nil || p.RunID != 1 {
		t.Fatalf("a cancelled pass is not the last full pass: %+v", p)
	}
}

func TestSkippedTrackersAreCounted(t *testing.T) {
	a, b := h20(1), h20(2)
	f := newFakeHTTP(t, map[[20]byte]Counts{b: {1, 1}})
	s, _ := newTest(t, []Target{
		{Hash: a, Trackers: []string{"http://tracker.example.org/tracker.php", "wss://tracker.example.org/announce"}},
		{Hash: b, Trackers: []string{f.announce()}},
	}, nil)
	_, _ = s.StartPass("manual", nil)
	p := waitDone(t, s)
	if p.Skipped != 1 || p.OK != 1 {
		t.Fatalf("pass = %+v", p)
	}
	if _, _, sk := s.Counts(); sk != 2 {
		t.Fatalf("skipped trackers = %d, want 2", sk)
	}
	if _, ok := s.Entry(a); ok {
		t.Fatal("a torrent with nothing to scrape keeps no (and gets no) entry")
	}
}

func TestPerHostIsolation(t *testing.T) {
	hang := newFakeHTTP(t, nil)
	release := make(chan struct{})
	hang.answer = func(w http.ResponseWriter, r *http.Request, _ [][20]byte) { <-release }
	t.Cleanup(func() { close(release) })
	live := newFakeHTTP(t, map[[20]byte]Counts{h20(2): {3, 3}})
	s, _ := newTest(t, []Target{
		{Hash: h20(1), Trackers: []string{hang.announce()}},
		{Hash: h20(2), Trackers: []string{live.announce()}},
	}, func(c *Config) { c.Timeout = time.Minute }) // the hung host stays hung for the whole test
	_, _ = s.StartPass("manual", nil)
	deadline := time.Now().Add(10 * time.Second) // a liveness bound, not a speed expectation
	for time.Now().Before(deadline) {
		if _, ok := s.Entry(h20(2)); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := s.Entry(h20(2)); !ok {
		t.Fatal("the live host was held up by the hanging one")
	}
	if !s.Running() {
		t.Fatal("the pass should still be running on the hanging host")
	}
}

func TestPassOverlapAndStatus(t *testing.T) {
	a := h20(1)
	gate := make(chan struct{})
	f := newFakeHTTP(t, map[[20]byte]Counts{a: {1, 1}})
	f.answer = func(w http.ResponseWriter, r *http.Request, hs [][20]byte) {
		<-gate
		_, _ = w.Write(benFiles(map[[20]byte]Counts{a: {1, 1}}))
	}
	s, _ := newTest(t, []Target{{Hash: a, Trackers: []string{f.announce()}}}, func(c *Config) { c.Timeout = 5 * time.Second })
	p, err := s.StartPass("manual", nil)
	if err != nil || p.RunID != 1 || p.State != "running" {
		t.Fatalf("%+v %v", p, err)
	}
	running, err := s.StartPass("manual", nil)
	if err != ErrRunning || running.RunID != 1 {
		t.Fatalf("second start while running: %+v %v", running, err)
	}
	if l, prev := s.Status(); l.State != "running" || prev != nil || !l.FinishedAt.IsZero() {
		t.Fatalf("status while running: %+v %+v", l, prev)
	}
	if s.LastFullPass() != nil {
		t.Fatal("no finished pass yet")
	}
	close(gate)
	done := waitDone(t, s)
	if done.FinishedAt.IsZero() || done.OK != 1 {
		t.Fatalf("%+v", done)
	}
	if lf := s.LastFullPass(); lf == nil || lf.RunID != 1 {
		t.Fatalf("last full pass = %+v", lf)
	}
	p2, err := s.StartPass("manual", nil)
	if err != nil || p2.RunID != 2 {
		t.Fatalf("next pass: %+v %v", p2, err)
	}
	waitDone(t, s)
	l, prev := s.Status()
	if l.RunID != 2 || prev == nil || prev.RunID != 1 {
		t.Fatalf("latest %+v previous %+v", l, prev)
	}
	if _, err := s.StartPass("manual", &[20]byte{9}); err != ErrUnknownHash {
		t.Fatalf("unknown hash: %v", err)
	}
	if lf := s.LastFullPass(); lf.RunID != 2 {
		t.Fatalf("a single-torrent pass must not count as a full pass: %+v", lf)
	}
}

func TestIntervalZeroHasNoScheduleButManualWorks(t *testing.T) {
	a := h20(1)
	f := newFakeHTTP(t, map[[20]byte]Counts{a: {1, 1}})
	s, _ := newTest(t, []Target{{Hash: a, Trackers: []string{f.announce()}}}, func(c *Config) { c.Interval = 0 })
	s.Start()
	time.Sleep(100 * time.Millisecond)
	if len(f.requests()) != 0 {
		t.Fatal("interval 0 must not run a scheduled pass")
	}
	if _, err := s.StartPass("manual", nil); err != nil {
		t.Fatal(err)
	}
	waitDone(t, s)
	if len(f.requests()) != 1 {
		t.Fatal("manual pass must work")
	}
}

func TestSchedulerRunsPassesWithJitter(t *testing.T) {
	a := h20(1)
	f := newFakeHTTP(t, map[[20]byte]Counts{a: {1, 1}})
	var mu sync.Mutex
	var draws []float64
	s, _ := newTest(t, []Target{{Hash: a, Trackers: []string{f.announce()}}}, func(c *Config) {
		c.Interval = 100 * time.Millisecond
		c.HostJitter = 20 * time.Millisecond
		c.Rand = func() float64 { mu.Lock(); defer mu.Unlock(); draws = append(draws, 1.0); return 1.0 }
	})
	s.Start()
	deadline := time.Now().Add(30 * time.Second) // a liveness bound, not a speed expectation
	for time.Now().Before(deadline) && len(f.requests()) < 5 {
		time.Sleep(10 * time.Millisecond)
	}
	reqs := f.requests()
	if len(reqs) < 5 {
		t.Fatalf("scheduled passes ran %d times", len(reqs))
	}
	// The wait between passes is never below the bottom of the +/-10 % window (90 ms),
	// whatever Rand says. Request times carry the machine's scheduling noise, so this
	// checks the floor over four intervals and has no upper bound: a loaded machine
	// only makes the passes later.
	gap := reqs[4].at.Sub(reqs[0].at) / 4
	if gap < 80*time.Millisecond {
		t.Fatalf("gap between scheduled passes = %v, want about 90 ms or more", gap)
	}
	if l, _ := s.Status(); l.Trigger != "schedule" {
		t.Fatalf("trigger = %q", l.Trigger)
	}
}

func TestBackoffDoublesAndCaps(t *testing.T) {
	s := New(Config{BackoffBase: time.Second, BackoffMax: 10 * time.Second})
	defer s.Stop()
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	for i, w := range want {
		if got := s.backoff(i + 1); got != w {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
	d := New(Config{Interval: time.Hour})
	defer d.Stop()
	if d.cfg.BackoffMax != time.Hour || d.cfg.HostJitter != time.Hour/48 {
		t.Errorf("defaults: max %v jitter %v", d.cfg.BackoffMax, d.cfg.HostJitter)
	}
}

func TestHostBacksOffAfterFailuresAndGivesUp(t *testing.T) {
	var hashes [][20]byte
	var targets []Target
	bad := newFakeHTTP(t, nil)
	bad.answer = func(w http.ResponseWriter, r *http.Request, _ [][20]byte) { http.Error(w, "x", 500) }
	for i := 0; i < 8; i++ {
		h := h20(byte(i * 3))
		hashes = append(hashes, h)
		targets = append(targets, Target{Hash: h, Trackers: []string{bad.announce()}})
	}
	s, _ := newTest(t, targets, func(c *Config) { c.Batch = 1; c.BackoffBase = 40 * time.Millisecond; c.BackoffMax = time.Second })
	_, _ = s.StartPass("manual", nil)
	p := waitDone(t, s)
	if n := len(bad.requests()); n != 3 {
		t.Fatalf("a failing host gets %d requests, want 3 (MaxHostFailures)", n)
	}
	if p.Errors != 8 {
		t.Fatalf("every torrent of the host ends in error: %+v", p)
	}
	r := bad.requests()
	if g := r[2].at.Sub(r[1].at); g < 75*time.Millisecond {
		t.Fatalf("second backoff gap = %v, want >= 80 ms (doubled)", g)
	}
	e, _ := s.Entry(hashes[7])
	if e.Seeders != nil || !strings.Contains(e.Err, "repeated failures") {
		t.Fatalf("%+v", e)
	}
}

func TestTrackerMinIntervalIsHonoured(t *testing.T) {
	a, b := h20(1), h20(2)
	f := newFakeHTTP(t, map[[20]byte]Counts{a: {1, 1}, b: {1, 1}})
	f.answer = func(w http.ResponseWriter, r *http.Request, hs [][20]byte) {
		_, _ = w.Write([]byte("d5:filesd20:" + string(hs[0][:]) + "d8:completei1e10:incompletei1eee5:flagsd20:min_request_intervali1eee"))
	}
	s, _ := newTest(t, []Target{{Hash: a, Trackers: []string{f.announce()}}, {Hash: b, Trackers: []string{f.announce()}}},
		func(c *Config) { c.Batch = 1; c.BackoffMax = 300 * time.Millisecond })
	_, _ = s.StartPass("manual", nil)
	waitDone(t, s)
	r := f.requests()
	if len(r) != 2 || r[1].at.Sub(r[0].at) < 290*time.Millisecond {
		t.Fatalf("tracker's min interval (capped at BackoffMax) not honoured: %+v", r)
	}
}

func TestPacingAndBatchSettings(t *testing.T) {
	counts := map[[20]byte]Counts{}
	var targets []Target
	f := newFakeHTTP(t, counts)
	for i := 0; i < 5; i++ {
		h := h20(byte(i * 2))
		counts[h] = Counts{i, i}
		targets = append(targets, Target{Hash: h, Trackers: []string{f.announce()}})
	}
	s, _ := newTest(t, targets, func(c *Config) { c.Batch = 2; c.Pace = 120 * time.Millisecond })
	_, _ = s.StartPass("manual", nil)
	waitDone(t, s)
	r := f.requests()
	if len(r) != 3 || len(r[0].hashes) != 2 || len(r[1].hashes) != 2 || len(r[2].hashes) != 1 {
		t.Fatalf("batch=2 over 5 hashes should make 2+2+1, got %+v", r)
	}
	for i := 1; i < len(r); i++ {
		if g := r[i].at.Sub(r[i-1].at); g < 115*time.Millisecond {
			t.Fatalf("gap %d = %v, want >= pace 120 ms", i, g)
		}
	}
}

func TestUDPBatchLimitedTo70AndPartialAnswers(t *testing.T) {
	counts := map[[20]byte]Counts{}
	var targets []Target
	for i := 0; i < 75; i++ {
		var h [20]byte
		h[0], h[1] = byte(i), 7
		counts[h] = Counts{i, 1}
	}
	u := newFakeUDP(t, counts)
	for h := range counts {
		targets = append(targets, Target{Hash: h, Trackers: []string{u.announce()}})
	}
	s, _ := newTest(t, targets, func(c *Config) { c.Batch = 100 })
	_, _ = s.StartPass("manual", nil)
	p := waitDone(t, s)
	u.mu.Lock()
	n := u.scrape
	u.mu.Unlock()
	if n != 2 || p.OK != 75 {
		t.Fatalf("75 hashes over UDP: %d packets (want 70+5), pass %+v", n, p)
	}
}

func TestPrivateTargetsOnlyReachTheirOwnTrackers(t *testing.T) {
	// The scraper only ever talks to the hosts in a Target's Trackers; the
	// source of that list is pinned in internal/gostorm/torr (retrack_test.go).
	a := h20(1)
	own := newFakeHTTP(t, map[[20]byte]Counts{a: {1, 1}})
	other := newFakeHTTP(t, map[[20]byte]Counts{a: {99, 99}})
	s, _ := newTest(t, []Target{{Hash: a, Trackers: []string{own.announce()}}}, nil)
	_, _ = s.StartPass("manual", nil)
	waitDone(t, s)
	if len(other.requests()) != 0 {
		t.Fatal("a host outside the target's trackers was contacted")
	}
}

// An evidence pass is a one-torrent pass over the same targets: it asks only the
// trackers of the hash it was started for, a private torrent's own and no others.
func TestEvidencePassReachesOnlyTheTorrentsOwnTrackers(t *testing.T) {
	a, b := h20(1), h20(2)
	own := newFakeHTTP(t, map[[20]byte]Counts{a: {1, 1}})
	otherTorrent := newFakeHTTP(t, map[[20]byte]Counts{b: {5, 5}})
	public := newFakeHTTP(t, map[[20]byte]Counts{a: {99, 99}})
	s, _ := newTest(t, []Target{
		{Hash: a, Trackers: []string{own.announce()}},
		{Hash: b, Trackers: []string{otherTorrent.announce()}},
	}, nil)
	if _, err := s.StartPass("evidence", &a); err != nil {
		t.Fatal(err)
	}
	p := waitDone(t, s)
	if p.Trigger != "evidence" || p.Scope != "one" || p.Torrents != 1 {
		t.Fatalf("pass = %+v, want a one-torrent evidence pass", p)
	}
	if len(own.requests()) == 0 {
		t.Fatal("the torrent's own tracker was not asked")
	}
	if len(public.requests()) != 0 || len(otherTorrent.requests()) != 0 {
		t.Fatal("a host outside the torrent's own trackers was contacted")
	}
}

func TestPasskeyNeverLeaks(t *testing.T) {
	const passkey = "test-passkey-not-real"
	const auth = "test-token-not-real"
	a, b, c := h20(1), h20(2), h20(3)
	echo := newFakeHTTP(t, nil) // echoes the secrets in its failure reason
	echo.answer = func(w http.ResponseWriter, r *http.Request, _ [][20]byte) {
		reason := "unknown passkey " + passkey + " / " + auth + " for you"
		_, _ = w.Write([]byte("d14:failure reason" + strconv.Itoa(len(reason)) + ":" + reason + "e"))
	}
	ok := newFakeHTTP(t, map[[20]byte]Counts{b: {4, 2}})
	udp := newFakeUDP(t, nil)
	udp.mode = "error"
	s, lb := newTest(t, []Target{
		{Hash: a, Trackers: []string{echo.srv.URL + "/" + passkey + "/announce?auth=" + auth}},
		{Hash: b, Trackers: []string{ok.srv.URL + "/" + passkey + "/announce?auth=" + auth}},
		{Hash: c, Trackers: []string{"http://nonexistent.invalid/" + passkey + "/announce"}},
	}, nil)
	_, _ = s.StartPass("manual", nil)
	waitDone(t, s)
	var out strings.Builder
	out.WriteString(lb.all())
	for _, h := range [][20]byte{a, b, c} {
		e, _ := s.Entry(h)
		out.WriteString(e.Err)
	}
	if st, _ := s.Status(); st != nil {
		out.WriteString(st.Trigger)
	}
	if strings.Contains(out.String(), passkey) || strings.Contains(out.String(), auth) {
		t.Fatalf("a secret leaked:\n%s", out.String())
	}
	if e, _ := s.Entry(a); e.Err == "" || !strings.HasPrefix(e.Err, echo.host()+": tracker: unknown passkey ***") {
		t.Fatalf("scrubbed error = %q", e.Err)
	}
	if !strings.Contains(lb.all(), echo.host()) {
		t.Fatal("the host should be logged")
	}
}
