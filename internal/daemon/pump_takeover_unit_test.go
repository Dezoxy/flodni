package daemon

import (
	"testing"
	"time"
)

func TestMayTakeOverStreamingPrimary(t *testing.T) {
	t.Parallel()
	base := time.Now()
	cases := []struct {
		name         string
		openedAt     time.Time
		sessionStart time.Time
		want         bool
	}{
		{"opened after the incumbent and before the session started: the session's player", base.Add(time.Second), base.Add(2 * time.Second), true},
		{"opened at the session start", base.Add(time.Second), base.Add(time.Second), true},
		{"opened after the session started: a probe", base.Add(3 * time.Second), base.Add(2 * time.Second), false},
		{"opened before the incumbent", base.Add(-time.Second), base.Add(2 * time.Second), false},
		{"no session start known: nothing makes a successor", base.Add(time.Second), time.Time{}, false},
		{"open time unknown", time.Time{}, base.Add(2 * time.Second), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			inc := &MkvHandle{openedAt: base}
			// An incumbent silent since before the newcomer opened makes no difference: a lull
			// between a buffered player's bursts is not a reopen.
			inc.lastRead.Store(base.UnixNano())
			if got := mayTakeOverStreamingPrimary(&MkvHandle{openedAt: c.openedAt}, inc, c.sessionStart); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestAnOlderHandleThatWasPrimaryIsNotDeferredToALaterIncumbent(t *testing.T) {
	newTestApp(t)
	useTestGlobals(t)
	const path = "/test/older-handle.mkv"
	base := time.Now()
	older := newTestHandle(path)
	older.openedAt = base
	older.wasPrimary.Store(true)
	inc := newTestHandle(path)
	inc.openedAt = base.Add(time.Second)
	inc.isPrimaryHandle.Store(true)
	inc.streamedOnce.Store(true)
	inc.lastRead.Store(time.Now().UnixNano())
	registerHandle(t, older, inc)

	if older.shadowsActivePlayer() {
		t.Error("the older handle is a probe behind a later incumbent: it could never retake the primary")
	}
	probe := newTestHandle(path)
	probe.openedAt = base.Add(-time.Second)
	registerHandle(t, probe)
	if !probe.shadowsActivePlayer() {
		t.Error("an older handle that was never primary is a probe behind the incumbent")
	}
	later := newTestHandle(path)
	later.openedAt = base.Add(2 * time.Second)
	registerHandle(t, later)
	later.lastRead.Store(time.Now().UnixNano())
	inc.lastRead.Store(time.Now().Add(time.Minute).UnixNano())
	if !later.shadowsActivePlayer() {
		t.Error("a handle opened after the incumbent, with the incumbent still reading, is a probe")
	}
}

func TestActiveStreamingPrimaryNeedsAPrimaryThatStreamedAndReadLately(t *testing.T) {
	newTestApp(t)
	useTestGlobals(t)
	const path = "/test/active-primary.mkv"
	probe := newTestHandle(path)
	registerHandle(t, probe)
	mk := func(primary, streamed bool, readAgo time.Duration) *MkvHandle {
		h := newTestHandle(path)
		h.isPrimaryHandle.Store(primary)
		h.streamedOnce.Store(streamed)
		h.lastRead.Store(time.Now().Add(-readAgo).UnixNano())
		registerHandle(t, h)
		return h
	}
	if activeStreamingPrimary(path, probe) != nil {
		t.Fatal("no other handle: want nil")
	}
	mk(true, false, 0) // a probe holding primary: never streamed
	mk(false, true, 0) // streams, but is not the primary
	mk(true, true, 10*primaryActiveWindow())
	if activeStreamingPrimary(path, probe) != nil {
		t.Fatal("a primary that never streamed, a streamer that is not primary and a quiet primary must not count")
	}
	want := mk(true, true, 0)
	if got := activeStreamingPrimary(path, probe); got != want {
		t.Errorf("got %v, want the primary that streamed and read just now", got)
	}
	if activeStreamingPrimary(path, want) != nil {
		t.Error("the handle itself must be excluded")
	}
	if activeStreamingPrimary("/test/other.mkv", probe) != nil {
		t.Error("another file's primary must not count")
	}
}
