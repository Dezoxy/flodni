package daemon

import (
	"fmt"
	"strings"
	"testing"
)

// roleCounts is the unconfirmed playback row of the counter, by origin, for one role.
func roleCounts(role int) (out [numStallOrigins]int64) {
	for o := range out {
		out[o] = keepaliveCounters.stalls[0][stallCausePlayback][o][role].Load()
	}
	return out
}

func sumOf(a [numStallOrigins]int64) (n int64) {
	for _, v := range a {
		n += v
	}
	return n
}

// The role is primary only for the file's primary handle, and never for a probe.
func TestStallRoleOf(t *testing.T) {
	newTestApp(t)
	primary, other, probe := &MkvHandle{path: "/m/r.mkv"}, &MkvHandle{path: "/m/r.mkv"}, &MkvHandle{path: "/m/r.mkv"}
	primary.isPrimaryHandle.Store(true)
	probe.isPrimaryHandle.Store(true)
	for _, c := range []struct {
		name  string
		h     *MkvHandle
		probe bool
		want  int
	}{
		{"the primary handle", primary, false, stallRolePrimary},
		{"a handle that is not the primary", other, false, stallRoleSecond},
		{"a probe, even one flagged primary", probe, true, stallRoleSecond},
	} {
		if got := stallRoleOf(c.h, c.probe); got != c.want {
			t.Errorf("%s: role %d, want %d", c.name, got, c.want)
		}
	}
}

// A stall counts under the role it was read with and under no other.
func TestStallCountsUnderTheReadRole(t *testing.T) {
	stallEnv(t)
	ttffRead("/m/s.mkv", srcFetchBlock, slow, int(stallMiB), 500*stallMiB, stallRolePrimary)
	if p, s := roleCounts(stallRolePrimary), roleCounts(stallRoleSecond); sumOf(p) != 1 || sumOf(s) != 0 {
		t.Fatalf("after a primary stall: primary=%v second=%v", p, s)
	}
	ttffRead("/m/s.mkv", srcFetchBlock, slow, int(stallMiB), 501*stallMiB, stallRoleSecond)
	ttffRead("/m/s.mkv", srcFetchBlock, slow, int(stallMiB), 502*stallMiB, stallRoleSecond)
	if p, s := roleCounts(stallRolePrimary), roleCounts(stallRoleSecond); sumOf(p) != 1 || sumOf(s) != 2 {
		t.Fatalf("after two second stalls: primary=%v second=%v", p, s)
	}
	// A read at the threshold is no stall, whatever its role.
	ttffRead("/m/s.mkv", srcFetchBlock, ttffStallDuration, int(stallMiB), 503*stallMiB, stallRolePrimary)
	if got := sumOf(roleCounts(stallRolePrimary)); got != 1 {
		t.Errorf("a read at the threshold counted: primary=%d", got)
	}
}

// Summed over role, a confirmed, cause and origin combination holds what it counted before
// the role existed: the same stalls, split.
func TestStallRoleSumEqualsTheCombination(t *testing.T) {
	stallEnv(t)
	for i, role := range []int{stallRolePrimary, stallRoleSecond, stallRoleSecond, stallRolePrimary, stallRoleSecond} {
		ttffRead("/m/s.mkv", srcFetchBlock, slow, int(stallMiB), int64(500+i)*stallMiB, role)
	}
	if got := stallSum(0, stallCausePlayback); got != 5 {
		t.Errorf("playback sum = %d, want 5", got)
	}
	var byRole int64
	for r := 0; r < numStallRoles; r++ {
		byRole += sumOf(roleCounts(r))
	}
	if byRole != 5 {
		t.Errorf("sum over role = %d, want 5", byRole)
	}
	if p, s := sumOf(roleCounts(stallRolePrimary)), sumOf(roleCounts(stallRoleSecond)); p != 2 || s != 3 {
		t.Errorf("primary=%d second=%d, want 2 and 3", p, s)
	}
}

// Every confirmed, cause, origin and role combination exists from the start, with the role
// text in the HELP, and a counted stall moves only its own series.
func TestStallRoleMetricSeries(t *testing.T) {
	resetKeepaliveState(t)
	body := scrape(t).Body.String()
	series := 0
	for _, conf := range []string{"true", "false"} {
		for _, cause := range stallCauseNames {
			for _, origin := range stallOriginNames {
				for _, role := range stallRoleNames {
					want := fmt.Sprintf(`flodni_read_stalls_total{confirmed=%q,cause=%q,origin=%q,role=%q} 0`, conf, cause, origin, role)
					if !strings.Contains(body, want+"\n") {
						t.Errorf("series missing: %s", want)
					}
					series++
				}
			}
		}
	}
	if series != 32 {
		t.Fatalf("checked %d series, want 32", series)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "# HELP flodni_read_stalls_total ") {
			for _, w := range []string{"role", "primary", "second", "probe"} {
				if !strings.Contains(line, w) {
					t.Errorf("HELP lacks %q: %s", w, line)
				}
			}
		}
	}
	keepaliveCounters.stalls[1][stallCauseSeek][stallOriginSwarm][stallRoleSecond].Add(4)
	body = scrape(t).Body.String()
	if !strings.Contains(body, `flodni_read_stalls_total{confirmed="true",cause="seek",origin="swarm",role="second"} 4`) {
		t.Error("the second series did not count")
	}
	if !strings.Contains(body, `flodni_read_stalls_total{confirmed="true",cause="seek",origin="swarm",role="primary"} 0`) {
		t.Error("the primary series moved with the second")
	}
}

// promotions returns the promotion lines of a log.
func promotions(log string) (lines []string) {
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, "[V284] Handle promoted to primary") {
			lines = append(lines, l)
		}
	}
	return lines
}

// One line per real promotion, with its reason and whether a primary was displaced; none
// when the handle already was the primary.
func TestBecomePrimaryLogsOneLinePerPromotion(t *testing.T) {
	newTestApp(t)
	useTestGlobals(t)
	buf := captureLog()
	const path = "/test/promote-log.mkv"
	old, next := newTestHandle(path), newTestHandle(path)
	registerHandle(t, old, next)

	if !becomePrimary(old, promoteSequentialReads, 12*mb, 3) {
		t.Fatal("the first promotion must report true")
	}
	lines := promotions(buf.String())
	if len(lines) != 1 || !strings.Contains(lines[0], "reason=sequential_reads had_primary=false off=12MB reads=3") || !strings.Contains(lines[0], "file=promote-log.mkv") {
		t.Fatalf("first promotion logged %q", lines)
	}

	if !becomePrimary(next, promoteSeekDuringPlayback, 90*mb, 0) {
		t.Fatal("a promotion over a primary must report true")
	}
	lines = promotions(buf.String())
	if len(lines) != 2 || !strings.Contains(lines[1], "reason=seek_during_playback had_primary=true off=90MB reads=0") {
		t.Fatalf("second promotion logged %q", lines)
	}

	if becomePrimary(next, promoteSequentialReads, 95*mb, 3) {
		t.Fatal("promoting the primary again must report false")
	}
	if got := len(promotions(buf.String())); got != 2 {
		t.Errorf("a promotion that changed nothing logged: %d lines", got)
	}
}

// A primary on another path is not "another primary" for had_primary.
func TestBecomePrimaryHadPrimaryIgnoresOtherPaths(t *testing.T) {
	newTestApp(t)
	useTestGlobals(t)
	buf := captureLog()
	elsewhere, h := newTestHandle("/test/else.mkv"), newTestHandle("/test/mine.mkv")
	elsewhere.isPrimaryHandle.Store(true)
	registerHandle(t, elsewhere, h)

	becomePrimary(h, promoteSequentialReads, 0, 0)
	if lines := promotions(buf.String()); len(lines) != 1 || !strings.Contains(lines[0], "had_primary=false") {
		t.Fatalf("logged %q", lines)
	}
}

// The sequential-reads path names itself and reports its streak.
func TestPromoteIfStreamingLogsSequentialReads(t *testing.T) {
	newTestApp(t)
	useTestGlobals(t)
	buf := captureLog()
	const (
		path    = "/test/promote-log-seq.mkv"
		readLen = 256 * 1024
	)
	app.playbackRegistry.Store(path, &PlaybackState{Path: path, IsHealthy: true})
	t.Cleanup(func() { app.playbackRegistry.Delete(path) })
	h := newTestHandle(path)
	registerHandle(t, h)
	off := int64(1 * gb)
	for i := 0; i <= primaryProofReads+2; i++ {
		h.promoteIfStreaming(off-readLen, off, readLen)
		off += readLen
	}
	lines := promotions(buf.String())
	want := fmt.Sprintf("reason=sequential_reads had_primary=false off=%dMB reads=%d", (1*gb+int64(primaryProofReads-1)*readLen)/mb, primaryProofReads)
	if len(lines) != 1 || !strings.Contains(lines[0], want) {
		t.Fatalf("logged %q, want one line with %q", lines, want)
	}
}

// The creator of a pump is its primary: reason pump_creator.
func TestStartNativePumpLogsPumpCreator(t *testing.T) {
	fe := useEngineReadPath(t)
	const path = "/test/pump-creator-log.mkv"
	h, _ := pumpHandle(path, 10*mb)
	buf := captureLog()
	gate := make(chan struct{})
	fe.newRead = func() *fakeReader {
		return &fakeReader{readFn: func(_ int, p []byte, off int64) (int, error) {
			<-gate
			fillPattern(p, off)
			return len(p), nil
		}}
	}
	t.Cleanup(func() { app.activePumps.Delete(path) })

	h.startNativePump(testHash, 3)
	lines := promotions(buf.String())
	if len(lines) != 1 || !strings.Contains(lines[0], "reason=pump_creator had_primary=false") {
		t.Fatalf("logged %q", lines)
	}
	close(gate)
	waitUntil(t, "the pump to finish", func() bool { return !h.hasSlot.Load() })
}

// The first handle back on a live pump takes the primary over: reason attach_first_ref. A
// second handle joining a pump that has references is no promotion.
func TestStartNativePumpLogsAttachFirstRef(t *testing.T) {
	for _, c := range []struct {
		name string
		refs int32
		want int
	}{
		{"a reconnect after every handle left", 0, 1},
		{"a second handle joins a live pump", 1, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			useEngineReadPath(t)
			path := fmt.Sprintf("/test/pump-attach-log-%d.mkv", c.refs)
			h, _ := pumpHandle(path, 100*mb)
			h.lastOff = -1
			buf := captureLog()
			ps := &NativePumpState{path: path, refCount: c.refs, reader: &fakeReader{}, cancel: func() {}, playerOff: 50 * mb}
			app.activePumps.Store(path, ps)
			t.Cleanup(func() { app.activePumps.Delete(path) })

			h.startNativePump(testHash, 3)
			lines := promotions(buf.String())
			if len(lines) != c.want {
				t.Fatalf("logged %q, want %d lines", lines, c.want)
			}
			if c.want == 1 && !strings.Contains(lines[0], "reason=attach_first_ref had_primary=false off=50MB") {
				t.Errorf("logged %q", lines[0])
			}
		})
	}
}

// A genuine seek during playback promotes a secondary handle: reason seek_during_playback,
// with the displaced primary reported.
func TestSeekDuringPlaybackLogsItsReason(t *testing.T) {
	useEngineReadPath(t)
	const path = "/test/seek-promote-log.mkv"
	app.playbackRegistry.Store(path, &PlaybackState{Path: path, IsHealthy: true})
	t.Cleanup(func() { app.playbackRegistry.Delete(path); app.activePumps.Delete(path) })
	incumbent, h := engineHandle(path, 200*mb), engineHandle(path, 200*mb)
	incumbent.isPrimaryHandle.Store(true)
	h.nativeReader = &fakeReader{}
	h.lastOff = 10 * mb
	app.activePumps.Store(path, &NativePumpState{})
	registerHandle(t, incumbent, h)
	buf := captureLog()

	readBytes(t, h, 256*1024, 10*mb+64*mb+mb)
	lines := promotions(buf.String())
	if len(lines) != 1 || !strings.Contains(lines[0], "reason=seek_during_playback had_primary=true off=75MB") {
		t.Fatalf("logged %q", lines)
	}
	if !h.isPrimaryHandle.Load() || incumbent.isPrimaryHandle.Load() {
		t.Error("the seeking handle must have taken the primary")
	}
}
