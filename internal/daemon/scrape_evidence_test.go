package daemon

import (
	"flodni/internal/config"
	"flodni/internal/flog"
	"flodni/internal/promtext"
	"flodni/internal/warmer"
	"flodni/internal/warmup"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"flodni/internal/gostorm/native"
	trackscrape "flodni/internal/scrape"
)

// fakeStarter records the passes asked for. A set gate holds StartPass until it closes.
type fakeStarter struct {
	mu     sync.Mutex
	calls  []startCall
	err    error
	gate   chan struct{}
	called chan struct{}
}

type startCall struct {
	trigger string
	hash    [20]byte
}

func (f *fakeStarter) StartPass(trigger string, hash *[20]byte) (trackscrape.Pass, error) {
	if f.called != nil {
		f.called <- struct{}{}
	}
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return trackscrape.Pass{}, f.err
	}
	f.calls = append(f.calls, startCall{trigger, *hash})
	return trackscrape.Pass{RunID: len(f.calls)}, nil
}

func (f *fakeStarter) got() []startCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]startCall(nil), f.calls...)
}

// newTestEvidence runs the start inline, so a test sees the result at once.
func newTestEvidence(on bool, s passStarter, clock *time.Time) (*scrapeEvidence, *syncBuf) {
	logs := &syncBuf{}
	e := &scrapeEvidence{
		enabled: func() bool { return on },
		scraper: func() passStarter { return s },
		now:     func() time.Time { return *clock },
		async:   func(f func()) { f() },
		logf:    func(line string) { _, _ = logs.Write([]byte(line + "\n")) },
		last:    map[[20]byte]time.Time{},
	}
	return e, logs
}

func evidenceHex(n byte) string { h := hashOf(n); return hexOf(h) }

func TestScrapeEvidenceSwitchOffStartsNothing(t *testing.T) {
	f := &fakeStarter{}
	now := time.Unix(1000, 0)
	e, _ := newTestEvidence(false, f, &now)
	e.note(evidenceHex(1), "dead_swarm")
	if len(f.got()) != 0 {
		t.Fatalf("a pass started with the switch off: %v", f.got())
	}
	if n := e.count(scrapeEvidenceDisabled); n != 1 {
		t.Fatalf("disabled count = %d, want 1", n)
	}
}

func TestScrapeEvidenceNilScraperIsDisabledNotAPanic(t *testing.T) {
	now := time.Unix(1000, 0)
	e, _ := newTestEvidence(true, nil, &now)
	e.note(evidenceHex(1), "dead_swarm")
	if n := e.count(scrapeEvidenceDisabled); n != 1 {
		t.Fatalf("disabled count = %d, want 1", n)
	}
}

func TestScrapeEvidenceStartsOnePassWithTheEvidenceTrigger(t *testing.T) {
	f := &fakeStarter{}
	now := time.Unix(1000, 0)
	e, logs := newTestEvidence(true, f, &now)
	e.note(strings.ToUpper(evidenceHex(1)), "metainfo_timeout")
	calls := f.got()
	if len(calls) != 1 || calls[0].trigger != "evidence" || calls[0].hash != hashOf(1) {
		t.Fatalf("calls = %+v, want one evidence pass for hash 1", calls)
	}
	if n := e.count(scrapeEvidenceStarted); n != 1 {
		t.Fatalf("started count = %d, want 1", n)
	}
	line := logs.String()
	if !strings.Contains(line, evidenceHex(1)[:8]) || !strings.Contains(line, "metainfo_timeout") ||
		strings.Contains(line, evidenceHex(1)[:12]) {
		t.Fatalf("log line = %q, want the short hash and the cause only", line)
	}
}

func TestScrapeEvidenceRateLimitsPerHash(t *testing.T) {
	f := &fakeStarter{}
	now := time.Unix(1000, 0)
	e, _ := newTestEvidence(true, f, &now)
	e.note(evidenceHex(1), "dead_swarm")
	now = now.Add(scrapeEvidenceInterval - time.Second)
	e.note(evidenceHex(1), "dead_swarm") // same hash inside the hour
	e.note(evidenceHex(2), "dead_swarm") // another hash is not limited
	if len(f.got()) != 2 {
		t.Fatalf("calls = %+v, want hash 1 and hash 2 once each", f.got())
	}
	if e.count(scrapeEvidenceRateLimited) != 1 || e.count(scrapeEvidenceStarted) != 2 {
		t.Fatalf("counts: started %d rate_limited %d", e.count(scrapeEvidenceStarted), e.count(scrapeEvidenceRateLimited))
	}
	now = now.Add(2 * time.Second) // the hour has passed for hash 1
	e.note(evidenceHex(1), "dead_swarm")
	if len(f.got()) != 3 {
		t.Fatalf("hash 1 after the hour: calls = %+v", f.got())
	}
}

func TestScrapeEvidenceBusySkipsAndFreesTheHash(t *testing.T) {
	f := &fakeStarter{err: trackscrape.ErrRunning}
	now := time.Unix(1000, 0)
	e, _ := newTestEvidence(true, f, &now)
	e.note(evidenceHex(1), "dead_swarm")
	if e.count(scrapeEvidenceBusy) != 1 || e.count(scrapeEvidenceStarted) != 0 {
		t.Fatalf("busy %d started %d", e.count(scrapeEvidenceBusy), e.count(scrapeEvidenceStarted))
	}
	// A skipped event used no pass, so the next one is not rate limited.
	f.mu.Lock()
	f.err = nil
	f.mu.Unlock()
	e.note(evidenceHex(1), "dead_swarm")
	if len(f.got()) != 1 || e.count(scrapeEvidenceRateLimited) != 0 {
		t.Fatalf("after busy: calls %+v rate_limited %d", f.got(), e.count(scrapeEvidenceRateLimited))
	}
}

func TestScrapeEvidenceDoesNotBlockTheCaller(t *testing.T) {
	f := &fakeStarter{gate: make(chan struct{}), called: make(chan struct{}, 1)}
	now := time.Unix(1000, 0)
	e, _ := newTestEvidence(true, f, &now)
	e.async = func(fn func()) { go fn() } // the production hand-off
	done := make(chan struct{})
	go func() { e.note(evidenceHex(1), "dead_swarm"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("note blocked on a StartPass that never returns")
	}
	<-f.called
	close(f.gate)
	deadline := time.Now().Add(2 * time.Second)
	for len(f.got()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(f.got()) != 1 {
		t.Fatalf("the pass never started: %+v", f.got())
	}
}

func TestScrapeEvidenceIgnoresABadHash(t *testing.T) {
	f := &fakeStarter{}
	now := time.Unix(1000, 0)
	e, _ := newTestEvidence(true, f, &now)
	e.note("", "dead_swarm")
	e.note("zz", "dead_swarm")
	if len(f.got()) != 0 {
		t.Fatalf("a bad hash started a pass: %+v", f.got())
	}
}

// The recorder is the trigger site for a dead swarm and a metainfo timeout.
func TestRecorderTriggersEvidenceScrape(t *testing.T) {
	f := &fakeStarter{}
	now := time.Unix(1000, 0)
	e, _ := newTestEvidence(true, f, &now)
	prev := evidenceScrape
	evidenceScrape = e
	t.Cleanup(func() { evidenceScrape = prev })

	rec := newReachabilityRecorder(failingStore{}, flog.NewText(io.Discard))
	rec(native.Outcome{Hash: evidenceHex(7), Kind: native.OutcomeFailed, Reason: failReasonNoPeers, Source: failSourceMount})
	rec(native.Outcome{Hash: evidenceHex(8), Kind: native.OutcomeSwarmRead}) // not evidence
	if got := f.got(); len(got) != 1 || got[0].hash != hashOf(7) {
		t.Fatalf("after a dead-swarm verdict: %+v", got)
	}
	rec(native.Outcome{Hash: evidenceHex(9), Kind: native.OutcomeMetainfoTimeout})
	if got := f.got(); len(got) != 2 || got[1].hash != hashOf(9) {
		t.Fatalf("after a metainfo timeout: %+v", got)
	}
}

func TestWarmFailureIsEvidenceOnlyWhenTheSwarmFailed(t *testing.T) {
	f := &fakeStarter{}
	now := time.Unix(1000, 0)
	e, _ := newTestEvidence(true, f, &now)
	prev := evidenceScrape
	evidenceScrape = e
	t.Cleanup(func() { evidenceScrape = prev })

	noteWarmFailure(warmer.Update{Key: warmup.FileKey{Hash: evidenceHex(4)}, Readability: warmer.ReadWarmed})
	if len(f.got()) != 0 {
		t.Fatalf("a warmed file started a pass: %+v", f.got())
	}
	noteWarmFailure(warmer.Update{Key: warmup.FileKey{Hash: evidenceHex(4)}, Readability: warmer.ReadFailed})
	if got := f.got(); len(got) != 1 || got[0].hash != hashOf(4) {
		t.Fatalf("a failed warm: %+v", got)
	}
}

// The status route reports the trigger of an evidence pass, and /metrics carries the counter.
func TestEvidencePassShowsInStatusAndMetrics(t *testing.T) {
	a := hashOf(1)
	tr := newScrapeTracker(t, map[[20]byte][2]int{a: {3, 1}})
	s, _ := useScraper(t, fastCfg(), []trackscrape.Target{{Hash: a, Trackers: []string{tr.srv.URL + "/announce"}}})
	cfg := config.Config{ScrapeOnFailure: true}
	prevCfg := globalConfig.Load()
	globalConfig.Store(&cfg)
	t.Cleanup(func() { globalConfig.Store(prevCfg) })
	prev := evidenceScrape
	e := &scrapeEvidence{enabled: prev.enabled, scraper: prev.scraper, now: time.Now,
		async: func(f func()) { f() }, logf: func(string) {}, last: map[[20]byte]time.Time{}}
	evidenceScrape = e
	t.Cleanup(func() { evidenceScrape = prev })

	e.note(hexOf(a), "dead_swarm_no_peers")
	m := waitState(t, "done")
	if m["trigger"] != "evidence" || m["scope"] != "one" {
		t.Fatalf("status = %v", m)
	}
	_ = s
	var w promtext.Writer
	collectScrapeEvidenceProm(&w)
	if !strings.Contains(string(w.Bytes()), `flodni_tracker_scrape_evidence_total{result="started"} 1`) {
		t.Fatalf("metrics:\n%s", w.Bytes())
	}
}
