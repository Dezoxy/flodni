package scrape

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hangTracker never answers: it holds each request until the scraper gives up
// (the client closing the connection cancels the request context) or the test ends.
func hangTracker(t *testing.T) *fakeHTTP {
	f := newFakeHTTP(t, nil)
	f.answer = func(w http.ResponseWriter, r *http.Request, _ [][20]byte) {
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}
	return f
}

// hangTimeout is the per-request timeout of tests that need a tracker to time out.
// It is the liveness bound, so it is generous: the answering trackers in the same
// test must never come close to it, even on a loaded machine. The test then waits
// for the hung request to hit it and asserts on the outcome, never on how fast.
const hangTimeout = 5 * time.Second

// The numbers are the max over the trackers that gave a count; the entry carries
// the facts (answered, failed, unscrapable, unknown) and the caller decides.
func TestFactsReportMaxOverAnsweredTrackers(t *testing.T) {
	t.Parallel() // spends hangTimeout waiting for the hung tracker
	a, b, c, d := h20(1), h20(2), h20(3), h20(4)
	zero := newFakeHTTP(t, map[[20]byte]Counts{a: {0, 0}, b: {0, 0}, c: {5, 1}})
	hang := hangTracker(t)
	s, _ := newTest(t, []Target{
		{Hash: a, Trackers: []string{hang.announce(), zero.announce()}},                          // one times out, one says 0
		{Hash: b, Trackers: []string{"http://tracker.example.org/tracker.php", zero.announce()}}, // one has no scrape URL
		{Hash: c, Trackers: []string{hang.announce(), zero.announce()}},                          // 5 stands even so
		{Hash: d, Trackers: []string{hang.announce(), zero.announce()}},                          // d is unknown to the answering tracker
	}, func(cf *Config) { cf.Timeout = hangTimeout })
	_, _ = s.StartPass("manual", nil)
	waitDone(t, s)
	ea, _ := s.Entry(a)
	if ea.Seeders == nil || *ea.Seeders != 0 || ea.Err != "" || ea.Answered != 1 || ea.Failed != 1 || ea.Unscrapable != 0 || ea.Unknown != 0 {
		t.Fatalf("timeout + 0: want seeders 0, answered 1, failed 1: %+v", ea)
	}
	eb, _ := s.Entry(b)
	if eb.Seeders == nil || *eb.Seeders != 0 || eb.Answered != 1 || eb.Unscrapable != 1 || eb.Failed != 0 {
		t.Fatalf("skipped + 0: %+v", eb)
	}
	ec, _ := s.Entry(c)
	if ec.Seeders == nil || *ec.Seeders != 5 || ec.Err != "" || ec.Source != "http" || ec.Failed != 1 || ec.Answered != 1 {
		t.Fatalf("a count above 0 with a failed tracker: %+v", ec)
	}
	ed, _ := s.Entry(d)
	if ed.Seeders != nil || ed.Answered != 0 || ed.Unknown != 1 || ed.Failed != 1 || !strings.HasSuffix(ed.Err, ": timeout") {
		t.Fatalf("nothing answered with a count: null, the failure as error: %+v", ed)
	}
}

// A hash absent from an HTTP answer is unknown to the tracker, not 0.
func TestHTTPOmittedHashIsUnknownNotZero(t *testing.T) {
	a, b, c := h20(1), h20(2), h20(3)
	A := newFakeHTTP(t, map[[20]byte]Counts{a: {3, 1}}) // omits b and c
	B := newFakeHTTP(t, map[[20]byte]Counts{c: {0, 2}}) // knows c with 0
	s, _ := newTest(t, []Target{
		{Hash: a, Trackers: []string{A.announce()}},
		{Hash: b, Trackers: []string{A.announce()}},
		{Hash: c, Trackers: []string{A.announce(), B.announce()}},
	}, nil)
	_, _ = s.StartPass("manual", nil)
	waitDone(t, s)
	if e, _ := s.Entry(a); e.Seeders == nil || *e.Seeders != 3 {
		t.Fatalf("other hashes keep their counts: %+v", e)
	}
	if e, _ := s.Entry(b); e.Seeders != nil || e.Leechers != nil || e.Err != UnknownToTracker {
		t.Fatalf("omitted hash: %+v", e)
	}
	// omitted on A, present with a count on B: B's answer, the 0 included
	if e, _ := s.Entry(c); e.Seeders == nil || *e.Seeders != 0 || *e.Leechers != 2 || e.Err != "" {
		t.Fatalf("falls through to the tracker that knows it: %+v", e)
	}
}

func TestSourceAndHTTPBeatsUDPOnATie(t *testing.T) {
	a, b, c := h20(1), h20(2), h20(3)
	h := newFakeHTTP(t, map[[20]byte]Counts{a: {0, 0}, b: {0, 3}})
	u := newFakeUDP(t, map[[20]byte]Counts{a: {0, 0}, b: {4, 4}, c: {2, 2}})
	s, _ := newTest(t, []Target{
		{Hash: a, Trackers: []string{u.announce(), h.announce()}},
		{Hash: b, Trackers: []string{h.announce(), u.announce()}},
		{Hash: c, Trackers: []string{u.announce()}},
	}, nil)
	_, _ = s.StartPass("manual", nil)
	waitDone(t, s)
	for _, w := range []struct {
		h       [20]byte
		source  string
		seeders int
	}{{a, "http", 0}, {b, "udp", 4}, {c, "udp", 2}} {
		e, _ := s.Entry(w.h)
		if e.Seeders == nil || *e.Seeders != w.seeders || e.Source != w.source {
			t.Errorf("%x: %+v, want %d from %s", w.h[:2], e, w.seeders, w.source)
		}
	}
}

// A host in a long backoff does not keep the pass open; the next pass starts with
// the torrents tried longest ago, so a long min interval does not pin the first batch.
func TestLongBackoffDoesNotHoldThePassOpenAndRotates(t *testing.T) {
	var targets []Target
	counts := map[[20]byte]Counts{}
	f := newFakeHTTP(t, counts)
	for i := 0; i < 3; i++ {
		h := h20(byte(i * 5))
		counts[h] = Counts{1, 1}
		targets = append(targets, Target{Hash: h, Trackers: []string{f.announce()}})
	}
	f.answer = func(w http.ResponseWriter, r *http.Request, hs [][20]byte) {
		_, _ = w.Write([]byte("d5:filesd20:" + string(hs[0][:]) + "d8:completei1e10:incompletei1eee5:flagsd20:min_request_intervali3600eee"))
	}
	var offset atomic.Int64
	s, _ := newTest(t, targets, func(c *Config) {
		c.Batch = 1
		c.BackoffMax = 2 * time.Hour
		c.MaxInPassWait = 100 * time.Millisecond
		c.Now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	})
	_, _ = s.StartPass("manual", nil) // held open for the 3600 s backoff, waitDone would give up at 10 s
	p := waitDone(t, s)
	if p.OK != 1 || p.Errors != 2 {
		t.Fatalf("pass = %+v", p)
	}
	var backed int
	for _, tg := range targets {
		if e, _ := s.Entry(tg.Hash); e.Err != "" && strings.HasSuffix(e.Err, ": host backing off") {
			backed++
		}
	}
	if backed != 2 {
		t.Fatalf("%d torrents ended 'host backing off', want 2", backed)
	}
	// the pass is over, so a manual request for one torrent is not blocked by 409
	if _, err := s.StartPass("manual", &targets[0].Hash); err != nil {
		t.Fatalf("one-torrent pass after: %v", err)
	}
	waitDone(t, s)

	// an hour and a bit later the host answers again; it starts with a torrent it did not serve before
	first := f.requests()[0].hashes[0]
	offset.Store(int64(2 * time.Hour))
	n := len(f.requests())
	_, _ = s.StartPass("manual", nil)
	waitDone(t, s)
	r := f.requests()
	if len(r) <= n || r[n].hashes[0] == first {
		t.Fatalf("second pass started with the same first batch again: %x vs %x", r[n].hashes[0][:2], first[:2])
	}
}

func TestFailureReasonRetryIsHonoured(t *testing.T) {
	a, b := h20(1), h20(2)
	f := newFakeHTTP(t, nil)
	f.answer = func(w http.ResponseWriter, r *http.Request, _ [][20]byte) {
		_, _ = w.Write([]byte("d14:failure reason4:slow12:min intervali1ee"))
	}
	s, _ := newTest(t, []Target{{Hash: a, Trackers: []string{f.announce()}}, {Hash: b, Trackers: []string{f.announce()}}},
		func(c *Config) { c.Batch = 1; c.BackoffMax = 300 * time.Millisecond; c.MaxInPassWait = time.Second })
	_, _ = s.StartPass("manual", nil)
	waitDone(t, s)
	r := f.requests()
	if len(r) != 2 || r[1].at.Sub(r[0].at) < 290*time.Millisecond {
		t.Fatalf("a failure reason's interval was not honoured: %+v", r)
	}
}

func TestCountsAreCapped(t *testing.T) {
	a, b := h20(1), h20(2)
	f := newFakeHTTP(t, nil)
	f.answer = func(w http.ResponseWriter, r *http.Request, hs [][20]byte) {
		_, _ = w.Write([]byte("d5:filesd20:" + string(a[:]) + "d8:completei2147483648e10:incompletei1ee20:" + string(b[:]) + "d8:completei2147483647e10:incompletei1eeee"))
	}
	got, _, e := HTTPScrape(t.Context(), newHTTPClient(time.Second, nil, false), f.srv.URL+"/scrape", [][20]byte{a, b}, 4096, "x")
	if e != nil {
		t.Fatal(e)
	}
	if _, ok := got[a]; ok {
		t.Fatal("2^31 must be refused")
	}
	if got[b].Seeders != 2147483647 {
		t.Fatalf("2^31-1 allowed: %+v", got[b])
	}
	u := newFakeUDP(t, map[[20]byte]Counts{a: {int(^uint32(0)), 7}})
	gu, e2 := UDPScrape(t.Context(), strings.TrimPrefix(u.host(), "udp://"), [][20]byte{a}, time.Second)
	if e2 != nil || gu[a].Seeders != 2147483647 {
		t.Fatalf("udp cap: %+v %v", gu, e2)
	}
}

func TestRedactionIsCaseInsensitiveAndCoversTruncatedEchoes(t *testing.T) {
	const pk = "Test-PassKey-Not-Real"
	ann := "http://tracker.example.org/" + pk + "/announce?auth=" + pk + "extra"
	sec := secretsOf(ann)
	for _, in := range []string{
		"bad key " + strings.ToLower(pk),
		"bad key " + strings.ToUpper(pk),
		"bad key " + pk[:10] + "...",
		"bad key ..." + pk[len(pk)-9:],
		"bad key " + pk + "extra",
	} {
		out := clean(in, sec)
		for _, frag := range []string{strings.ToLower(pk[:8]), strings.ToLower(pk[len(pk)-8:])} {
			if strings.Contains(strings.ToLower(out), frag) {
				t.Errorf("%q -> %q still holds %q", in, out, frag)
			}
		}
	}
	if out := clean("bad key "+pk+"extra", sec); strings.Count(out, "***") != 1 {
		t.Errorf("longest token first, one replacement: %q", out)
	}
}

// With an HTTP proxy configured, HTTP scrapes go through it with the given user agent
// and UDP trackers are not scraped at all.
func TestProxyCarriesHTTPAndUDPIsSkipped(t *testing.T) {
	a, b := h20(1), h20(2)
	var mu sync.Mutex
	var seen []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Host+"|"+r.Header.Get("User-Agent"))
		mu.Unlock()
		_, _ = w.Write(benFiles(map[[20]byte]Counts{a: {8, 1}}))
	}))
	defer proxy.Close()
	pu, _ := url.Parse(proxy.URL)
	u := newFakeUDP(t, map[[20]byte]Counts{b: {9, 9}})
	c := h20(3)
	s, lb := newTest(t, []Target{
		{Hash: a, Trackers: []string{"http://tracker.example.org:6969/announce"}},
		{Hash: b, Trackers: []string{u.announce()}},
		{Hash: c, Trackers: []string{"http://tracker.example.org:6969/announce", u.announce()}},
	}, func(cf *Config) { cf.Proxy = pu; cf.UserAgent = "engine-ua/1" })
	_, _ = s.StartPass("manual", nil)
	waitDone(t, s)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "tracker.example.org:6969|engine-ua/1" { // one batch of a and c
		t.Fatalf("proxy saw %v", seen)
	}
	u.mu.Lock()
	n := u.scrape
	u.mu.Unlock()
	if n != 0 {
		t.Fatal("UDP must not be scraped through a proxy setup")
	}
	if e, _ := s.Entry(a); e.Seeders == nil || *e.Seeders != 8 {
		t.Fatalf("http via proxy: %+v", e)
	}
	if _, ok := s.Entry(b); ok {
		t.Fatal("a torrent with only skipped trackers keeps its previous entry (none here)")
	}
	if !strings.Contains(lb.all(), "udp skipped: proxy") {
		t.Fatal("the skip should be logged")
	}
	// c: the proxied http tracker omitted it, the udp one was skipped
	if e, _ := s.Entry(c); e.Seeders != nil || e.Unscrapable != 1 || e.Unknown != 1 || e.Err != UnknownToTracker {
		t.Fatalf("mixed entry: %+v", e)
	}
}

// Nothing is loaded before the stopped, running and ready checks.
func TestStartPassChecksBeforeLoadingAndWaitsForReady(t *testing.T) {
	a := h20(1)
	f := newFakeHTTP(t, map[[20]byte]Counts{a: {1, 1}})
	gate := make(chan struct{})
	f.answer = func(w http.ResponseWriter, r *http.Request, _ [][20]byte) { <-gate; _, _ = w.Write(benFiles(f.counts)) }
	var loads atomic.Int32
	var ready atomic.Bool
	s, _ := newTest(t, nil, func(c *Config) {
		c.Timeout = 5 * time.Second
		c.Ready = ready.Load
		c.Source = func(only *[20]byte) []Target {
			loads.Add(1)
			return []Target{{Hash: a, Trackers: []string{f.announce()}}}
		}
	})
	if _, err := s.StartPass("manual", nil); err != ErrNotReady || loads.Load() != 0 {
		t.Fatalf("before ready: %v, %d loads", err, loads.Load())
	}
	ready.Store(true)
	if _, err := s.StartPass("manual", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartPass("manual", nil); err != ErrRunning || loads.Load() != 1 {
		t.Fatalf("while running: %v, %d loads", err, loads.Load())
	}
	close(gate)
	waitDone(t, s)
	s.Stop()
	if _, err := s.StartPass("manual", nil); err != ErrStopped || loads.Load() != 1 {
		t.Fatalf("after stop: %v, %d loads", err, loads.Load())
	}
	_ = strconv.Itoa
}

func TestRedactionSurvivesOddUnicode(t *testing.T) {
	const pk = "test-passkey-not-real"
	sec := secretsOf("http://tracker.example.org/" + pk + "/announce")
	// U+023A and U+2C65 change length when lowered; U+212A (Kelvin) lowers to 'k'
	odd := "\u023a\u2c65\u212a\xff\xfe"
	for _, in := range []string{
		odd + pk + odd,
		"test-pass\u212aey-not-real",
		"\u023a\u023a\u023a\u023a" + strings.ToUpper(pk) + "\u023a",
		pk[:5] + odd + pk[5:],
		strings.Repeat(odd, 200) + pk,
		pk + pk + odd,
	} {
		out := clean(in, sec)
		if strings.Contains(strings.ToLower(out), strings.ToLower(pk[:8])) || strings.Contains(strings.ToLower(out), strings.ToLower(pk[len(pk)-8:])) {
			t.Errorf("%q leaked: %q", in, out)
		}
		for i := 0; i < len(out); i++ {
			if out[i] < 0x20 || out[i] > 0x7e {
				t.Fatalf("non-ASCII byte in %q", out)
			}
		}
		if len(out) > maxMsg {
			t.Fatalf("too long: %d", len(out))
		}
	}
}

func FuzzClean(f *testing.F) {
	const pk = "test-passkey-not-real"
	for _, s := range []string{"", pk, strings.ToUpper(pk), "\u023a" + pk, "\u212aey", "\xff\xfe", pk[:9], "a" + pk + "b" + pk} {
		f.Add(s)
	}
	sec := secretsOf("http://tracker.example.org/" + pk + "/announce?auth=Tok3nValue99")
	f.Fuzz(func(t *testing.T, in string) {
		out := clean(in, sec)
		if len(out) > maxMsg {
			t.Fatalf("too long")
		}
		for i := 0; i < len(out); i++ {
			if out[i] < 0x20 || out[i] > 0x7e {
				t.Fatalf("non-ASCII byte")
			}
		}
		// a whole secret in the input never survives, in any letter case
		if strings.Contains(strings.ToLower(asciiOnly(in)), strings.ToLower(pk)) && strings.Contains(strings.ToLower(out), strings.ToLower(pk)) {
			t.Fatalf("passkey survived: %q", out)
		}
	})
}

// A tracker answer that makes the worker panic ends that host's part of the pass with an
// error and leaves the other hosts, and the daemon, alone.
func TestPanicInAWorkerIsContained(t *testing.T) {
	a, b := h20(1), h20(2)
	bad := newFakeHTTP(t, nil)
	bad.answer = func(w http.ResponseWriter, r *http.Request, _ [][20]byte) { http.Error(w, "x", 500) }
	good := newFakeHTTP(t, map[[20]byte]Counts{b: {4, 4}})
	lb := &logBuf{}
	s, _ := newTest(t, []Target{
		{Hash: a, Trackers: []string{bad.announce()}},
		{Hash: b, Trackers: []string{good.announce()}},
	}, func(c *Config) {
		c.Logf = func(f string, args ...any) {
			if strings.Contains(sprintf(f, args...), "http status 500") {
				panic("boom")
			}
			lb.logf(f, args...)
		}
	})
	_, _ = s.StartPass("manual", nil)
	p := waitDone(t, s)
	ea, _ := s.Entry(a)
	if ea.Seeders != nil || !strings.HasSuffix(ea.Err, ": internal error") || ea.Failed != 1 {
		t.Fatalf("panicked host's torrent: %+v", ea)
	}
	if eb, _ := s.Entry(b); eb.Seeders == nil || *eb.Seeders != 4 {
		t.Fatalf("other host: %+v", eb)
	}
	if p.Errors != 1 || p.OK != 1 || !strings.Contains(lb.all(), "internal error, host skipped") {
		t.Fatalf("pass %+v log %q", p, lb.all())
	}
}
