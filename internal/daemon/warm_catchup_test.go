package daemon

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"flodni/internal/config"
	"flodni/internal/library"
	"flodni/internal/metadb"
	"flodni/internal/promtext"
	"flodni/internal/warmer"
	"flodni/internal/warmup"
)

// ---- the predicate: one row of the table per case ----

func TestCatchupStateTable(t *testing.T) {
	now := wt0
	ago := func(d time.Duration) int64 { return now.Add(-d).Unix() }
	cases := []struct {
		name string
		in   catchupFile
		want string
	}{
		{"no record", catchupFile{}, statePending},
		{"both windows on disk, no record", catchupFile{complete: true}, stateWarm},
		{"both windows on disk, an eviction mark does not matter", catchupFile{complete: true, evicted: true}, stateWarm},
		{"both windows on disk, a failed record does not matter",
			catchupFile{complete: true, warm: fileWarm{readability: readFailed, failedAttempts: 9, lastAttemptAt: ago(time.Hour)}}, stateWarm},
		{"cache evicted", catchupFile{warm: fileWarm{cache: cacheEvicted}}, stateEvicted},
		{"eviction mark with no cache state (schema 13)", catchupFile{evicted: true}, stateEvicted},
		{"eviction mark beats a failed record",
			catchupFile{evicted: true, warm: fileWarm{readability: readFailed, failedAttempts: 5}}, stateEvicted},
		{"failed 4 times", catchupFile{warm: fileWarm{readability: readFailed, failedAttempts: 4, lastAttemptAt: ago(100 * time.Hour)}}, stateGaveUp},
		{"failed 4 times and just now", catchupFile{warm: fileWarm{readability: readFailed, failedAttempts: 4, lastAttemptAt: ago(time.Minute)}}, stateGaveUp},
		{"failed 3 times, long ago", catchupFile{warm: fileWarm{readability: readFailed, failedAttempts: 3, lastAttemptAt: ago(21 * time.Hour)}}, statePending},
		{"failed 3 times, 19 h ago", catchupFile{warm: fileWarm{readability: readFailed, failedAttempts: 3, lastAttemptAt: ago(19 * time.Hour)}}, stateRetryWait},
		{"attempted 1 h ago, whatever the outcome",
			catchupFile{warm: fileWarm{cache: "dropped_quota", lastAttemptAt: ago(time.Hour)}}, stateRetryWait},
		{"attempted 21 h ago, dropped for quota", catchupFile{warm: fileWarm{cache: "dropped_quota", lastAttemptAt: ago(21 * time.Hour)}}, statePending},
		{"dropped for no evidence", catchupFile{warm: fileWarm{cache: "dropped_no_evidence"}}, statePending},
		{"lost", catchupFile{warm: fileWarm{cache: "lost", lastAttemptAt: ago(25 * time.Hour)}}, statePending},
		{"write_failed", catchupFile{warm: fileWarm{cache: "write_failed"}}, statePending},
		{"the record says cached while the disk does not",
			catchupFile{warm: fileWarm{readability: readWarmed, cache: cacheCached, lastAttemptAt: ago(48 * time.Hour)}}, statePending},
		{"a sibling failed 1 h ago: the whole hash waits", catchupFile{hashFailedRecently: true}, stateRetryWait},
		{"a sibling failed, this one is evicted", catchupFile{hashFailedRecently: true, evicted: true}, stateEvicted},
		{"a sibling failed, this one is warm", catchupFile{hashFailedRecently: true, complete: true}, stateWarm},
		{"refused by WakeStored", catchupFile{notStored: true}, stateNotStored},
		{"refused, and recently attempted: the wait comes first",
			catchupFile{notStored: true, warm: fileWarm{lastAttemptAt: ago(time.Hour)}}, stateRetryWait},
	}
	for _, c := range cases {
		if got := catchupState(c.in, now); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestFailedRecentlyIsAFailedRecordUnderTwentyHoursOld(t *testing.T) {
	now := wt0
	if !failedRecently(fileWarm{readability: readFailed, lastAttemptAt: now.Add(-time.Hour).Unix()}, now) {
		t.Fatal("a failure an hour ago is recent")
	}
	if failedRecently(fileWarm{readability: readFailed, lastAttemptAt: now.Add(-21 * time.Hour).Unix()}, now) {
		t.Fatal("a failure 21 h ago is not recent")
	}
	if failedRecently(fileWarm{readability: readWarmed, lastAttemptAt: now.Unix()}, now) {
		t.Fatal("a warmed attempt is not a failure")
	}
}

func TestCatchupFactReadsTheEvictionMarkAndTheEvictionTimeFromMemory(t *testing.T) {
	x := warmIndex(t)
	if _, ev := x.CatchupFact(fk("AA", 1)); ev {
		t.Fatal("evicted for a file the index never saw")
	}
	x.OnEvicted([]warmup.Eviction{{Key: fk("AA", 1)}})
	w, ev := x.CatchupFact(fk("aa", 1))
	if !ev || w.cache != cacheEvicted {
		t.Fatalf("fact = %+v, %v after an eviction", w, ev)
	}
	// A partial play clears the mark but not the time of the eviction.
	x.ClearEvicted(fk("aa", 1))
	if _, ev := x.CatchupFact(fk("aa", 1)); !ev {
		t.Fatal("the eviction time must still exclude the file")
	}
}

func TestCatchupFactSeesAnEvictionMarkLoadedFromSchema13(t *testing.T) {
	// A row of the old shape: evicted_at only, no cache state.
	st := &fakeWarmStore{rows: []metadb.FileState{{Hash: "aa", FileIdx: 2, EvictedAt: 1700000000}}}
	x := warmIndex(t)
	if err := x.attach(st); err != nil {
		t.Fatal(err)
	}
	w, ev := x.CatchupFact(fk("aa", 2))
	if !ev || w.cache != "" {
		t.Fatalf("fact = %+v, evicted %v: want the mark with no cache state", w, ev)
	}
	if got := catchupState(catchupFile{warm: w, evicted: ev}, wt0); got != stateEvicted {
		t.Fatalf("state = %s", got)
	}
}

// ---- a pass, with a fake warmer ----

type fakeTarget struct {
	mu        sync.Mutex
	fed       []string // "hash/idx,idx" in feed order
	notStored map[string]bool
	noRoomAt  string // set the no-room flag when this hash is fed
	noRoom    bool
	holding   bool
	hold      chan struct{} // set: after the first feed Idle is false until it is closed
}

func (f *fakeTarget) AddCatchup(hash string, files []warmer.File) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var idx []string
	for _, x := range files {
		idx = append(idx, string(rune('0'+x.Index)))
	}
	f.fed = append(f.fed, hash+"/"+strings.Join(idx, ","))
	f.holding = f.hold != nil
	if hash == f.noRoomAt {
		f.noRoom = true
	}
	return len(files)
}
func (f *fakeTarget) Idle() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.holding {
		return true
	}
	select {
	case <-f.hold:
		return true
	default:
		return false
	}
}
func (f *fakeTarget) NoRoom() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.noRoom }
func (f *fakeTarget) ResetNoRoom() { f.mu.Lock(); f.noRoom = false; f.mu.Unlock() }
func (f *fakeTarget) NotStored(h string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.notStored[h]
}
func (f *fakeTarget) CatchupCacheFailures() int64 { return 0 }
func (f *fakeTarget) feeds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.fed...)
}

type catchupEnv struct {
	c        *catchup
	tg       *fakeTarget
	items    []library.Item
	complete map[warmup.FileKey]bool
	x        *fileStateIndex
	now      time.Time
	nowMu    sync.Mutex
	log      []string
	logMu    sync.Mutex
	gauge    *catchupGauge
}

func newCatchupEnv(t *testing.T) *catchupEnv {
	t.Helper()
	e := &catchupEnv{tg: &fakeTarget{notStored: map[string]bool{}}, complete: map[warmup.FileKey]bool{},
		x: warmIndex(t), now: wt0, gauge: &catchupGauge{}}
	e.c = &catchup{
		list:     func() ([]library.Item, error) { return e.items, nil },
		complete: func(k warmup.FileKey) bool { return e.complete[k] },
		facts:    e.x.CatchupFact,
		record:   func(u warmUpdate) { e.x.RecordWarm(u) },
		target:   e.tg,
		perHour:  func() int { return 3_600_000 }, // an interval of a millisecond
		now:      func() time.Time { e.nowMu.Lock(); defer e.nowMu.Unlock(); return e.now },
		stop:     make(chan struct{}),
		poll:     time.Millisecond,
		logf: func(f string, v ...interface{}) {
			e.logMu.Lock()
			defer e.logMu.Unlock()
			e.log = append(e.log, strings.TrimSpace(fmt.Sprintf(f, v...)))
		},
		gauge: e.gauge,
	}
	return e
}

func (e *catchupEnv) advance(d time.Duration) { e.nowMu.Lock(); e.now = e.now.Add(d); e.nowMu.Unlock() }

func stub(hash string, idx int, mtime int64) library.Item {
	return library.Item{Path: "/private/path/" + hash + ".mkv", Hash: strings.ToUpper(hash), FileIndex: idx, Size: 1 << 30, ModTime: time.Unix(mtime, 0)}
}

func TestAPassFeedsTheNewestStubFirstAndEachHashOnce(t *testing.T) {
	e := newCatchupEnv(t)
	e.items = []library.Item{stub("aa", 1, 100), stub("bb", 1, 300), stub("aa", 2, 400), stub("cc", 7, 200), stub("bb", 2, 50)}
	e.c.runPass()
	// aa holds the newest stub (400), then bb (300), then cc (200).
	want := []string{"aa/1,2", "bb/1,2", "cc/7"}
	if got := e.tg.feeds(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("fed %v, want %v", got, want)
	}
}

func TestAPassSkipsWarmFilesAndWritesTheCachedRecordWithoutAWake(t *testing.T) {
	e := newCatchupEnv(t)
	e.items = []library.Item{stub("aa", 1, 100), stub("aa", 2, 100)}
	e.complete[fk("aa", 1)] = true
	e.c.runPass()
	if got := e.tg.feeds(); len(got) != 1 || got[0] != "aa/2" {
		t.Fatalf("fed %v, want only the file that is not warm", got)
	}
	if w, _ := e.x.CatchupFact(fk("aa", 1)); w.cache != cacheCached {
		t.Fatalf("record = %+v, want cache=cached written for the warm file", w)
	}
	if w, _ := e.x.CatchupFact(fk("aa", 2)); w.recorded() {
		t.Fatalf("record = %+v: the pass must not record a file it fed", w)
	}
}

func TestAPassNeverFeedsEvictedGivenUpOrWaitingFiles(t *testing.T) {
	e := newCatchupEnv(t)
	now := e.now
	e.items = []library.Item{
		stub("ev", 1, 10), stub("gu", 1, 10), stub("rw", 1, 10), stub("ns", 1, 10),
		stub("hw", 1, 10), stub("hw", 2, 10), stub("ok", 1, 10),
	}
	e.x.OnEvicted([]warmup.Eviction{{Key: fk("ev", 1)}})
	e.x.RecordWarm(warmUpdate{key: fk("gu", 1), readability: readFailed, attemptAt: now.Add(-90 * time.Hour)})
	for i := 0; i < 3; i++ {
		e.x.RecordWarm(warmUpdate{key: fk("gu", 1), readability: readFailed, attemptAt: now.Add(-90 * time.Hour)})
	}
	e.x.RecordWarm(warmUpdate{key: fk("rw", 1), cache: "lost", attemptAt: now.Add(-time.Hour)})
	e.x.RecordWarm(warmUpdate{key: fk("hw", 1), readability: readFailed, attemptAt: now.Add(-2 * time.Hour)})
	e.tg.notStored["ns"] = true
	e.c.runPass()
	if got := e.tg.feeds(); len(got) != 1 || got[0] != "ok/1" {
		t.Fatalf("fed %v, want only ok/1", got)
	}
	g := e.gauge.get()
	// ok/1 was fed, and the fake warmer records nothing, so it is still pending.
	want := map[string]int64{statePending: 1, stateRetryWait: 3, stateGaveUp: 1, stateEvicted: 1, stateNotStored: 1, stateLeftNoRoom: 0}
	for k, v := range want {
		if g[k] != v {
			t.Errorf("gauge %s = %d, want %d (%v)", k, g[k], v, g)
		}
	}
}

func TestAPassThatFindsNoRoomLeavesTheRestAsLeftNoRoom(t *testing.T) {
	e := newCatchupEnv(t)
	e.items = []library.Item{stub("aa", 1, 300), stub("bb", 1, 200), stub("bb", 2, 200), stub("cc", 1, 100)}
	e.tg.noRoomAt = "aa"
	e.c.runPass()
	if got := e.tg.feeds(); len(got) != 1 || got[0] != "aa/1" {
		t.Fatalf("fed %v: the pass must end on the no-room flag", got)
	}
	g := e.gauge.get()
	// The title that found no room is counted no_room and skipped with no record, so it
	// is left for room like the three behind it.
	if g[stateLeftNoRoom] != 4 || g[statePending] != 0 {
		t.Fatalf("gauge = %v, want left_no_room 4 and pending 0", g)
	}
}

func TestTheNextPassPicksUpARetryAfterTwentyHours(t *testing.T) {
	e := newCatchupEnv(t)
	e.items = []library.Item{stub("aa", 1, 100), stub("aa", 2, 100)}
	e.x.RecordWarm(warmUpdate{key: fk("aa", 1), readability: readFailed, attemptAt: e.now.Add(-time.Hour)})
	e.c.runPass()
	if got := e.tg.feeds(); len(got) != 0 {
		t.Fatalf("fed %v while the hash waits for its 20 hours", got)
	}
	e.advance(6 * time.Hour) // the 6 h re-pass: 7 h since the failure
	e.c.runPass()
	if got := e.tg.feeds(); len(got) != 0 {
		t.Fatalf("fed %v at 7 h", got)
	}
	e.advance(14 * time.Hour) // 21 h since the failure
	e.c.runPass()
	if got := e.tg.feeds(); len(got) != 1 || got[0] != "aa/1,2" {
		t.Fatalf("fed %v at 21 h, want the whole hash again", got)
	}
}

func TestStillDropsAFileThatTurnedWarmAfterTheListWasMade(t *testing.T) {
	e := newCatchupEnv(t)
	e.items = []library.Item{stub("aa", 1, 300), stub("bb", 1, 200)}
	// bb becomes warm while aa is being fed.
	e.tg.hold = make(chan struct{})
	done := make(chan struct{})
	go func() { e.c.runPass(); close(done) }()
	waitFor(t, "the first feed", func() bool { return len(e.tg.feeds()) == 1 })
	e.complete[fk("bb", 1)] = true
	close(e.tg.hold)
	<-done
	if got := e.tg.feeds(); len(got) != 1 {
		t.Fatalf("fed %v: bb was warm by then", got)
	}
}

func TestTwoPassesNeverRunAtOnce(t *testing.T) {
	e := newCatchupEnv(t)
	e.items = []library.Item{stub("aa", 1, 100)}
	e.tg.hold = make(chan struct{})
	first := make(chan bool)
	go func() { first <- e.c.runPass() }()
	waitFor(t, "the first pass to be feeding", func() bool { return len(e.tg.feeds()) == 1 })
	if e.c.runPass() {
		t.Fatal("a second pass ran while the first was in flight")
	}
	close(e.tg.hold)
	if !<-first {
		t.Fatal("the first pass did not run")
	}
	if !e.c.runPass() {
		t.Fatal("a pass after the first ended must run")
	}
}

func TestAPassLogsOneLineAtStartAndOneAtEndWithCountsAndNoPath(t *testing.T) {
	e := newCatchupEnv(t)
	e.items = []library.Item{stub("aa", 1, 100), stub("aa", 2, 100), stub("bb", 1, 50)}
	e.complete[fk("bb", 1)] = true
	e.c.runPass()
	e.logMu.Lock()
	defer e.logMu.Unlock()
	if len(e.log) != 2 {
		t.Fatalf("log = %q, want a start and an end line", e.log)
	}
	for _, l := range e.log {
		if !strings.HasPrefix(l, "[WarmCatchup] {") || strings.Contains(l, "/private/path") {
			t.Fatalf("line = %s", l)
		}
	}
	var start, end map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(e.log[0], "[WarmCatchup] ")), &start); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(e.log[1], "[WarmCatchup] ")), &end); err != nil {
		t.Fatal(err)
	}
	if start["event"] != "pass_start" || start["pending"] != float64(2) || start["candidate_hashes"] != float64(1) {
		t.Fatalf("start = %v", start)
	}
	if end["event"] != "pass_end" || end["fed"] != float64(2) || end["fed_hashes"] != float64(1) {
		t.Fatalf("end = %v", end)
	}
}

// ---- the loop: requirements, pace, schedule ----

type fakeClock struct {
	mu    sync.Mutex
	waits []time.Duration
	tick  chan struct{}
}

func (f *fakeClock) after(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	f.waits = append(f.waits, d)
	f.mu.Unlock()
	ch := make(chan time.Time, 1)
	go func() { <-f.tick; ch <- time.Time{} }()
	return ch
}
func (f *fakeClock) got() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.waits...)
}

func TestTheLoopWaitsOneIntervalFirstThenSixHours(t *testing.T) {
	e := newCatchupEnv(t)
	e.items = []library.Item{stub("aa", 1, 100)}
	e.c.perHour = func() int { return 20 }
	clk := &fakeClock{tick: make(chan struct{})}
	stop := make(chan struct{})
	e.c.stop = stop
	done := make(chan struct{})
	go func() { e.c.loop(func() string { return "" }, clk.after); close(done) }()
	waitFor(t, "the first wait", func() bool { return len(clk.got()) == 1 })
	if clk.got()[0] != 3*time.Minute {
		t.Fatalf("first wait = %v, want one interval (3m at 20 a hour)", clk.got()[0])
	}
	if len(e.tg.feeds()) != 0 {
		t.Fatal("a pass ran before the first interval")
	}
	clk.tick <- struct{}{}
	waitFor(t, "the second wait", func() bool { return len(clk.got()) == 2 })
	if clk.got()[1] != 6*time.Hour || len(e.tg.feeds()) != 1 {
		t.Fatalf("waits %v, feeds %v: want a pass, then 6 h", clk.got(), e.tg.feeds())
	}
	close(stop)
	clk.tick <- struct{}{}
	<-done
}

func TestTheLoopRunsNothingAndWarnsOnceWhenARequirementIsMissing(t *testing.T) {
	e := newCatchupEnv(t)
	e.items = []library.Item{stub("aa", 1, 100)}
	e.c.perHour = func() int { return 20 }
	clk := &fakeClock{tick: make(chan struct{})}
	stop := make(chan struct{})
	e.c.stop = stop
	done := make(chan struct{})
	go func() {
		e.c.loop(func() string { return "the state database is not attached" }, clk.after)
		close(done)
	}()
	for i := 1; i <= 3; i++ {
		waitFor(t, "a wait", func() bool { return len(clk.got()) == i })
		clk.tick <- struct{}{}
	}
	waitFor(t, "a fourth wait", func() bool { return len(clk.got()) == 4 })
	close(stop)
	clk.tick <- struct{}{}
	<-done
	if len(e.tg.feeds()) != 0 {
		t.Fatalf("fed %v without the requirements", e.tg.feeds())
	}
	e.logMu.Lock()
	defer e.logMu.Unlock()
	n := 0
	for _, l := range e.log {
		if strings.Contains(l, "WARNING") && strings.Contains(l, "state database") {
			n++
		}
		if strings.Contains(l, "pass_start") {
			t.Fatalf("a pass started: %q", e.log)
		}
	}
	if n != 1 {
		t.Fatalf("warnings = %d, want one:\n%q", n, e.log)
	}
}

func TestTheLoopDoesNothingWhileThePaceIsZero(t *testing.T) {
	e := newCatchupEnv(t)
	e.items = []library.Item{stub("aa", 1, 100)}
	pace := int32(0)
	e.c.perHour = func() int { return int(atomic.LoadInt32(&pace)) }
	clk := &fakeClock{tick: make(chan struct{})}
	stop := make(chan struct{})
	e.c.stop = stop
	done := make(chan struct{})
	go func() { e.c.loop(func() string { return "" }, clk.after); close(done) }()
	waitFor(t, "the first wait", func() bool { return len(clk.got()) == 1 })
	if clk.got()[0] != 6*time.Hour {
		t.Fatalf("first wait with pace 0 = %v, want the 6 h cadence", clk.got()[0])
	}
	clk.tick <- struct{}{}
	waitFor(t, "the second wait", func() bool { return len(clk.got()) == 2 })
	if len(e.tg.feeds()) != 0 {
		t.Fatal("fed with the pace at 0: it turns catch-up off")
	}
	atomic.StoreInt32(&pace, 3_600_000) // the panel turns it on
	clk.tick <- struct{}{}
	waitFor(t, "the third wait", func() bool { return len(clk.got()) == 3 })
	if len(e.tg.feeds()) != 1 {
		t.Fatalf("fed %v after the pace was raised", e.tg.feeds())
	}
	close(stop)
	clk.tick <- struct{}{}
	<-done
}

func TestCatchupRequirementsNeedTheSwitchTheDirAndTheStateDatabase(t *testing.T) {
	x := warmIndex(t)
	cases := []struct {
		name    string
		on      bool
		dir     string
		store   bool
		wantMsg string
	}{
		{"all set", true, "/cache", true, ""},
		{"switch off", false, "/cache", true, "warm_on_add"},
		{"no warmup_dir", true, "", true, "warmup_dir"},
		{"no state database", true, "/cache", false, "state database"},
	}
	for _, c := range cases {
		withConfig(t, func(cfg *config.Config) { cfg.WarmOnAdd, cfg.WarmupDir = c.on, c.dir })
		x.store = nil
		if c.store {
			x.store = &fakeWarmStore{}
		}
		got := catchupRequirement(x)
		if (c.wantMsg == "") != (got == "") || !strings.Contains(got, c.wantMsg) {
			t.Errorf("%s: %q, want a message containing %q", c.name, got, c.wantMsg)
		}
	}
}

func TestTheDaemonsCatchupStopsWithTheBackgroundStopChannel(t *testing.T) {
	c := newCatchup(nil, fileStates, func() ([]library.Item, error) { return nil, nil })
	if c.stop != (<-chan struct{})(backgroundStopChan) {
		t.Fatal("the feeder must stop with backgroundStopChan")
	}
	if c.perHour == nil {
		t.Fatal("the feeder needs a pace")
	}
}

// ---- metrics ----

func TestCatchupGaugeStartsAtZeroForEveryState(t *testing.T) {
	var w promtext.Writer
	collectCatchupProm(&w, &catchupGauge{})
	if err := promtext.Lint(w.Bytes()); err != nil {
		t.Fatalf("lint: %v\n%s", err, w.Bytes())
	}
	for _, s := range []string{"pending", "retry_wait", "gave_up", "evicted", "not_stored", "left_no_room"} {
		want := `flodni_warm_catchup_files{state="` + s + `"} 0` + "\n"
		if !strings.Contains(string(w.Bytes()), want) {
			t.Errorf("missing %q in:\n%s", want, w.Bytes())
		}
	}
}

func TestTheLoopDoesNotWarnWhileWarmOnAddIsOff(t *testing.T) {
	e := newCatchupEnv(t)
	e.items = []library.Item{stub("aa", 1, 100)}
	e.c.perHour = func() int { return 20 }
	clk := &fakeClock{tick: make(chan struct{})}
	stop := make(chan struct{})
	e.c.stop = stop
	done := make(chan struct{})
	go func() { e.c.loop(func() string { return catchupSwitchOff }, clk.after); close(done) }()
	waitFor(t, "the first wait", func() bool { return len(clk.got()) == 1 })
	clk.tick <- struct{}{}
	waitFor(t, "the second wait", func() bool { return len(clk.got()) == 2 })
	close(stop)
	clk.tick <- struct{}{}
	<-done
	e.logMu.Lock()
	defer e.logMu.Unlock()
	if len(e.log) != 0 || len(e.tg.feeds()) != 0 {
		t.Fatalf("log = %q, feeds = %v: the switch off is a choice, not a fault", e.log, e.tg.feeds())
	}
}

func TestAStubWithoutAnIndexIsNotACandidate(t *testing.T) {
	e := newCatchupEnv(t)
	e.items = []library.Item{stub("aa", 0, 100), stub("bb", 1, 50)}
	e.c.runPass()
	if got := e.tg.feeds(); len(got) != 1 || got[0] != "bb/1" {
		t.Fatalf("fed %v: engine file ids start at 1, a stub with no index is not a file", got)
	}
}

// ---- pack-level give-up ----

// giveUp records four failed swarm reads of a file, the last attemptAgo ago.
func giveUp(e *catchupEnv, hash string, idx int, attemptAgo time.Duration) {
	for i := 0; i < catchupMaxFailed; i++ {
		e.x.RecordWarm(warmUpdate{key: fk(hash, idx), readability: readFailed, attemptAt: e.now.Add(-attemptAgo)})
	}
}

func TestAPackWhoseFirstFileGaveUpIsNotAttemptedForItsOtherFiles(t *testing.T) {
	e := newCatchupEnv(t)
	e.items = []library.Item{stub("aa", 1, 100), stub("aa", 2, 100), stub("aa", 3, 100), stub("ok", 1, 50)}
	giveUp(e, "aa", 1, 90*time.Hour) // long past the 20 h wait: file 2 and 3 would be pending
	e.c.runPass()
	if got := e.tg.feeds(); len(got) != 1 || got[0] != "ok/1" {
		t.Fatalf("fed %v, want only ok/1", got)
	}
	if g := e.gauge.get(); g[stateGaveUp] != 3 || g[stateRetryWait] != 0 {
		t.Fatalf("gauge = %v, want gave_up 3 (the file and its two siblings)", g)
	}
}

func TestASwarmDeliveryOnAnyFileOfTheHashLiftsThePackGiveUp(t *testing.T) {
	e := newCatchupEnv(t)
	e.items = []library.Item{stub("aa", 1, 100), stub("aa", 2, 100), stub("aa", 3, 100)}
	giveUp(e, "aa", 1, 90*time.Hour)
	// A delivery before the give-up is old news.
	e.x.RecordWarm(warmUpdate{key: fk("aa", 3), readability: readWarmed, cache: cacheCached, deliveredAt: e.now.Add(-100 * time.Hour)})
	e.c.runPass()
	if got := e.tg.feeds(); len(got) != 0 {
		t.Fatalf("fed %v, want none: the delivery is older than the give-up", got)
	}
	// A delivery after it, on a sibling, lifts the pack; the given-up file stays given up.
	e.x.RecordWarm(warmUpdate{key: fk("aa", 3), readability: readWarmed, cache: cacheCached, deliveredAt: e.now.Add(-time.Hour)})
	e.c.runPass()
	if got := e.tg.feeds(); len(got) != 1 || got[0] != "aa/2" {
		t.Fatalf("fed %v, want aa/2 (file 1 stays given up, file 3 is cached in the record only)", got)
	}
}

func TestCacheSideFailuresDoNotGiveUpAPack(t *testing.T) {
	e := newCatchupEnv(t)
	e.items = []library.Item{stub("aa", 1, 100), stub("aa", 2, 100)}
	for i := 0; i < 2*catchupMaxFailed; i++ {
		e.x.RecordWarm(warmUpdate{key: fk("aa", 1), cache: cacheWriteFailed, attemptAt: e.now.Add(-90 * time.Hour)})
		e.x.RecordWarm(warmUpdate{key: fk("aa", 1), cache: cacheLost, attemptAt: e.now.Add(-90 * time.Hour)})
	}
	e.c.runPass()
	if got := e.tg.feeds(); len(got) != 1 || got[0] != "aa/1,2" {
		t.Fatalf("fed %v, want the whole pack: lost and write_failed are no swarm give-up", got)
	}
}

func TestPackGaveUpTable(t *testing.T) {
	failed := fileWarm{readability: readFailed, failedAttempts: catchupMaxFailed, lastAttemptAt: 1000}
	cases := []struct {
		name string
		ws   []fileWarm
		want bool
	}{
		{"nothing recorded", []fileWarm{{}, {}}, false},
		{"three failures", []fileWarm{{readability: readFailed, failedAttempts: 3, lastAttemptAt: 1000}, {}}, false},
		{"four failures", []fileWarm{failed, {}}, true},
		{"cache-side outcomes only", []fileWarm{{cache: cacheLost, failedAttempts: 9, lastAttemptAt: 1000}, {}}, false},
		{"delivery after the give-up", []fileWarm{failed, {readability: readWarmed, lastDeliveryAt: 1001}}, false},
		{"delivery before the give-up", []fileWarm{failed, {readability: readWarmed, lastDeliveryAt: 999}}, true},
	}
	for _, c := range cases {
		if got := packGaveUp(c.ws); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
