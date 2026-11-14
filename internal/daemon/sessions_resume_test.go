package daemon

import (
	"flodni/internal/gostorm/native"
	"testing"
	"time"
)

// A read on a handle that is still open after its session closed (60s idle) opens a
// new session, linked to the old one, and the bytes are counted there.
func TestReadAfterCloseOpensLinkedSession(t *testing.T) {
	resetSessionState(t)
	const path = "/m/pause.mkv"
	ttffRegister(path, 100<<20, "h", 3, true, false)
	old := mustSession(t, path)
	ttffRead(path, srcFetchBlock, time.Millisecond, 4096, 0)
	old.closeSession()
	if _, ok := sessions.Load(path); ok {
		t.Fatal("closed session still registered")
	}

	ttffRead(path, srcFetchBlock, time.Millisecond, 2<<20, 50<<20)
	ns := mustSession(t, path)
	if ns == old || ns.resumedFrom != old.id || ns.id == old.id {
		t.Fatalf("resumed session not linked: old=%d new=%d from=%d", old.id, ns.id, ns.resumedFrom)
	}
	r := ns.record(time.Time{}, false)
	if r.BytesServed != 2<<20 || r.Reads != 1 || r.FileIndex != 3 || r.Hash != "h" || r.Size != 100<<20 ||
		r.ResumedFrom != old.id || r.ID != ns.id {
		t.Fatalf("record = %+v", r)
	}
	if got := closedSessions.newestFirst(); len(got) != 1 || got[0].BytesServed != 4096 || got[0].ResumedFrom != 0 {
		t.Fatalf("old record changed: %+v", got)
	}

	// A second pause links to the second session, not the first.
	ns.closeSession()
	ttffRead(path, srcFetchBlock, time.Millisecond, 10, 0)
	if third := mustSession(t, path); third.resumedFrom != ns.id {
		t.Fatalf("third from %d, want %d", third.resumedFrom, ns.id)
	}
}

// A probe handle releasing closes its session and leaves nothing to resume; so does
// a path no session ever closed on.
func TestProbeReleaseDoesNotOpenSession(t *testing.T) {
	resetSessionState(t)
	const path = "/m/probe.mkv"
	ttffRegister(path, 100, "h", 0, false, false)
	ttffReleaseClose(path)
	if _, ok := lastClosed.Load(path); ok {
		t.Fatal("probe release left a resumable session")
	}
	ttffRead(path, srcWarmupHead, time.Millisecond, 10, 0)
	if _, ok := sessions.Load(path); ok {
		t.Fatal("read with nothing to resume opened a session")
	}
	ttffRead("/m/never.mkv", srcWarmupHead, time.Millisecond, 10, 0)
	if _, ok := sessions.Load("/m/never.mkv"); ok {
		t.Fatal("read on an unknown path opened a session")
	}
}

// A fresh Open after a close is a new session, not a continuation.
func TestRegisterAfterCloseIsNotAResume(t *testing.T) {
	resetSessionState(t)
	const path = "/m/again.mkv"
	ttffRegister(path, 100, "h", 0, false, false)
	mustSession(t, path).closeSession()
	ttffRegister(path, 100, "h", 0, false, false)
	if s := mustSession(t, path); s.resumedFrom != 0 {
		t.Fatalf("fresh open resumed_from = %d", s.resumedFrom)
	}
	if _, ok := lastClosed.Load(path); ok {
		t.Fatal("fresh open kept the resume entry")
	}
}

// The resumed session obeys the verdict floor: failing within 25s of the resume is
// no dead-swarm verdict, however long ago the first session opened; past the floor it is.
func TestResumedSessionVerdictFloor(t *testing.T) {
	for _, tt := range []struct {
		name string
		age  time.Duration
		want string
	}{
		{"fails within the floor", 5 * time.Second, outcomeUnserved},
		{"fails past the floor", time.Minute, outcomeDeadSwarm},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resetSessionState(t)
			prev := native.ReachabilityOutcome
			t.Cleanup(func() { native.ReachabilityOutcome = prev })
			var kinds []native.OutcomeKind
			native.ReachabilityOutcome = func(o native.Outcome) { kinds = append(kinds, o.Kind) }
			const path = "/m/dead.mkv"
			ttffRegister(path, 100, "h", 0, false, false)
			first := mustSession(t, path)
			first.openedAt.Store(time.Now().Add(-10 * time.Minute).UnixNano())
			first.closeSession()
			kinds = nil

			ttffReadFailed(path) // the first read after the close fails
			ns := mustSession(t, path)
			if ns.resumedFrom != first.id || !ns.readFailed.Load() {
				t.Fatalf("failed read did not open a linked session: %+v", ns.record(time.Time{}, false))
			}
			ns.openedAt.Store(time.Now().Add(-tt.age).UnixNano())
			ns.closeSession()
			got := closedSessions.newestFirst()
			if got[0].Outcome != tt.want || got[0].ResumedFrom != first.id {
				t.Fatalf("got %+v, want outcome %s", got[0], tt.want)
			}
			if dead := tt.want == outcomeDeadSwarm; dead != (len(kinds) == 1 && kinds[0] == native.OutcomeFailed) {
				t.Fatalf("outcomes reported: %v", kinds)
			}
		})
	}
}

func mustSession(t *testing.T, path string) *TTFFSession {
	t.Helper()
	v, ok := sessions.Load(path)
	if !ok {
		t.Fatalf("no session for %s", path)
	}
	return v.(*TTFFSession)
}
