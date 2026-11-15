package daemon

import (
	"bytes"
	"flodni/internal/flog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"flodni/internal/config"
	"flodni/internal/library"
	"flodni/internal/metadb"
	"flodni/internal/promtext"
	"flodni/internal/warmer"
	"flodni/internal/warmup"
)

// ---- policy: evict only with evidence of plays ----

func TestWarmPolicyNeedsTheStoreAndAPlay(t *testing.T) {
	t.Run("no store, no play", func(t *testing.T) {
		x := warmIndex(t)
		if got := x.WarmPolicy(); got != warmup.EvictNone {
			t.Fatalf("policy = %v, want EvictNone", got)
		}
	})
	t.Run("a play with no store is no evidence", func(t *testing.T) {
		x := warmIndex(t)
		x.MarkPlayed(fk("aa", 1))
		if got := x.WarmPolicy(); got != warmup.EvictNone {
			t.Fatalf("policy = %v, want EvictNone: the state database is off", got)
		}
	})
	t.Run("store attached, no play ever", func(t *testing.T) {
		x := warmIndex(t)
		if err := x.attach(&fakeWarmStore{rows: []metadb.FileState{{Hash: "aa", FileIdx: 1, EvictedAt: 5}}}); err != nil {
			t.Fatal(err)
		}
		if got := x.WarmPolicy(); got != warmup.EvictNone {
			t.Fatalf("policy = %v, want EvictNone: nobody has played anything", got)
		}
	})
	t.Run("store attached with a stored play", func(t *testing.T) {
		x := warmIndex(t)
		if err := x.attach(&fakeWarmStore{rows: []metadb.FileState{{Hash: "aa", FileIdx: 1, LastPlayedAt: 5}}}); err != nil {
			t.Fatal(err)
		}
		if got := x.WarmPolicy(); got != warmup.EvictNeverPlayed {
			t.Fatalf("policy = %v, want EvictNeverPlayed", got)
		}
	})
	t.Run("store attached, then a play", func(t *testing.T) {
		x := warmIndex(t)
		if err := x.attach(&fakeWarmStore{}); err != nil {
			t.Fatal(err)
		}
		x.MarkPlayed(fk("aa", 1))
		if got := x.WarmPolicy(); got != warmup.EvictNeverPlayed {
			t.Fatalf("policy = %v, want EvictNeverPlayed", got)
		}
	})
	t.Run("a play before the attach counts once it is attached", func(t *testing.T) {
		x := warmIndex(t)
		x.MarkPlayed(fk("aa", 1))
		if err := x.attach(&fakeWarmStore{}); err != nil {
			t.Fatal(err)
		}
		if got := x.WarmPolicy(); got != warmup.EvictNeverPlayed {
			t.Fatalf("policy = %v, want EvictNeverPlayed", got)
		}
	})
}

// ---- EvictedSince: the eviction time in memory ----

func TestEvictedSinceUsesTheTimeOfTheLastEviction(t *testing.T) {
	x := warmIndex(t)
	k := fk("AA", 1)
	if x.EvictedSince(k, wt0.Add(-time.Hour)) {
		t.Fatal("a file never evicted reports an eviction")
	}
	x.OnEvicted([]warmup.Eviction{{Key: k}})
	if !x.EvictedSince(fk("aa", 1), wt0.Add(-time.Second)) || !x.EvictedSince(k, wt0) {
		t.Fatal("an eviction at wt0 is not newer than an attempt that began before it")
	}
	if x.EvictedSince(k, wt0.Add(time.Hour)) {
		t.Fatal("an eviction older than the attempt counted: the mark can be weeks old")
	}
	if x.EvictedSince(fk("aa", 2), wt0.Add(-time.Hour)) {
		t.Fatal("another file's eviction counted")
	}
}

func TestEvictedSinceSurvivesTheReloadFromTheStore(t *testing.T) {
	x := warmIndex(t)
	if err := x.attach(&fakeWarmStore{rows: []metadb.FileState{{Hash: "aa", FileIdx: 1, EvictedAt: wt0.Unix() - 100}}}); err != nil {
		t.Fatal(err)
	}
	if !x.EvictedSince(fk("aa", 1), wt0.Add(-time.Hour)) || x.EvictedSince(fk("aa", 1), wt0) {
		t.Fatal("the stored eviction time is not used")
	}
}

// ---- playbackOpen: what pauses the warmer ----

func playbackOpenSetup(t *testing.T) {
	newTestApp(t)
	t.Helper()
	prevNow := playbackOpenNow
	now := time.Unix(1_800_000_000, 0)
	playbackOpenNow = func() time.Time { return now }
	t.Cleanup(func() {
		playbackOpenNow = prevNow
		app.playbackRegistry.Range(func(k, _ any) bool { app.playbackRegistry.Delete(k); return true })
		app.activePumps.Range(func(k, _ any) bool { app.activePumps.Delete(k); return true })
		playbackOpenCache.mu.Lock()
		playbackOpenCache.at = time.Time{}
		playbackOpenCache.mu.Unlock()
	})
	playbackOpenCache.mu.Lock()
	playbackOpenCache.at = time.Time{}
	playbackOpenCache.mu.Unlock()
}

func TestPlaybackOpen(t *testing.T) {
	inferred := func() *PlaybackState {
		return &PlaybackState{Path: "/m/a.mkv", ReadCount: 5, LastSeekOff: 10 * mb, LastReadAt: time.Now()}
	}
	cases := []struct {
		name   string
		pump   bool
		state  *PlaybackState
		handle bool
		want   bool
	}{
		{"nothing is open", false, nil, false, false},
		{"a pump is running", true, nil, false, true},
		{"confirmed playback with a live handle", false, &PlaybackState{Path: "/m/a.mkv", IsHealthy: true}, true, true},
		{"confirmed playback restored at start, no handle", false, &PlaybackState{Path: "/m/a.mkv", IsHealthy: true}, false, false},
		{"inferred playback with a live handle", false, inferred(), true, true},
		{"inferred playback, no handle", false, inferred(), false, false},
		{"a stopped session", false, &PlaybackState{Path: "/m/a.mkv", IsStopped: true, ReadCount: 5, LastSeekOff: 10 * mb, LastReadAt: time.Now()}, true, false},
		{"a scan: a handle and a state that is neither confirmed nor inferred", false, &PlaybackState{Path: "/m/a.mkv", ReadCount: 1}, true, false},
		{"a handle alone", false, nil, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			playbackOpenSetup(t)
			if c.pump {
				app.activePumps.Store("/m/a.mkv", &NativePumpState{})
			}
			if c.state != nil {
				app.playbackRegistry.Store("/m/a.mkv", c.state)
			}
			if c.handle {
				registerHandle(t, newTestHandle("/m/a.mkv"))
			}
			if got := playbackOpen(); got != c.want {
				t.Fatalf("playbackOpen() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestPlaybackOpenIsCachedForOneSecond(t *testing.T) {
	playbackOpenSetup(t)
	now := time.Unix(1_800_000_000, 0)
	playbackOpenNow = func() time.Time { return now }
	if playbackOpen() {
		t.Fatal("open with nothing running")
	}
	app.activePumps.Store("/m/a.mkv", &NativePumpState{})
	now = now.Add(900 * time.Millisecond)
	if playbackOpen() {
		t.Fatal("the answer changed inside the one-second cache")
	}
	now = now.Add(200 * time.Millisecond)
	if !playbackOpen() {
		t.Fatal("the answer is still stale after a second")
	}
}

// ---- the library hook ----

type fillSpy struct {
	mu   sync.Mutex
	reqs []warmup.FillRequest
}

func (s *fillSpy) fill(req warmup.FillRequest) (warmup.FillResult, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	return warmup.FillResult{AlreadyWarm: true}, nil
}

func (s *fillSpy) n() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.reqs) }

// spyWarmer installs a warmer built from the real wiring (addWarmerDeps) with the engine
// side replaced: a live torrent, no playback, and a fill that records its request.
func spyWarmer(t *testing.T, x *fileStateIndex) *fillSpy {
	t.Helper()
	spy := &fillSpy{}
	stop := make(chan struct{})
	d := addWarmerDeps(x, stop)
	d.Fill = spy.fill
	d.Log = func(string) {} // the worker can log after the test restored the package logger
	d.Live = func(string) bool { return true }
	d.PlaybackOpen = func() bool { return false }
	prev := addWarmer
	addWarmer = warmer.New(d)
	t.Cleanup(func() { close(stop); addWarmer = prev })
	return spy
}

func withConfig(t *testing.T, mutate func(*config.Config)) *bytes.Buffer {
	t.Helper()
	useTestGlobals(t)
	cfg := *gc()
	mutate(&cfg)
	globalConfig.Store(&cfg)
	var buf bytes.Buffer
	logger = flog.NewText(&buf)
	warnWarmOnAddNoDir = sync.Once{}
	return &buf
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestOnAddedEnqueuesTheFilesOnlyWhenTheSwitchAndTheDirAreSet(t *testing.T) {
	files := []library.AddedFile{{FileIndex: 3, Size: 100}, {FileIndex: 5, Size: 200}}

	t.Run("on, with warmup_dir", func(t *testing.T) {
		withConfig(t, func(c *config.Config) { c.WarmOnAdd, c.WarmupDir = true, "/cache" })
		spy := spyWarmer(t, warmIndex(t))
		onLibraryAdded("AABB", files)
		waitFor(t, "two fills", func() bool { return spy.n() == 2 })
		spy.mu.Lock()
		defer spy.mu.Unlock()
		got := map[int]int64{}
		for _, r := range spy.reqs {
			if r.Key.Hash != "aabb" || r.Trigger != warmup.TriggerWarmAdd {
				t.Fatalf("request = %+v", r)
			}
			got[r.Key.FileIdx] = r.FileSize
		}
		if len(got) != 2 || got[3] != 100 || got[5] != 200 {
			t.Fatalf("files = %v, want the engine file ids with their sizes", got)
		}
	})
	t.Run("off", func(t *testing.T) {
		withConfig(t, func(c *config.Config) { c.WarmOnAdd, c.WarmupDir = false, "/cache" })
		spy := spyWarmer(t, warmIndex(t))
		onLibraryAdded("aabb", files)
		time.Sleep(50 * time.Millisecond)
		if spy.n() != 0 {
			t.Fatal("warmed with the switch off")
		}
	})
	t.Run("the switch is read at each add", func(t *testing.T) {
		withConfig(t, func(c *config.Config) { c.WarmOnAdd, c.WarmupDir = false, "/cache" })
		spy := spyWarmer(t, warmIndex(t))
		onLibraryAdded("aabb", files)
		cfg := *gc()
		cfg.WarmOnAdd = true
		globalConfig.Store(&cfg)
		onLibraryAdded("ccdd", files)
		waitFor(t, "fills for the second add", func() bool { return spy.n() == 2 })
	})
}

func TestWarmOnAddWithoutAWarmupDirWarnsOnceAndDoesNothing(t *testing.T) {
	buf := withConfig(t, func(c *config.Config) { c.WarmOnAdd, c.WarmupDir = true, "" })
	spy := spyWarmer(t, warmIndex(t))
	files := []library.AddedFile{{FileIndex: 1, Size: 100}}
	onLibraryAdded("aabb", files)
	onLibraryAdded("ccdd", files)
	time.Sleep(50 * time.Millisecond)
	if spy.n() != 0 {
		t.Fatal("warmed without warmup_dir: the windows would have nowhere to live")
	}
	if got := strings.Count(buf.String(), "warm_on_add"); got != 1 {
		t.Fatalf("warnings = %d, want exactly one:\n%s", got, buf)
	}
}

// ---- removal: cancel the warmer, forget the records ----

func TestHashRemovalCancelsTheWarmerAndForgetsTheRecords(t *testing.T) {
	withConfig(t, func(c *config.Config) { c.WarmOnAdd, c.WarmupDir = true, "/cache" })
	x := warmIndex(t)
	gate := make(chan struct{})
	stop := make(chan struct{})
	d := addWarmerDeps(x, stop)
	d.Workers = 1
	d.Log = func(string) {}
	d.Live = func(string) bool { return true }
	d.PlaybackOpen = func() bool { return false }
	d.Fill = func(warmup.FillRequest) (warmup.FillResult, error) {
		<-gate
		return warmup.FillResult{AlreadyWarm: true}, nil
	}
	w := warmer.New(d)
	t.Cleanup(func() { close(gate); close(stop) })

	x.RecordWarm(warmUpdate{key: fk("aabb", 1), readability: "warmed", cache: "cached", deliveredAt: wt0})
	w.Add("ccdd", []warmer.File{{Index: 1, Size: 10}})
	waitFor(t, "the worker to take ccdd", func() bool { return w.Stats().QueueFiles == 0 })
	w.Add("aabb", []warmer.File{{Index: 1, Size: 10}, {Index: 2, Size: 10}})
	if w.Stats().QueueFiles != 2 {
		t.Fatalf("queue = %d, want 2", w.Stats().QueueFiles)
	}
	onHashRemoved(w, x)("AABB")
	if got := w.Stats().QueueFiles; got != 0 {
		t.Fatalf("queue = %d after the removal", got)
	}
	if len(x.WarmSnapshot()["aabb"]) != 0 {
		t.Fatal("the record of a removed torrent was kept")
	}
}

// ---- metrics ----

func TestAddWarmerSeriesStartAtZero(t *testing.T) {
	var w promtext.Writer
	st := warmer.Stats{
		Fills:     map[string]int64{warmer.OutcomeWarmed: 5, warmer.OutcomeLost: 1},
		HeadBytes: 130,
		TailBytes: 26,
		ByTrigger: map[string]warmer.TriggerStats{
			warmer.Trigger: {Fills: map[string]int64{warmer.OutcomeWarmed: 2, warmer.OutcomeLost: 1}, HeadBytes: 100, TailBytes: 20},
			warmer.TriggerCatchup: {Fills: map[string]int64{warmer.OutcomeWarmed: 3, warmer.OutcomeNoRoom: 4},
				HeadBytes: 30, TailBytes: 6},
		},
		CacheFailures: map[string]int64{"lost": 1},
		QueueFiles:    7,
		Paused:        true,
	}
	w.Counter("flodni_warm_fills_total", "h", addWarmFillSamples(st)...)
	collectAddWarmProm(&w, st)
	if err := w.Err(); err != nil {
		t.Fatal(err)
	}
	if err := promtext.Lint(w.Bytes()); err != nil {
		t.Fatalf("lint: %v\n%s", err, w.Bytes())
	}
	out := string(w.Bytes())
	for _, o := range warmer.Outcomes {
		want := `flodni_warm_fills_total{trigger="add",outcome="` + o + `"} `
		if !strings.Contains(out, want) {
			t.Errorf("series for outcome %s missing", o)
		}
	}
	for _, want := range []string{
		`flodni_warm_fills_total{trigger="add",outcome="warmed"} 2`,
		`flodni_warm_fills_total{trigger="add",outcome="present"} 0`,
		`flodni_warm_fills_total{trigger="add",outcome="dropped_queue_full"} 0`,
		`flodni_warm_fill_bytes_total{trigger="add",window="head"} 100`,
		`flodni_warm_fill_bytes_total{trigger="add",window="tail"} 20`,
		`flodni_warm_fills_total{trigger="catchup",outcome="warmed"} 3`,
		`flodni_warm_fills_total{trigger="catchup",outcome="no_room"} 4`,
		`flodni_warm_fills_total{trigger="catchup",outcome="lost"} 0`,
		`flodni_warm_fill_bytes_total{trigger="catchup",window="head"} 30`,
		`flodni_warm_fill_bytes_total{trigger="catchup",window="tail"} 6`,
		`flodni_warm_cache_failures_total{reason="lost"} 1`,
		`flodni_warm_cache_failures_total{reason="write_failed"} 0`,
		`flodni_warm_queue_files 7`,
		`flodni_warm_paused 1`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestScrapeCarriesTheWarmerSeriesAtZeroWithNoWarmer(t *testing.T) {
	useTestGlobals(t)
	prev := addWarmer
	addWarmer = nil
	t.Cleanup(func() { addWarmer = prev })
	body := scrape(t).Body.String()
	for _, want := range []string{
		`flodni_warm_fills_total{trigger="mount",outcome="warmed"} `, // the mount series stays
		`flodni_warm_fills_total{trigger="add",outcome="warmed"} 0`,
		`flodni_warm_fills_total{trigger="add",outcome="wake_error"} 0`,
		`flodni_warm_fill_bytes_total{trigger="add",window="head"} 0`,
		`flodni_warm_fills_total{trigger="catchup",outcome="warmed"} 0`,
		`flodni_warm_fills_total{trigger="catchup",outcome="dropped_queue_full"} 0`,
		`flodni_warm_fill_bytes_total{trigger="catchup",window="head"} 0`,
		`flodni_warm_fill_bytes_total{trigger="catchup",window="tail"} 0`,
		`flodni_warm_catchup_files{state="pending"} 0`,
		`flodni_warm_catchup_files{state="left_no_room"} 0`,
		`flodni_warm_cache_failures_total{reason="write_failed"} 0`,
		`flodni_warm_queue_files 0`,
		`flodni_warm_paused 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("series %q missing", want)
		}
	}
}

// ---- the recording side ----

func TestWarmerRecordsReachTheFileIndex(t *testing.T) {
	x := warmIndex(t)
	d := addWarmerDeps(x, make(chan struct{}))
	d.Record(warmer.Update{Key: fk("AA", 4), Readability: warmer.ReadWarmed, Cache: warmer.CacheCached, DeliveredAt: wt0.Add(-time.Hour), AttemptAt: wt0})
	got := snap(t, x, "aa", 4)
	if got.Readability != "warmed" || got.Cache != "cached" || !reflect.DeepEqual(got.LastAttemptAt, ts(wt0.Unix())) || !reflect.DeepEqual(got.LastSwarmDeliveryAt, ts(wt0.Add(-time.Hour).Unix())) {
		t.Fatalf("record = %+v", got)
	}
	if d.EvictedSince(fk("aa", 4), wt0) {
		t.Fatal("EvictedSince true for a file never evicted")
	}
	if d.Policy() != warmup.EvictNone {
		t.Fatal("policy without evidence must be EvictNone")
	}
}
