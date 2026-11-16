package daemon

import (
	"testing"
	"time"
)

func TestMayTakeOverStreamingPrimary(t *testing.T) {
	t.Parallel()
	base := time.Now()
	inc := &MkvHandle{openedAt: base}
	cases := []struct {
		name      string
		openedAt  time.Time
		confirmed time.Time
		want      bool
	}{
		{"opened after the incumbent and before the confirmation: the session's player", base.Add(time.Second), base.Add(2 * time.Second), true},
		{"opened at the confirmation", base.Add(time.Second), base.Add(time.Second), true},
		{"opened after the confirmation: an analysis of a file being watched", base.Add(3 * time.Second), base.Add(2 * time.Second), false},
		{"opened before the incumbent", base.Add(-time.Second), base.Add(2 * time.Second), false},
		{"never confirmed", base.Add(time.Second), time.Time{}, false},
		{"open time unknown", time.Time{}, base.Add(2 * time.Second), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := mayTakeOverStreamingPrimary(&MkvHandle{openedAt: c.openedAt}, inc, c.confirmed); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestActiveStreamingPrimaryNeedsAPrimaryThatStreamedAndReadLately(t *testing.T) {
	newTestApp(t)
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
	mk(true, true, 10*primaryActiveWindow)
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
