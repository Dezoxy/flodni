package daemon

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"flodni/internal/flog"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"flodni/internal/config"
	"flodni/internal/gostorm/torr"
	"flodni/internal/promtext"
	trackscrape "flodni/internal/scrape"
)

// trackerScraper is the EXPERIMENTAL tracker scrape (README): nil until started, and
// nil when a test does not need it. The handlers and the health response read it.
var trackerScraper atomic.Pointer[trackscrape.Scraper]

// scrapeHealth is the "scrape" object of /api/torrents/health. seeders, answered, failed,
// unknown, source, error and run_id are stable (scrape_contract_test.go); the rest is
// EXPERIMENTAL. seeders and leechers are the most over the trackers that gave a count,
// null when none did; a failed pass is an error, never a 0.
// run_id is the pass the entry came from: a torrent a pass did not reach keeps the
// entry, and the run_id, of the pass that did.
type scrapeHealth struct {
	Seeders   *int    `json:"seeders"`
	Leechers  *int    `json:"leechers"`
	ScrapedAt *string `json:"scraped_at"`
	Error     *string `json:"error"`
	// Source is the protocol of the answer that supplied the numbers, "http" or "udp"; a
	// UDP 0 may mean "unknown" (BEP 15 answers a fixed triple), an HTTP one was present
	// in the tracker's files. Null when there are no numbers.
	Source *string `json:"source"`
	// Facts about the torrent's trackers in that pass; the caller decides what they mean.
	Answered    int `json:"answered"`
	Failed      int `json:"failed"`
	Unscrapable int `json:"unscrapable"`
	Unknown     int `json:"unknown"`
	RunID       int `json:"run_id"`
}

// scrapePassHealth is the top-level "scrape_pass": the last finished pass over every torrent.
type scrapePassHealth struct {
	RunID      int    `json:"run_id"`
	FinishedAt string `json:"finished_at"`
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func scrapeFor(hash string) *scrapeHealth {
	s := trackerScraper.Load()
	if s == nil {
		return nil
	}
	raw, err := hex.DecodeString(hash)
	if err != nil || len(raw) != 20 {
		return nil
	}
	var h [20]byte
	copy(h[:], raw)
	e, ok := s.Entry(h)
	if !ok {
		return nil
	}
	out := &scrapeHealth{Seeders: e.Seeders, Leechers: e.Leechers, RunID: e.RunID,
		Answered: e.Answered, Failed: e.Failed, Unscrapable: e.Unscrapable, Unknown: e.Unknown}
	at := rfc3339(e.At)
	out.ScrapedAt = &at
	out.Error = strOrNil(e.Err)
	out.Source = strOrNil(e.Source)
	return out
}

func scrapePassFor() *scrapePassHealth {
	s := trackerScraper.Load()
	if s == nil {
		return nil
	}
	p := s.LastFullPass()
	if p == nil {
		return nil
	}
	return &scrapePassHealth{RunID: p.RunID, FinishedAt: rfc3339(p.FinishedAt)}
}

// storedHashes is the set of stored torrents (a seam for tests).
var storedHashes = torr.StoredHashes

// scrapeHashes lists the torrents that have a scrape entry, as hex.
func scrapeHashes() []string {
	s := trackerScraper.Load()
	if s == nil {
		return nil
	}
	var out []string
	for _, h := range s.Hashes() {
		out = append(out, hex.EncodeToString(h[:]))
	}
	return out
}

// storedScrapeTargets turns the stored records into scrape targets: all of them, or
// the one hash alone (only that record is read). Which trackers a torrent may be asked is
// decided in torr (a private torrent: its own only).
func storedScrapeTargets(only *[20]byte) []trackscrape.Target {
	var stored []torr.ScrapeTarget
	if only != nil {
		stored = torr.StoredScrapeTarget(hex.EncodeToString(only[:]))
	} else {
		stored = torr.StoredScrapeTargets()
	}
	out := make([]trackscrape.Target, 0, len(stored))
	for _, st := range stored {
		raw, err := hex.DecodeString(st.Hash)
		if err != nil || len(raw) != 20 {
			continue
		}
		t := trackscrape.Target{Trackers: st.Trackers}
		copy(t.Hash[:], raw)
		out = append(out, t)
	}
	return out
}

// startTrackerScraper builds the scraper from the settings and starts its schedule
// (tracker_scrape_hours 0 leaves only the on-demand route). Log lines carry hosts and
// failure classes, never an announce or scrape URL.
func startTrackerScraper(cfg *config.Config, source func(*[20]byte) []trackscrape.Target, ready func() bool,
	net torr.ScrapeNet, stop <-chan struct{}, logger *slog.Logger) *trackscrape.Scraper {
	s := trackscrape.New(trackscrape.Config{
		Source:    source,
		Ready:     ready,
		Proxy:     net.Proxy,
		NoUDP:     !net.UDP,
		UserAgent: net.UserAgent,
		Interval:  time.Duration(cfg.TrackerScrapeHours) * time.Hour,
		Batch:     cfg.TrackerScrapeBatch,
		Pace:      time.Duration(cfg.TrackerScrapePaceMs) * time.Millisecond,
		Logf:      flog.Logf(logger),
	})
	trackerScraper.Store(s)
	s.Start()
	go func() {
		<-stop
		s.Stop()
	}()
	if cfg.TrackerScrapeHours == 0 {
		logger.Info("[Scrape] experimental tracker scrape: schedule off (tracker_scrape_hours 0); POST /api/torrents/scrape runs a pass")
	} else {
		logger.Info(fmt.Sprintf("[Scrape] experimental tracker scrape: every %d h, batches of %d, %d ms between requests to one host", cfg.TrackerScrapeHours, cfg.TrackerScrapeBatch, cfg.TrackerScrapePaceMs), "tracker_scrape_hours", cfg.TrackerScrapeHours, "tracker_scrape_batch", cfg.TrackerScrapeBatch, "tracker_scrape_pace_ms", cfg.TrackerScrapePaceMs)
	}
	return s
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// handleScrapeTrigger serves POST /api/torrents/scrape[?hash=<40 hex>]: it starts a
// pass and answers 202 at once with its run_id; GET .../scrape/status shows how it goes.
// A pass already running answers 409 with the running run_id.
func handleScrapeTrigger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s := trackerScraper.Load()
	if s == nil {
		http.Error(w, "tracker scrape unavailable", http.StatusServiceUnavailable)
		return
	}
	var one *[20]byte
	if v := r.URL.Query().Get("hash"); v != "" {
		raw, err := hex.DecodeString(v)
		if err != nil || len(raw) != 20 {
			http.Error(w, "hash must be 40 hex characters", http.StatusBadRequest)
			return
		}
		var h [20]byte
		copy(h[:], raw)
		one = &h
	}
	p, err := s.StartPass("manual", one)
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]any{"run_id": p.RunID, "started_at": rfc3339(p.StartedAt)})
	case errors.Is(err, trackscrape.ErrRunning):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "a scrape pass is running", "run_id": p.RunID})
	case errors.Is(err, trackscrape.ErrUnknownHash):
		http.Error(w, "no stored torrent with that hash", http.StatusNotFound)
	case errors.Is(err, trackscrape.ErrNotReady):
		http.Error(w, "stored torrents are not ready yet", http.StatusServiceUnavailable)
	default:
		http.Error(w, "tracker scrape is stopping", http.StatusServiceUnavailable)
	}
}

type scrapePassJSON struct {
	RunID      int     `json:"run_id"`
	Trigger    string  `json:"trigger"`
	Scope      string  `json:"scope"`
	State      string  `json:"state"`
	StartedAt  string  `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
	Torrents   int     `json:"torrents"`
	OK         int     `json:"ok"`
	Errors     int     `json:"errors"`
	Skipped    int     `json:"skipped"`
}

func passJSON(p *trackscrape.Pass) *scrapePassJSON {
	if p == nil {
		return nil
	}
	out := &scrapePassJSON{RunID: p.RunID, Trigger: p.Trigger, Scope: p.Scope, State: p.State,
		StartedAt: rfc3339(p.StartedAt), Torrents: p.Torrents, OK: p.OK, Errors: p.Errors, Skipped: p.Skipped}
	if !p.FinishedAt.IsZero() {
		f := rfc3339(p.FinishedAt)
		out.FinishedAt = &f
	}
	return out
}

// handleScrapeStatus serves GET /api/torrents/scrape/status: the latest pass at the top
// level and the finished one before it as "previous". ok, errors and skipped count
// torrents, not trackers. With no pass yet it answers {"state":"none"}.
func handleScrapeStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s := trackerScraper.Load()
	if s == nil {
		http.Error(w, "tracker scrape unavailable", http.StatusServiceUnavailable)
		return
	}
	latest, prev := s.Status()
	if latest == nil {
		writeJSON(w, http.StatusOK, map[string]any{"state": "none", "previous": nil})
		return
	}
	cur := passJSON(latest)
	writeJSON(w, http.StatusOK, struct {
		*scrapePassJSON
		Previous *scrapePassJSON `json:"previous"`
	}{cur, passJSON(prev)})
}

// collectScrapeProm writes the tracker scrape series, none before the scraper exists.
func collectScrapeProm(w *promtext.Writer) {
	s := trackerScraper.Load()
	if s == nil {
		return
	}
	ok, errs, skipped := s.Counts()
	w.Counter("flodni_tracker_scrapes_total",
		"Tracker scrapes by result, per torrent and tracker: ok (answered), error (the request failed or the answer left the torrent out), skipped (no scrape URL: BEP 48). Experimental.",
		promSample(ok, lbl("result", "ok")), promSample(errs, lbl("result", "error")), promSample(skipped, lbl("result", "skipped")))
	zero, few, many := s.SeederBuckets()
	w.Gauge("flodni_torrents_by_seeders",
		"Torrents by the seeders their latest tracker scrape reported (best of their trackers). Torrents without numbers are in no bucket. Experimental.",
		promSample(int64(zero), lbl("bucket", "0")), promSample(int64(few), lbl("bucket", "1-2")), promSample(int64(many), lbl("bucket", "3+")))
	running := int64(0)
	if s.Running() {
		running = 1
	}
	w.Gauge("flodni_tracker_scrape_pass_running", "1 while a tracker scrape pass runs. Experimental.", promSample(running))
	if p := s.LastFullPass(); p != nil {
		w.Gauge("flodni_tracker_scrape_last_pass_finished_timestamp_seconds",
			"Unix time the last finished tracker scrape pass over every torrent ended. Absent before the first. Experimental.",
			promtext.Sample{Value: float64(p.FinishedAt.Unix())})
	}
}
