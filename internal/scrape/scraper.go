package scrape

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Target is one stored torrent and the announce URLs of its stored record.
// The URLs hold passkeys: they stay inside this package.
type Target struct {
	Hash     [20]byte
	Trackers []string
}

// Entry is the latest outcome for one torrent. Seeders and Leechers are nil
// unless a tracker answered: a failed pass never turns into a 0. RunID is the
// pass that produced the entry; a torrent a pass did not reach keeps its old one.
type Entry struct {
	Seeders, Leechers *int
	At                time.Time
	Err               string // host and failure class, or "unknown_to_tracker"; never a URL
	RunID             int
	Source            string // "http" or "udp": the tracker answer that supplied the numbers
	// Facts about the trackers of the torrent in this pass; the caller decides what
	// they mean (a 0 with Failed > 0 is a weaker signal than one with Failed 0).
	Answered    int // trackers that gave a count (0 included)
	Failed      int // failed, timed out, or host backing off
	Unscrapable int // wss://, unsupported scheme, no scrape URL, UDP behind a proxy
	Unknown     int // HTTP answers that omitted the hash
}

// UnknownToTracker is the entry error when no tracker gave a count and the ones that
// answered left the torrent out: the torrent is not known there, which is not a 0.
const UnknownToTracker = "unknown_to_tracker"

// Pass describes one pass. Torrents counts the torrents in it; OK, Errors and
// Skipped count torrents finished with numbers, with an error, and without a
// scrapeable tracker. A torrent a stopped pass did not reach counts in none.
type Pass struct {
	RunID      int
	Trigger    string // "schedule" or "manual"
	Scope      string // "all" or "one"
	State      string // "running" or "done"
	StartedAt  time.Time
	FinishedAt time.Time // zero while running
	Torrents   int
	OK         int
	Errors     int
	Skipped    int
}

var (
	// ErrRunning: a pass is already running; StartPass also returns it.
	ErrRunning = errors.New("a scrape pass is running")
	// ErrUnknownHash: a single-torrent pass for a hash that is not stored.
	ErrUnknownHash = errors.New("unknown torrent")
	// ErrStopped: the scraper was stopped.
	ErrStopped = errors.New("scraper stopped")
	// ErrNotReady: the stored records cannot be read yet (start-up).
	ErrNotReady = errors.New("stored torrents not ready")
)

// Config tunes a Scraper. Zero values take the defaults noted.
type Config struct {
	// Source lists the stored torrents at the start of a pass: every one for nil, only
	// that hash otherwise. It is not called while a pass runs or the scraper stops.
	Source func(only *[20]byte) []Target
	// Ready, when set, says whether Source may be called yet; before that StartPass
	// answers ErrNotReady.
	Ready func() bool
	// Proxy, when set, carries every HTTP scrape request, as the engine does for its
	// tracker requests. UDP cannot go through it, so UDP trackers are skipped
	// ("udp skipped: proxy") rather than scraped from the real address.
	Proxy *url.URL
	// NoUDP skips UDP trackers even without Proxy (the engine's proxy mode may leave
	// its HTTP requests direct while a proxy is still configured).
	NoUDP bool
	// InsecureTLS skips verification of an https tracker's certificate (tracker_tls_insecure).
	// Off by default.
	InsecureTLS bool
	// UserAgent is sent with HTTP scrapes (default "flodni"; the daemon passes the
	// engine's).
	UserAgent string
	// MaxInPassWait is the longest a host waits for its pacing or backoff inside a
	// pass (default 2 min); a longer block ends its remaining torrents of the pass
	// with "host backing off" so one slow host never holds the pass open.
	MaxInPassWait time.Duration
	// Interval between scheduled passes; 0 turns the schedule off (a manual pass
	// still works).
	Interval time.Duration
	// Batch is the most hashes per HTTP request (default 100). UDP is limited
	// to MaxUDPHashes by the protocol.
	Batch int
	// Pace is the least gap between two requests to one host (default 1 s).
	Pace time.Duration
	// Timeout bounds one request (default 10 s).
	Timeout time.Duration
	// BackoffBase doubles with each consecutive failed request of a host, up to
	// BackoffMax (defaults 30 s and 6 h, never above Interval when that is set).
	BackoffBase, BackoffMax time.Duration
	// HostJitter spreads the start of a scheduled pass: each host waits a random
	// part of it (default Interval/48 up to 10 min).
	HostJitter time.Duration
	// MaxHostFailures consecutive failed requests end a host's part of a pass; its
	// remaining torrents get an error (default 3).
	MaxHostFailures int
	// MaxBody bounds an HTTP answer (default DefaultMaxBody).
	MaxBody int64
	// Logf receives log lines: host names and failure classes only.
	Logf func(format string, args ...any)
	// Now and Rand are test seams.
	Now  func() time.Time
	Rand func() float64
}

type hostState struct {
	failures     int
	blockedUntil time.Time
}

// Scraper runs the passes.
type Scraper struct {
	cfg Config

	mu       sync.Mutex
	results  map[[20]byte]Entry
	runSeq   int
	cur      *Pass // the latest pass, running or done
	lastDone *Pass
	prevDone *Pass
	lastFull *Pass                  // last finished pass over all torrents, not cancelled
	tried    map[[20]byte]time.Time // when a request last carried the torrent
	hosts    map[string]*hostState
	stopped  bool
	starting int // run id reserved while the targets load, else 0

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	client *http.Client

	okTotal, errTotal, skipTotal atomic.Int64
}

// New builds a Scraper. Start begins the schedule; StartPass runs one pass now.
func New(cfg Config) *Scraper {
	if cfg.Batch <= 0 {
		cfg.Batch = 100
	}
	if cfg.Pace <= 0 {
		cfg.Pace = time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.BackoffBase <= 0 {
		cfg.BackoffBase = 30 * time.Second
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = 6 * time.Hour
		if cfg.Interval > 0 && cfg.Interval < cfg.BackoffMax {
			cfg.BackoffMax = cfg.Interval
		}
	}
	if cfg.HostJitter == 0 && cfg.Interval > 0 {
		cfg.HostJitter = min(cfg.Interval/48, 10*time.Minute)
	}
	if cfg.MaxHostFailures <= 0 {
		cfg.MaxHostFailures = 3
	}
	if cfg.MaxInPassWait <= 0 {
		cfg.MaxInPassWait = 2 * time.Minute
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "flodni"
	}
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = DefaultMaxBody
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Rand == nil {
		cfg.Rand = defaultRand
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Scraper{
		cfg:     cfg,
		results: map[[20]byte]Entry{},
		tried:   map[[20]byte]time.Time{},
		hosts:   map[string]*hostState{},
		ctx:     ctx,
		cancel:  cancel,
		client:  newHTTPClient(cfg.Timeout, cfg.Proxy, cfg.InsecureTLS),
	}
}

// Start runs the schedule until Stop: the first pass after a random part of the
// jitter, then one every Interval, give or take 10 %. A no-op when Interval is 0.
func (s *Scraper) Start() {
	if s.cfg.Interval <= 0 {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		wait := time.Duration(s.cfg.Rand() * float64(s.cfg.HostJitter))
		for {
			t := time.NewTimer(wait)
			select {
			case <-s.ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			wait = time.Duration((0.9 + 0.2*s.cfg.Rand()) * float64(s.cfg.Interval))
			if _, err := s.StartPass("schedule", nil); err != nil && !errors.Is(err, ErrStopped) {
				s.cfg.Logf("[Scrape] scheduled pass skipped: %v", err)
				wait = min(wait, time.Minute) // try again soon, not a whole interval later
			}
		}
	}()
}

// Stop cancels the running pass and waits for the workers. A torrent the pass
// had not finished keeps its previous entry.
func (s *Scraper) Stop() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()
}

const msgBackingOff = "host backing off"

type answer struct {
	Counts
	source string
}

type tstate struct {
	hash    [20]byte
	pending int
	answers []answer
	unknown int    // trackers that answered and left the torrent out
	failed  int    // trackers that failed
	skipped int    // trackers that cannot be scraped
	err     string // first tracker that failed
}

type item struct {
	ts      *tstate
	ep      Endpoint
	hash    [20]byte
	secrets []string
	age     time.Time // when a request last carried the torrent; zero if never
	done    bool
}

// StartPass starts a pass and returns at once. hash nil scrapes every stored
// torrent; otherwise only that one. While a pass runs it returns ErrRunning and
// the running pass. Nothing is loaded before those checks pass.
func (s *Scraper) StartPass(trigger string, hash *[20]byte) (Pass, error) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return Pass{}, ErrStopped
	}
	if s.starting != 0 {
		p := Pass{RunID: s.starting, Trigger: trigger, State: "running"}
		s.mu.Unlock()
		return p, ErrRunning
	}
	if s.cur != nil && s.cur.State == "running" {
		p := *s.cur
		s.mu.Unlock()
		return p, ErrRunning
	}
	if s.cfg.Ready != nil && !s.cfg.Ready() {
		s.mu.Unlock()
		return Pass{}, ErrNotReady
	}
	s.starting = s.runSeq + 1
	s.mu.Unlock()

	var targets []Target
	if s.cfg.Source != nil {
		targets = s.cfg.Source(hash)
	}
	scope := "all"
	if hash != nil {
		scope = "one"
		var one []Target
		for _, t := range targets {
			if t.Hash == *hash {
				one = append(one, t)
			}
		}
		targets = one
	}

	s.mu.Lock()
	s.starting = 0
	if scope == "one" && len(targets) == 0 {
		s.mu.Unlock()
		return Pass{}, ErrUnknownHash
	}
	if s.stopped {
		s.mu.Unlock()
		return Pass{}, ErrStopped
	}
	s.runSeq++
	pass := &Pass{RunID: s.runSeq, Trigger: trigger, Scope: scope, State: "running",
		StartedAt: s.cfg.Now(), Torrents: len(targets)}
	s.cur = pass

	work := map[string][]*item{}
	var order []string
	inSnapshot := make(map[[20]byte]bool, len(targets))
	proxySkips := 0
	for _, t := range targets {
		inSnapshot[t.Hash] = true
		ts := &tstate{hash: t.Hash}
		seen := map[[2]string]bool{}
		age := s.tried[t.Hash]
		for _, a := range t.Trackers {
			ep, err := Derive(a)
			if err != nil {
				s.skipTotal.Add(1)
				ts.skipped++
				continue
			}
			k := [2]string{ep.Host, ep.Key}
			if seen[k] {
				continue
			}
			seen[k] = true
			if ep.Kind == "udp" && (s.cfg.Proxy != nil || s.cfg.NoUDP) {
				s.skipTotal.Add(1)
				proxySkips++
				ts.skipped++
				continue
			}
			ts.pending++
			if _, ok := work[ep.Host]; !ok {
				order = append(order, ep.Host)
			}
			work[ep.Host] = append(work[ep.Host], &item{ts: ts, ep: ep, hash: t.Hash, secrets: secretsOf(a), age: age})
		}
		if ts.pending == 0 {
			pass.Skipped++
		}
	}
	// The oldest entries go first, never-scraped ones before all: a host that only
	// lets a few requests through a pass serves a different part of its torrents each time.
	for _, items := range work {
		sort.SliceStable(items, func(i, j int) bool { return items[i].age.Before(items[j].age) })
	}
	if scope == "all" {
		for h := range s.results {
			if !inSnapshot[h] {
				delete(s.results, h) // the torrent is gone
			}
		}
		for h := range s.tried {
			if !inSnapshot[h] {
				delete(s.tried, h)
			}
		}
	}
	hostStates := map[string]*hostState{}
	for _, h := range order {
		if s.hosts[h] == nil {
			s.hosts[h] = &hostState{}
		}
		hostStates[h] = s.hosts[h]
	}
	info := *pass
	s.wg.Add(1)
	s.mu.Unlock()

	s.cfg.Logf("[Scrape] pass %d (%s) started: %d torrents on %d hosts", info.RunID, trigger, info.Torrents, len(order))
	if proxySkips > 0 {
		s.cfg.Logf("[Scrape] udp skipped: proxy configured (%d tracker entries)", proxySkips)
	}
	go s.runPass(pass, work, order, hostStates, trigger == "schedule")
	return info, nil
}

func (s *Scraper) runPass(pass *Pass, work map[string][]*item, order []string, hs map[string]*hostState, jitter bool) {
	defer s.wg.Done()
	var wg sync.WaitGroup
	for _, h := range order {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.runHost(pass, h, work[h], hs[h], jitter)
		}()
	}
	wg.Wait()
	cancelled := s.ctx.Err() != nil
	s.mu.Lock()
	pass.State = "done"
	pass.FinishedAt = s.cfg.Now()
	s.prevDone, s.lastDone = s.lastDone, pass
	if pass.Scope == "all" && !cancelled {
		c := *pass
		s.lastFull = &c
	}
	done := *pass
	s.mu.Unlock()
	s.cfg.Logf("[Scrape] pass %d finished: %d ok, %d errors, %d skipped of %d torrents", done.RunID, done.OK, done.Errors, done.Skipped, done.Torrents)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// runHost is the only goroutine that talks to one host during a pass: its
// requests are sequential, paced and backed off on their own clock, so a slow
// or failing host never delays another host's batches.
func (s *Scraper) runHost(pass *Pass, host string, items []*item, hs *hostState, jitter bool) {
	defer func() {
		// Nothing a tracker sends may take the daemon down: a panic ends this host's
		// part of the pass with an error for what it had not finished.
		if r := recover(); r != nil {
			s.mu.Lock()
			var rest []*item
			for _, it := range items {
				if !it.done {
					rest = append(rest, it)
				}
			}
			s.mu.Unlock()
			func() {
				defer func() { _ = recover() }()
				s.cfg.Logf("[Scrape] %s: internal error, host skipped for this pass", host)
			}()
			if len(rest) > 0 {
				s.finish(pass, rest, nil, host, "internal error")
			}
		}
	}()
	if jitter && !sleepCtx(s.ctx, time.Duration(s.cfg.Rand()*float64(s.cfg.HostJitter))) {
		return
	}
	var groups [][]*item
	index := map[string]int{}
	for _, it := range items {
		i, ok := index[it.ep.Key]
		if !ok {
			i = len(groups)
			index[it.ep.Key] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], it)
	}
	consecutive := 0
	for _, g := range groups {
		size := s.cfg.Batch
		if g[0].ep.Kind == "udp" {
			size = min(size, MaxUDPHashes)
		}
		for start := 0; start < len(g); start += size {
			chunk := g[start:min(start+size, len(g))]
			if consecutive >= s.cfg.MaxHostFailures {
				s.finish(pass, chunk, nil, host, "host unavailable after repeated failures")
				continue
			}
			s.mu.Lock()
			wait := hs.blockedUntil.Sub(s.cfg.Now())
			s.mu.Unlock()
			if wait > s.cfg.MaxInPassWait {
				// Never wait out a long block inside a pass: it would hold every
				// manual request and the schedule hostage to the slowest host.
				s.finish(pass, chunk, nil, host, msgBackingOff)
				continue
			}
			if !sleepCtx(s.ctx, wait) {
				return
			}
			answers, retry, e := s.request(chunk)
			if s.ctx.Err() != nil {
				return
			}
			s.mu.Lock()
			now := s.cfg.Now()
			hs.blockedUntil = now.Add(max(s.cfg.Pace, min(retry, s.cfg.BackoffMax)))
			if e != nil {
				hs.failures++
				hs.blockedUntil = now.Add(max(s.cfg.Pace, min(retry, s.cfg.BackoffMax), s.backoff(hs.failures)))
			} else {
				hs.failures = 0
			}
			s.mu.Unlock()
			if e != nil {
				consecutive++
				msg := clean(e.Msg, chunk[0].secrets)
				s.cfg.Logf("[Scrape] %s: %s", host, msg)
				s.finish(pass, chunk, nil, host, msg)
				continue
			}
			consecutive = 0
			s.finish(pass, chunk, answers, host, "")
		}
	}
}

func (s *Scraper) backoff(failures int) time.Duration {
	d := s.cfg.BackoffBase
	for i := 1; i < failures && d < s.cfg.BackoffMax; i++ {
		d *= 2
	}
	return min(d, s.cfg.BackoffMax)
}

func (s *Scraper) request(chunk []*item) (map[[20]byte]Counts, time.Duration, *Error) {
	hashes := make([][20]byte, len(chunk))
	for i, it := range chunk {
		hashes[i] = it.hash
	}
	if chunk[0].ep.Kind == "udp" {
		a, e := UDPScrape(s.ctx, chunk[0].ep.Key, hashes, s.cfg.Timeout)
		return a, 0, e
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.Timeout)
	defer cancel()
	a, retry, e := HTTPScrape(ctx, s.client, chunk[0].ep.Key, hashes, s.cfg.MaxBody, s.cfg.UserAgent)
	if e != nil {
		retry = max(retry, e.RetryAfter) // a failure reason can carry the tracker's own interval
	}
	return a, retry, e
}

// finish records one request's outcome for each item and, when a torrent has
// heard from all its trackers, writes its entry. answers nil means the whole
// request failed with msg. Otherwise an item missing from answers is a hash the
// tracker did not know: not a count, not a failure.
//
// Flodni reports facts and the caller decides: the numbers are the maximum over
// the trackers that gave a count (0 when every one of them said 0), and the entry
// says how many answered, failed, were unscrapable or did not know the hash. A 0
// with failed > 0 is a weaker signal than one with failed 0.
func (s *Scraper) finish(pass *Pass, chunk []*item, answers map[[20]byte]Counts, host, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range chunk {
		ts := it.ts
		ts.pending--
		it.done = true
		src := it.ep.Kind
		if msg != msgBackingOff {
			s.tried[it.hash] = s.cfg.Now()
		}
		if c, ok := answers[it.hash]; ok {
			s.okTotal.Add(1)
			ts.answers = append(ts.answers, answer{Counts: c, source: src})
		} else if answers != nil {
			s.errTotal.Add(1)
			ts.unknown++
		} else {
			s.errTotal.Add(1)
			ts.failed++
			if ts.err == "" {
				ts.err = host + ": " + clean(msg, it.secrets)
			}
		}
		if ts.pending > 0 {
			continue
		}
		e := Entry{At: s.cfg.Now(), RunID: pass.RunID, Answered: len(ts.answers), Failed: ts.failed,
			Unscrapable: ts.skipped, Unknown: ts.unknown}
		var best *answer
		for i := range ts.answers {
			a := &ts.answers[i]
			// most seeders wins; on a tie an http answer, which was present in the
			// tracker's files, beats a udp one (a udp 0 may mean unknown)
			if best == nil || a.Seeders > best.Seeders || (a.Seeders == best.Seeders && a.source == "http" && best.source != "http") {
				best = a
			}
		}
		switch {
		case best != nil:
			e.Seeders, e.Leechers, e.Source = &best.Seeders, &best.Leechers, best.source
			pass.OK++
		case ts.err != "":
			e.Err = ts.err
			pass.Errors++
		default:
			e.Err = UnknownToTracker
			pass.Errors++
		}
		s.results[ts.hash] = e
	}
}

// Entry returns a torrent's latest entry.
func (s *Scraper) Entry(hash [20]byte) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.results[hash]
	return e, ok
}

// Hashes lists the torrents that have an entry.
func (s *Scraper) Hashes() [][20]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][20]byte, 0, len(s.results))
	for h := range s.results {
		out = append(out, h)
	}
	return out
}

// Status returns the latest pass (running or done) and the finished one before
// it. Either can be nil.
func (s *Scraper) Status() (latest, previous *Pass) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return nil, nil
	}
	c := *s.cur
	prev := s.prevDone
	if c.State == "running" {
		prev = s.lastDone
	}
	if prev != nil {
		p := *prev
		previous = &p
	}
	return &c, previous
}

// LastFullPass is the last finished pass over every torrent that was not cut short.
func (s *Scraper) LastFullPass() *Pass {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastFull == nil {
		return nil
	}
	p := *s.lastFull
	return &p
}

// Running says whether a pass is running.
func (s *Scraper) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur != nil && s.cur.State == "running"
}

// Counts returns per-tracker outcomes since start: answered, failed (a failed
// request counts each torrent in it), and skipped (no scrape URL).
func (s *Scraper) Counts() (ok, errs, skipped int64) {
	return s.okTotal.Load(), s.errTotal.Load(), s.skipTotal.Load()
}

// SeederBuckets counts torrents whose latest entry has numbers, by seeders:
// 0, 1 to 2, 3 or more.
func (s *Scraper) SeederBuckets() (zero, few, many int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.results {
		switch {
		case e.Seeders == nil:
		case *e.Seeders == 0:
			zero++
		case *e.Seeders <= 2:
			few++
		default:
			many++
		}
	}
	return
}
