package daemon

import (
	"strings"
	"testing"
	"time"

	"flodni/internal/library"
	"flodni/internal/warmup"
)

// Roadmap step 18: catch-up warms an evicted file again only when a confirmed play of
// it is recorded within played_protection_days and no attempt followed that play.

func TestCatchupStateRewarm(t *testing.T) {
	now := wt0
	ago := func(d time.Duration) int64 { return now.Add(-d).Unix() }
	cases := []struct {
		name    string
		in      catchupFile
		retries bool
		want    string
	}{
		{"evicted, played recently", catchupFile{evicted: true, rewarmable: true}, false, stateRewarm},
		{"evicted by cache state only", catchupFile{warm: fileWarm{cache: cacheEvicted}, rewarmable: true}, false, stateRewarm},
		{"evicted, not rewarmable", catchupFile{evicted: true}, false, stateEvicted},
		{"rewarmable but warm on disk", catchupFile{evicted: true, rewarmable: true, complete: true}, false, stateWarm},
		{"a sibling failed: waits", catchupFile{evicted: true, rewarmable: true, hashFailedRecently: true}, false, stateRetryWait},
		{"refused by WakeStored", catchupFile{evicted: true, rewarmable: true, notStored: true}, false, stateNotStored},
		{"given up", catchupFile{evicted: true, rewarmable: true, warm: fileWarm{readability: readFailed, failedAttempts: 4, lastAttemptAt: ago(100 * time.Hour)}}, false, stateGaveUp},
		{"a file not evicted is no rewarm", catchupFile{rewarmable: true}, false, statePending},
		{"scope retries: the rewarm is in scope", catchupFile{evicted: true, rewarmable: true}, true, stateRewarm},
		{"scope retries: not rewarmable stays evicted", catchupFile{evicted: true}, true, stateEvicted},
		{"scope retries: a played cold file is still the backlog", catchupFile{rewarmable: true}, true, stateOutOfScope},
	}
	for _, c := range cases {
		c.in.retriesOnly = c.retries
		if got := catchupState(c.in, now); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// rewarmEnv has the index clock under the test's control and the played hook wired.
func rewarmEnv(t *testing.T) *catchupEnv {
	e := newCatchupEnv(t)
	e.x.now = func() time.Time { e.nowMu.Lock(); defer e.nowMu.Unlock(); return e.now }
	e.c.played = e.x.RewarmFact
	return e
}

func TestAPlayedEvictedFileIsFedAgain(t *testing.T) {
	e := rewarmEnv(t)
	e.items = []library.Item{stub("aa", 1, 100), stub("bb", 1, 100)}
	e.x.MarkPlayed(fk("aa", 1))
	e.advance(time.Hour)
	e.x.OnEvicted([]warmup.Eviction{{Key: fk("aa", 1)}, {Key: fk("bb", 1)}})
	e.c.runPass()
	if got := e.tg.feeds(); len(got) != 1 || got[0] != "aa/1" {
		t.Fatalf("fed %v, want only the played file aa/1 (bb was never played)", got)
	}
	g := e.gauge.get()
	if g[stateEvicted] != 1 || g[statePending] != 1 {
		t.Fatalf("gauge = %v, want evicted 1 (bb) and pending 1 (aa)", g)
	}
	var start string
	for _, l := range e.log {
		if strings.Contains(l, `"pass_start"`) {
			start = l
		}
	}
	if !strings.Contains(start, `"rewarm":1`) {
		t.Fatalf("pass_start line = %s, want rewarm 1", start)
	}
}

func TestANeverPlayedEvictedFileIsNotFed(t *testing.T) {
	e := rewarmEnv(t)
	e.items = []library.Item{stub("aa", 1, 100)}
	e.x.OnEvicted([]warmup.Eviction{{Key: fk("aa", 1)}})
	e.c.runPass()
	if got := e.tg.feeds(); len(got) != 0 {
		t.Fatalf("fed %v, want nothing", got)
	}
	if g := e.gauge.get(); g[stateEvicted] != 1 {
		t.Fatalf("gauge = %v", g)
	}
}

func TestAPlayOlderThanTheProtectionTimeDoesNotRewarm(t *testing.T) {
	e := rewarmEnv(t)
	e.items = []library.Item{stub("aa", 1, 100)}
	e.x.MarkPlayed(fk("aa", 1))
	e.x.OnEvicted([]warmup.Eviction{{Key: fk("aa", 1)}})
	e.advance(91 * 24 * time.Hour)
	e.c.runPass()
	if got := e.tg.feeds(); len(got) != 0 {
		t.Fatalf("fed %v at 91 days, protection is 90", got)
	}
	e.x.setProtectionDays(0) // 0 = a play counts forever
	e.c.runPass()
	if got := e.tg.feeds(); len(got) != 1 {
		t.Fatalf("fed %v with protection 0, want the file", got)
	}
}

// No room: the pass ends on the flag, the file stays unwarmed and nothing is evicted
// by the pass (the warmer fills catch-up files under EvictNone; the warmer's own tests
// pin that). Here: the rewarm goes through the same no-room stop as any candidate.
func TestARewarmThatFindsNoRoomIsLeftForRoom(t *testing.T) {
	e := rewarmEnv(t)
	e.items = []library.Item{stub("aa", 1, 300), stub("bb", 1, 200)}
	for _, h := range []string{"aa", "bb"} {
		e.x.MarkPlayed(fk(h, 1))
	}
	e.x.OnEvicted([]warmup.Eviction{{Key: fk("aa", 1)}, {Key: fk("bb", 1)}})
	e.tg.noRoomAt = "aa"
	e.c.runPass()
	if got := e.tg.feeds(); len(got) != 1 || got[0] != "aa/1" {
		t.Fatalf("fed %v: the pass must end on the no-room flag", got)
	}
	if g := e.gauge.get(); g[stateLeftNoRoom] != 2 || g[statePending] != 0 {
		t.Fatalf("gauge = %v, want left_no_room 2", g)
	}
	if _, ev := e.x.CatchupFact(fk("bb", 1)); !ev {
		t.Fatal("the pass must not touch the eviction mark")
	}
}

// The loop: played, evicted, rewarmed (an attempt), evicted again: not fed until a new
// play.
func TestARewarmedFileEvictedAgainIsNotFedUntilItsNextPlay(t *testing.T) {
	e := rewarmEnv(t)
	e.items = []library.Item{stub("aa", 1, 100)}
	e.x.MarkPlayed(fk("aa", 1))
	e.advance(time.Hour)
	e.x.OnEvicted([]warmup.Eviction{{Key: fk("aa", 1)}})
	e.c.runPass()
	if got := e.tg.feeds(); len(got) != 1 {
		t.Fatalf("fed %v, want the first rewarm", got)
	}
	// The warmer's attempt, then the quota takes the file again.
	e.advance(time.Minute)
	e.x.RecordWarm(warmUpdate{key: fk("aa", 1), readability: readWarmed, cache: cacheCached, attemptAt: e.now})
	e.advance(time.Hour)
	e.x.OnEvicted([]warmup.Eviction{{Key: fk("aa", 1)}})
	e.advance(24 * time.Hour)
	e.c.runPass()
	if got := e.tg.feeds(); len(got) != 1 {
		t.Fatalf("fed %v: a rewarmed file evicted again must not loop", got)
	}
	if g := e.gauge.get(); g[stateEvicted] != 1 {
		t.Fatalf("gauge = %v, want evicted 1", g)
	}
	// A new play makes it eligible again.
	e.x.MarkPlayed(fk("aa", 1))
	e.advance(time.Hour)
	e.x.OnEvicted([]warmup.Eviction{{Key: fk("aa", 1)}})
	e.advance(24 * time.Hour)
	e.c.runPass()
	if got := e.tg.feeds(); len(got) != 2 {
		t.Fatalf("fed %v, want a second rewarm after the new play", got)
	}
}

func TestScopeRetriesFeedsARewarmButNotTheColdBacklog(t *testing.T) {
	e := rewarmEnv(t)
	e.c.scope = func() string { return scopeRetries }
	e.items = []library.Item{stub("aa", 1, 300), stub("bb", 1, 200)}
	e.x.MarkPlayed(fk("aa", 1))
	e.x.OnEvicted([]warmup.Eviction{{Key: fk("aa", 1)}})
	e.x.MarkPlayed(fk("bb", 1)) // played, never evicted, never attempted: cold backlog
	e.c.runPass()
	if got := e.tg.feeds(); len(got) != 1 || got[0] != "aa/1" {
		t.Fatalf("fed %v, want only the rewarm aa/1", got)
	}
	if g := e.gauge.get(); g[stateOutOfScope] != 1 {
		t.Fatalf("gauge = %v, want out_of_scope 1 (bb)", g)
	}
}
