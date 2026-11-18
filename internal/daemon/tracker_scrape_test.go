package daemon

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flodni/internal/flog"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"flodni/internal/config"
	"flodni/internal/gostorm/torr"
	"flodni/internal/metadb"
	trackscrape "flodni/internal/scrape"
)

func hashOf(n byte) [20]byte {
	var h [20]byte
	for i := range h {
		h[i] = n + byte(i)
	}
	return h
}

func hexOf(h [20]byte) string { return hex.EncodeToString(h[:]) }

// scrapeTracker is a local tracker: it answers the hashes it has, or the failure
// reason, and can hold a request until released.
type scrapeTracker struct {
	srv     *httptest.Server
	answers map[[20]byte][2]int
	failure string
	gate    chan struct{}
}

func newScrapeTracker(t *testing.T, answers map[[20]byte][2]int) *scrapeTracker {
	t.Helper()
	st := &scrapeTracker{answers: answers}
	st.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if st.gate != nil {
			<-st.gate
		}
		if st.failure != "" {
			fmt.Fprintf(w, "d14:failure reason%d:%se", len(st.failure), st.failure)
			return
		}
		var b strings.Builder
		b.WriteString("d5:filesd")
		for _, kv := range strings.Split(r.URL.RawQuery, "&") {
			v, ok := strings.CutPrefix(kv, "info_hash=")
			if !ok {
				continue
			}
			raw, _ := url.QueryUnescape(v)
			var h [20]byte
			copy(h[:], raw)
			if c, ok := st.answers[h]; ok {
				fmt.Fprintf(&b, "20:%sd8:completei%de10:incompletei%dee", h[:], c[0], c[1])
			}
		}
		b.WriteString("ee")
		_, _ = w.Write([]byte(b.String()))
	}))
	t.Cleanup(st.srv.Close)
	return st
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

var notReady atomic.Bool

func useScraper(t *testing.T, cfg config.Config, targets []trackscrape.Target) (*trackscrape.Scraper, *syncBuf) {
	t.Helper()
	logs := &syncBuf{}
	stop := make(chan struct{})
	// the targets are the stored torrents (the health listing checks the store)
	storedHashes = func() map[string]bool {
		m := map[string]bool{}
		for _, t := range targets {
			m[hexOf(t.Hash)] = true
		}
		return m
	}
	s := startTrackerScraper(&cfg, func(only *[20]byte) []trackscrape.Target {
		var out []trackscrape.Target
		for _, t := range targets {
			if only == nil || t.Hash == *only {
				out = append(out, t)
			}
		}
		return out
	}, func() bool { return !notReady.Load() }, torr.ScrapeNet{UserAgent: "test-ua", UDP: true}, stop, flog.NewText(logs))
	t.Cleanup(func() { close(stop); s.Stop(); trackerScraper.Store(nil); storedHashes = torr.StoredHashes })
	return s, logs
}

func fastCfg() config.Config {
	return config.Config{TrackerScrapeHours: 0, TrackerScrapeBatch: 100, TrackerScrapePaceMs: 100}
}

func post(target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handleScrapeTrigger(rec, httptest.NewRequest(http.MethodPost, target, nil))
	return rec
}

func status(t *testing.T) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	handleScrapeStatus(rec, httptest.NewRequest(http.MethodGet, "/api/torrents/scrape/status", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func waitState(t *testing.T, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m := status(t); m["state"] == want {
			return m
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pass never reached state %q", want)
	return nil
}

func healthJSON(t *testing.T, store readabilityLister) (map[string]any, string) {
	t.Helper()
	h := newTorrentsHealthHandler(store, nil, flog.NewText(io.Discard))
	rec := getHealth(h, http.MethodGet, "/api/torrents/health")
	if rec.Code != 200 {
		t.Fatalf("health %d: %s", rec.Code, rec.Body)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m, rec.Body.String()
}

func torrentOf(t *testing.T, m map[string]any, hash string) map[string]any {
	t.Helper()
	for _, x := range m["torrents"].([]any) {
		if tt := x.(map[string]any); tt["hash"] == hash {
			return tt
		}
	}
	t.Fatalf("hash %s not in health", hash)
	return nil
}

// POST answers 202 with a run id and does not wait; the status moves running -> done; a second
// POST while it runs is a 409 naming the running pass; health then carries scrape_pass and each
// torrent's run_id.
func TestScrapeTriggerStatusOverlapAndHealth(t *testing.T) {
	a, b := hashOf(1), hashOf(2)
	tr := newScrapeTracker(t, map[[20]byte][2]int{a: {5, 2}}) // b is left out of the answer
	tr.gate = make(chan struct{})
	s, _ := useScraper(t, fastCfg(), []trackscrape.Target{
		{Hash: a, Trackers: []string{tr.srv.URL + "/announce"}},
		{Hash: b, Trackers: []string{tr.srv.URL + "/announce"}},
	})
	_ = s
	if m := status(t); m["state"] != "none" {
		t.Fatalf("before any pass: %v", m)
	}
	if m, _ := healthJSON(t, &fakeLister{}); m["scrape_pass"] != nil {
		t.Fatalf("scrape_pass before a finished pass = %v", m["scrape_pass"])
	}

	rec := post("/api/torrents/scrape")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body)
	}
	var acc map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &acc)
	if acc["run_id"] != float64(1) || acc["started_at"] == nil {
		t.Fatalf("202 body = %v", acc)
	}
	st := status(t)
	if st["state"] != "running" || st["finished_at"] != nil || st["run_id"] != float64(1) || st["trigger"] != "manual" || st["torrents"] != float64(2) {
		t.Fatalf("running status = %v", st)
	}
	conflict := post("/api/torrents/scrape")
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), `"run_id":1`) {
		t.Fatalf("overlap = %d %s", conflict.Code, conflict.Body)
	}
	var sm string
	rec2 := httptest.NewRecorder()
	handlePrometheus(rec2, httptest.NewRequest(http.MethodGet, "/metrics/prometheus", nil))
	sm = rec2.Body.String()
	if !strings.Contains(sm, "flodni_tracker_scrape_pass_running 1") {
		t.Fatal("pass_running gauge should be 1 while a pass runs")
	}

	close(tr.gate)
	done := waitState(t, "done")
	if done["finished_at"] == nil || done["ok"] != float64(1) || done["errors"] != float64(1) || done["skipped"] != float64(0) {
		t.Fatalf("done status = %v", done)
	}

	m, _ := healthJSON(t, &fakeLister{rows: []metadb.Readability{{Hash: hexOf(a)}, {Hash: hexOf(b)}}})
	sp := m["scrape_pass"].(map[string]any)
	if sp["run_id"] != float64(1) || sp["finished_at"] == nil {
		t.Fatalf("scrape_pass = %v", sp)
	}
	ta := torrentOf(t, m, hexOf(a))["scrape"].(map[string]any)
	if ta["seeders"] != float64(5) || ta["leechers"] != float64(2) || ta["error"] != nil || ta["run_id"] != float64(1) || ta["scraped_at"] == nil {
		t.Fatalf("a scrape = %v", ta)
	}
	// An omitted hash is an error with null numbers, never 0.
	tb := torrentOf(t, m, hexOf(b))["scrape"].(map[string]any)
	if tb["seeders"] != nil || tb["leechers"] != nil || tb["error"] == nil || tb["run_id"] != float64(1) {
		t.Fatalf("b scrape = %v", tb)
	}
	if tb["answered"] != float64(0) || tb["unknown"] != float64(1) || tb["failed"] != float64(0) || tb["unscrapable"] != float64(0) ||
		ta["answered"] != float64(1) || ta["failed"] != float64(0) {
		t.Fatalf("facts: %v / %v", ta, tb)
	}
	if tb["error"] != "unknown_to_tracker" || tb["source"] != nil || ta["source"] != "http" {
		t.Fatalf("an omitted hash is unknown, not a failure and not 0: %v / %v", tb, ta)
	}

	// the next pass has run id 2 and the previous finished one is shown
	if rec := post("/api/torrents/scrape?hash=" + hexOf(a)); rec.Code != http.StatusAccepted {
		t.Fatalf("single POST = %d %s", rec.Code, rec.Body)
	}
	st2 := waitState(t, "done")
	prev := st2["previous"].(map[string]any)
	if st2["run_id"] != float64(2) || st2["scope"] != "one" || prev["run_id"] != float64(1) {
		t.Fatalf("status = %v", st2)
	}
	// a single-torrent pass is not a full pass; b keeps its old entry and run id
	m, _ = healthJSON(t, &fakeLister{})
	if m["scrape_pass"].(map[string]any)["run_id"] != float64(1) {
		t.Fatalf("scrape_pass moved on a one-torrent pass: %v", m["scrape_pass"])
	}
	if torrentOf(t, m, hexOf(b))["scrape"].(map[string]any)["run_id"] != float64(1) {
		t.Fatal("an unreached torrent must keep its run_id")
	}

	// metrics
	rec3 := httptest.NewRecorder()
	handlePrometheus(rec3, httptest.NewRequest(http.MethodGet, "/metrics/prometheus", nil))
	for _, want := range []string{
		`flodni_tracker_scrapes_total{result="ok"} 2`,
		`flodni_tracker_scrapes_total{result="error"} 1`,
		`flodni_tracker_scrapes_total{result="skipped"} 0`,
		`flodni_torrents_by_seeders{bucket="3+"} 1`,
		`flodni_torrents_by_seeders{bucket="0"} 0`,
		`flodni_tracker_scrape_pass_running 0`,
		`flodni_tracker_scrape_last_pass_finished_timestamp_seconds `,
	} {
		if !strings.Contains(rec3.Body.String(), want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

// Before the torrent store is open, a pass is refused with 503 instead of reading a nil store.
func TestScrapeRefusedUntilStoreReady(t *testing.T) {
	notReady.Store(true)
	t.Cleanup(func() { notReady.Store(false) })
	useScraper(t, fastCfg(), []trackscrape.Target{{Hash: hashOf(1)}})
	if rec := post("/api/torrents/scrape"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not ready = %d", rec.Code)
	}
	notReady.Store(false)
	if rec := post("/api/torrents/scrape"); rec.Code != http.StatusAccepted {
		t.Fatalf("ready = %d", rec.Code)
	}
}

func TestScrapeTriggerRejections(t *testing.T) {
	if rec := post("/api/torrents/scrape"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no scraper = %d", rec.Code)
	}
	useScraper(t, fastCfg(), []trackscrape.Target{{Hash: hashOf(1)}})
	if rec := post("/api/torrents/scrape?hash=zz"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad hash = %d", rec.Code)
	}
	if rec := post("/api/torrents/scrape?hash=" + hexOf(hashOf(9))); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown hash = %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	handleScrapeTrigger(rec, httptest.NewRequest(http.MethodGet, "/api/torrents/scrape", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
		t.Fatalf("GET = %d", rec.Code)
	}
}

// tracker_scrape_hours 0 turns the schedule off: nothing runs by itself, a manual pass works.
func TestScrapeHoursZeroHasNoSchedule(t *testing.T) {
	a := hashOf(1)
	tr := newScrapeTracker(t, map[[20]byte][2]int{a: {1, 1}})
	cfg := fastCfg()
	cfg.TrackerScrapeHours = 0
	useScraper(t, cfg, []trackscrape.Target{{Hash: a, Trackers: []string{tr.srv.URL + "/announce"}}})
	time.Sleep(150 * time.Millisecond)
	if m := status(t); m["state"] != "none" {
		t.Fatalf("a pass ran by itself: %v", m)
	}
	if rec := post("/api/torrents/scrape"); rec.Code != http.StatusAccepted {
		t.Fatalf("manual = %d", rec.Code)
	}
	waitState(t, "done")
}

// The scrape settings reach the scraper: a batch of 2 over 5 torrents is 3 requests, at least
// the pace apart.
func TestScrapeSettingsReachTheScraper(t *testing.T) {
	answers := map[[20]byte][2]int{}
	var targets []trackscrape.Target
	var mu sync.Mutex
	var times []time.Time
	var sizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		times = append(times, time.Now())
		sizes = append(sizes, strings.Count(r.URL.RawQuery, "info_hash="))
		mu.Unlock()
		_, _ = w.Write([]byte("d5:filesdee"))
	}))
	defer srv.Close()
	for i := 0; i < 5; i++ {
		h := hashOf(byte(i * 2))
		answers[h] = [2]int{1, 1}
		targets = append(targets, trackscrape.Target{Hash: h, Trackers: []string{srv.URL + "/announce"}})
	}
	cfg := fastCfg()
	cfg.TrackerScrapeBatch = 2
	cfg.TrackerScrapePaceMs = 200
	useScraper(t, cfg, targets)
	post("/api/torrents/scrape")
	waitState(t, "done")
	mu.Lock()
	defer mu.Unlock()
	if len(sizes) != 3 || sizes[0] != 2 || sizes[1] != 2 || sizes[2] != 1 {
		t.Fatalf("request sizes = %v, want [2 2 1]", sizes)
	}
	for i := 1; i < len(times); i++ {
		if times[i].Sub(times[i-1]) < 190*time.Millisecond {
			t.Fatalf("gap %d = %v, want >= 200 ms", i, times[i].Sub(times[i-1]))
		}
	}
}

// An announce URL carries a passkey. Render every log line and the health JSON for torrents
// whose trackers echo it, hang, or answer, and the passkey appears nowhere.
func TestScrapePasskeyNeverLeaks(t *testing.T) {
	const passkey = "test-passkey-not-real"
	const auth = "test-token-not-real"
	a, b, c := hashOf(1), hashOf(2), hashOf(3)
	echo := newScrapeTracker(t, nil)
	echo.failure = "unknown passkey " + passkey + " auth " + auth
	live := newScrapeTracker(t, map[[20]byte][2]int{b: {3, 3}})
	cfg := fastCfg()
	_, logs := useScraper(t, cfg, []trackscrape.Target{
		{Hash: a, Trackers: []string{echo.srv.URL + "/" + passkey + "/announce?auth=" + auth}},
		{Hash: b, Trackers: []string{live.srv.URL + "/" + passkey + "/announce?auth=" + auth}},
		{Hash: c, Trackers: []string{"http://127.0.0.1:1/" + passkey + "/announce?auth=" + auth, "http://tracker.example.org/" + passkey + "/nope"}},
	})
	post("/api/torrents/scrape")
	waitState(t, "done")

	m, body := healthJSON(t, &fakeLister{rows: []metadb.Readability{{Hash: hexOf(a)}, {Hash: hexOf(b)}, {Hash: hexOf(c)}}})
	rec := httptest.NewRecorder()
	handlePrometheus(rec, httptest.NewRequest(http.MethodGet, "/metrics/prometheus", nil))
	st, _ := json.Marshal(status(t))
	all := logs.String() + "\n" + body + "\n" + rec.Body.String() + "\n" + string(st)
	for _, secret := range []string{passkey, auth, "info_hash", "/nope"} {
		if strings.Contains(all, secret) {
			t.Fatalf("%q leaked:\n%s", secret, all)
		}
	}
	if e := torrentOf(t, m, hexOf(a))["scrape"].(map[string]any)["error"].(string); !strings.Contains(e, echo.srv.URL) || !strings.Contains(e, "***") {
		t.Fatalf("scrubbed error = %q", e)
	}
	if !strings.Contains(logs.String(), echo.srv.URL) {
		t.Fatal("the log should name the host")
	}
}

// A pass that started while a torrent was stored can finish after the torrent was refused
// or dropped. Its entry is then an orphan: health must not list it. A stored torrent keeps
// its scrape object (and the pinned fields) as before.
func TestHealthListsNoScrapeEntryOfAnUnstoredTorrent(t *testing.T) {
	stored, gone := hashOf(1), hashOf(2)
	tr := newScrapeTracker(t, map[[20]byte][2]int{stored: {5, 2}, gone: {3, 1}})
	useScraper(t, fastCfg(), []trackscrape.Target{
		{Hash: stored, Trackers: []string{tr.srv.URL + "/announce"}},
		{Hash: gone, Trackers: []string{tr.srv.URL + "/announce"}},
	})
	if rec := post("/api/torrents/scrape"); rec.Code != http.StatusAccepted {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body)
	}
	waitState(t, "done")
	// the torrent is refused after the pass took its snapshot; the result still landed
	storedHashes = func() map[string]bool { return map[string]bool{hexOf(stored): true} }

	m, body := healthJSON(t, &fakeLister{})
	if strings.Contains(body, hexOf(gone)) {
		t.Fatalf("health lists a torrent that is not stored: %s", body)
	}
	if sc := torrentOf(t, m, hexOf(stored))["scrape"].(map[string]any); sc["seeders"] != float64(5) || sc["run_id"] != float64(1) {
		t.Fatalf("stored torrent's scrape = %v", sc)
	}
}
