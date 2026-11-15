package daemon

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

var testApps sync.Map // *testing.T -> *App

// newTestApp installs a fresh App for the test and restores the previous one when it
// ends. Tests that seed or reset the shared read-path maps call it first, so no test
// sees another's state. A second call for the same t returns the App already installed,
// so setup helpers can each call it without swapping state mid-test.
func newTestApp(t *testing.T) *App {
	t.Helper()
	if a, ok := testApps.Load(t); ok {
		return a.(*App)
	}
	prev := app
	a := newApp()
	app = a
	testApps.Store(t, a)
	t.Cleanup(func() {
		assertDrained(t, a)
		app = prev
		testApps.Delete(t)
	})
	return a
}

// assertDrained fails the test when the retiring App still has background work
// registered: a prefetch in flight, a pump, or a pump or priority timer. A goroutine
// that outlives its test would write into the next App, so the test must drain it.
// Some tests drain asynchronously, so it polls for up to a second first.
func assertDrained(t *testing.T, a *App) {
	t.Helper()
	pending := func() []string {
		var left []string
		for name, m := range map[string]*sync.Map{
			"inFlightPrefetches": &a.inFlightPrefetches, "activePumps": &a.activePumps,
			"pumpTimers": &a.pumpTimers, "priorityTimers": &a.priorityTimers,
		} {
			m.Range(func(k, _ any) bool { left = append(left, fmt.Sprintf("%s[%v]", name, k)); return true })
		}
		sort.Strings(left)
		return left
	}
	deadline := time.Now().Add(time.Second)
	left := pending()
	for len(left) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		left = pending()
	}
	if len(left) > 0 {
		t.Errorf("the test left background work registered on its App: %s", strings.Join(left, ", "))
	}
}

func TestTwoAppsDoNotShareState(t *testing.T) {
	a, b := newApp(), newApp()
	a.activePumps.Store("/m/a.mkv", &NativePumpState{})
	a.playbackRegistry.Store("/m/a.mkv", &PlaybackState{Path: "/m/a.mkv"})
	a.activeHandles.Store(&MkvHandle{}, true)
	a.pumpTimers.Store("/m/a.mkv", true)
	a.priorityTimers.Store("/m/a.mkv", true)
	a.inFlightFetches.Store("k", true)
	a.inFlightPrefetches.Store("k", true)

	for name, m := range map[string]interface{ Range(func(k, v any) bool) }{
		"activePumps": &b.activePumps, "playbackRegistry": &b.playbackRegistry,
		"activeHandles": &b.activeHandles, "pumpTimers": &b.pumpTimers,
		"priorityTimers": &b.priorityTimers, "inFlightFetches": &b.inFlightFetches,
		"inFlightPrefetches": &b.inFlightPrefetches,
	} {
		m.Range(func(_, _ any) bool { t.Errorf("%s leaked into a second App", name); return false })
	}
}
