package daemon

import (
	"strings"
	"testing"
	"time"

	"flodni/internal/promtext"
)

func gaugeBody(t *testing.T) string {
	t.Helper()
	var w promtext.Writer
	collectPumpWatchdogProm(&w)
	if err := promtext.Lint(w.Bytes()); err != nil {
		t.Fatal(err)
	}
	return string(w.Bytes())
}

// A pump that is gone while the player plays (here it is cancelled; the handle the player
// reads through attached to it and still believes it has a pump) is noticed by the gauge at
// once and replaced by the watchdog once it has been missing for the grace period.
func TestTheWatchdogRestartsAPumpThatWentMissingDuringPlayback(t *testing.T) {
	const path = "/test/watchdog.mkv"
	sim := newPumpSim(t, path, eofSize, eofAt, probeFetch)
	buf := captureLog()
	a := sim.open("A")
	b := sim.open("B") // attaches to A's pump
	sim.settle()
	a.release()
	b.readRange(eofAt, 40*probeStep, probeStep)
	p := sim.open("P")
	p.readRange(0, 1*mb, probeStep)
	sim.settle()
	if body := gaugeBody(t); !strings.Contains(body, "flodni_playing_sessions_without_pump 0\n") {
		t.Fatalf("gauge with a pump:\n%s", body)
	}

	sim.endPump()
	if body := gaugeBody(t); !strings.Contains(body, "flodni_playing_sessions_without_pump 1\n") {
		t.Fatalf("gauge without a pump:\n%s", body)
	}
	restarts := pumpWatchdogRestarts.Load()
	w := newPumpWatchdog()
	t0 := time.Now()
	w.tick(t0)
	w.tick(t0.Add(pumpWatchdogGrace - time.Second))
	if pumpRegistered(path) {
		t.Fatal("the watchdog restarted the pump before the grace period")
	}
	w.tick(t0.Add(pumpWatchdogGrace))
	if !pumpRegistered(path) {
		t.Fatal("no pump after the grace period")
	}
	if got := pumpWatchdogRestarts.Load() - restarts; got != 1 {
		t.Errorf("restarts counted = %d, want 1", got)
	}
	if !strings.Contains(buf.String(), "[Pump] watchdog: no pump for watchdog.mkv during playback for 10s; restarting") {
		t.Errorf("no watchdog line in:\n%s", buf.String())
	}

	next := eofAt + 40*probeStep
	b.readRange(next, 8*probeStep, probeStep)
	pumpFollows(t, sim, next)
	if body := gaugeBody(t); !strings.Contains(body, "flodni_playing_sessions_without_pump 0\n") {
		t.Errorf("gauge after the restart:\n%s", body)
	}
	if got := sim.primaryOwner(); got != "[B]" {
		t.Errorf("primary owner = %s, want [B]", got)
	}
	if got, want := sim.resumeOff(), next+7*probeStep; got != want {
		t.Errorf("resume position = %d MiB, want %d MiB", got/mb, want/mb)
	}
}

// A file with no playback, or a playback with no handle, is not a session without a pump.
func TestAFileNobodyPlaysIsNotASessionWithoutAPump(t *testing.T) {
	sim := newPumpSim(t, "/test/watchdog-idle.mkv", eofSize, eofAt, 0)
	a := sim.open("A")
	sim.settle()
	a.release()
	sim.endPump()
	if body := gaugeBody(t); !strings.Contains(body, "flodni_playing_sessions_without_pump 0\n") {
		t.Errorf("gauge with no handle open:\n%s", body)
	}
}
