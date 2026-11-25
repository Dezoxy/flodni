package daemon

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"flodni/internal/flog"
	"flodni/internal/promtext"
	trackscrape "flodni/internal/scrape"
	"flodni/internal/warmer"
)

// Opt-in (scrape_on_failure): when Flodni records failure evidence for a torrent,
// it starts a one-torrent scrape pass for that hash, so the record holds fresh
// seeder counts within seconds. It reuses StartPass, so a private torrent is
// scraped only on its own trackers exactly as for the schedule and the manual route.
const (
	// scrapeEvidenceTriggerName is the trigger the pass reports in /api/torrents/scrape/status.
	scrapeEvidenceTriggerName = "evidence"
	// scrapeEvidenceInterval is the least gap between two evidence passes for one hash.
	scrapeEvidenceInterval = time.Hour

	scrapeEvidenceStarted     = "started"
	scrapeEvidenceRateLimited = "rate_limited"
	scrapeEvidenceBusy        = "busy"
	scrapeEvidenceDisabled    = "disabled"
)

var scrapeEvidenceResults = [...]string{scrapeEvidenceStarted, scrapeEvidenceRateLimited, scrapeEvidenceBusy, scrapeEvidenceDisabled}

// passStarter is the part of the scraper the trigger needs.
type passStarter interface {
	StartPass(trigger string, hash *[20]byte) (trackscrape.Pass, error)
}

type scrapeEvidence struct {
	enabled func() bool
	scraper func() passStarter // nil when the scrape is not configured
	now     func() time.Time
	async   func(func()) // runs the start off the caller's goroutine
	logf    func(string)

	mu   sync.Mutex
	last map[[20]byte]time.Time // when a pass was last started or claimed per hash

	counts [len(scrapeEvidenceResults)]atomic.Int64
}

// evidenceScrape is the daemon's trigger; tests replace it.
var evidenceScrape = &scrapeEvidence{
	enabled: func() bool { c := gc(); return c != nil && c.ScrapeOnFailure },
	scraper: func() passStarter {
		if s := trackerScraper.Load(); s != nil {
			return s
		}
		return nil
	},
	now:   time.Now,
	async: func(f func()) { go f() },
	logf:  func(line string) { flog.Printf(logger, "%s", line) },
	last:  map[[20]byte]time.Time{},
}

func (e *scrapeEvidence) bump(result string) {
	for i, r := range scrapeEvidenceResults {
		if r == result {
			e.counts[i].Add(1)
			return
		}
	}
}

func (e *scrapeEvidence) count(result string) int64 {
	for i, r := range scrapeEvidenceResults {
		if r == result {
			return e.counts[i].Load()
		}
	}
	return 0
}

// note reports failure evidence for hash (hex) with a short cause. It returns at
// once and holds no engine, handle or pump lock: the pass is started on another
// goroutine, because StartPass reads the stored record.
func (e *scrapeEvidence) note(hash, cause string) {
	if !e.enabled() {
		e.bump(scrapeEvidenceDisabled)
		return
	}
	s := e.scraper()
	if s == nil {
		e.bump(scrapeEvidenceDisabled)
		return
	}
	raw, err := hex.DecodeString(strings.ToLower(hash))
	if err != nil || len(raw) != 20 {
		return
	}
	var h [20]byte
	copy(h[:], raw)
	now := e.now()
	e.mu.Lock()
	if at, ok := e.last[h]; ok && now.Sub(at) < scrapeEvidenceInterval {
		e.mu.Unlock()
		e.bump(scrapeEvidenceRateLimited)
		return
	}
	for k, at := range e.last { // bounded: only hashes inside the hour stay
		if now.Sub(at) >= scrapeEvidenceInterval {
			delete(e.last, k)
		}
	}
	e.last[h] = now // claimed, so a second event for the hash cannot start another
	e.mu.Unlock()
	e.async(func() { e.start(s, h, cause) })
}

func (e *scrapeEvidence) start(s passStarter, h [20]byte, cause string) {
	_, err := s.StartPass(scrapeEvidenceTriggerName, &h)
	if err == nil {
		e.bump(scrapeEvidenceStarted)
		e.logf(fmt.Sprintf("[Scrape] evidence pass for %s started (%s)", hex.EncodeToString(h[:4]), cause))
		return
	}
	// No pass ran for this hash, so it must not count against its hour.
	e.mu.Lock()
	delete(e.last, h)
	e.mu.Unlock()
	if errors.Is(err, trackscrape.ErrRunning) {
		e.bump(scrapeEvidenceBusy)
		return
	}
	e.bump(scrapeEvidenceDisabled) // stopping, not ready yet or no stored record
}

// noteWarmFailure is the trigger for a warm that ended without bytes from the swarm.
func noteWarmFailure(u warmer.Update) {
	if u.Readability == warmer.ReadFailed {
		evidenceScrape.note(u.Key.Hash, "warm_failed")
	}
}

// collectScrapeEvidenceProm writes the evidence counter, from 0 even with the switch off.
func collectScrapeEvidenceProm(w *promtext.Writer) {
	samples := make([]promtext.Sample, 0, len(scrapeEvidenceResults))
	for _, r := range scrapeEvidenceResults {
		samples = append(samples, promSample(evidenceScrape.count(r), lbl("result", r)))
	}
	w.Counter("flodni_tracker_scrape_evidence_total",
		"Failure-evidence events by what the opt-in scrape_on_failure did: started (a one-torrent scrape pass), rate_limited (one pass per torrent per hour), busy (a pass was running) and disabled (switch off, scrape not running or the pass could not start). Experimental.",
		samples...)
}
