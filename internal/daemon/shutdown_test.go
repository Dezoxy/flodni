package daemon

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// shutdownRecorder records step order and log lines from any goroutine.
type shutdownRecorder struct {
	mu    sync.Mutex
	steps []string
	logs  []string
}

func (r *shutdownRecorder) step(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, name)
}

func (r *shutdownRecorder) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

func (r *shutdownRecorder) joined() (string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.steps, ","), strings.Join(r.logs, "\n")
}

func (r *shutdownRecorder) stepsFor(restart bool) shutdownSteps {
	return shutdownSteps{
		closeSessions:   func() { r.step("sessions") },
		saveState:       func() { r.step("save") },
		stopBackground:  func() { r.step("stop") },
		unmount:         func() error { r.step("unmount"); return nil },
		detach:          func() error { r.step("detach"); return nil },
		restartRequired: restart,
		logf:            r.logf,
	}
}

func TestShutdownNormal(t *testing.T) {
	for _, tc := range []struct {
		restart bool
		want    int
	}{{false, 0}, {true, restartExitCode}} {
		r := &shutdownRecorder{}
		if got := shutdown(r.stepsFor(tc.restart), time.Second, 500*time.Millisecond); got != tc.want {
			t.Errorf("restart=%v: exit code = %d, want %d", tc.restart, got, tc.want)
		}
		if order, _ := r.joined(); order != "sessions,save,stop,unmount" {
			t.Errorf("restart=%v: steps = %q, want save,stop,unmount (no detach)", tc.restart, order)
		}
	}
}

func TestShutdownDetachUnblocksUnmount(t *testing.T) {
	r := &shutdownRecorder{}
	s := r.stepsFor(true)
	release := make(chan struct{})
	s.unmount = func() error { r.step("unmount"); <-release; return errors.New("transport endpoint is not connected") }
	s.detach = func() error { r.step("detach"); close(release); return nil }

	start := time.Now()
	code := shutdown(s, 2*time.Second, 20*time.Millisecond)
	if code != restartExitCode {
		t.Errorf("exit code = %d, want %d", code, restartExitCode)
	}
	if took := time.Since(start); took >= time.Second {
		t.Errorf("took %v, want well under the 2s deadline", took)
	}
	order, logs := r.joined()
	if order != "sessions,save,stop,unmount,detach" {
		t.Errorf("steps = %q, want save,stop,unmount,detach", order)
	}
	if !strings.Contains(logs, "WARNING: unmount still running") {
		t.Errorf("missing detach warning in logs:\n%s", logs)
	}
	if strings.Contains(logs, "ERROR") {
		t.Errorf("unexpected ERROR in logs:\n%s", logs)
	}
}

func TestShutdownDeadlineWhenUnmountAndDetachHang(t *testing.T) {
	r := &shutdownRecorder{}
	s := r.stepsFor(false)
	stuck := make(chan struct{})
	defer close(stuck) // lets the leaked goroutines finish after the test
	s.unmount = func() error { <-stuck; return nil }
	s.detach = func() error { r.step("detach"); <-stuck; return nil }

	start := time.Now()
	code := shutdown(s, 100*time.Millisecond, 20*time.Millisecond)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if took := time.Since(start); took < 100*time.Millisecond || took > time.Second {
		t.Errorf("took %v, want about the 100ms deadline", took)
	}
	_, logs := r.joined()
	if !strings.Contains(logs, "ERROR: shutdown not finished") || !strings.Contains(logs, `"lazy detach"`) {
		t.Errorf("log should name the stuck step:\n%s", logs)
	}
}

func TestShutdownDeadlineKeepsRestartCode(t *testing.T) {
	r := &shutdownRecorder{}
	s := r.stepsFor(true)
	stuck := make(chan struct{})
	defer close(stuck)
	s.saveState = func() { <-stuck }

	if code := shutdown(s, 50*time.Millisecond, 20*time.Millisecond); code != restartExitCode {
		t.Errorf("exit code = %d, want %d", code, restartExitCode)
	}
	if _, logs := r.joined(); !strings.Contains(logs, `"save state"`) {
		t.Errorf("log should name the save step:\n%s", logs)
	}
}

func TestShutdownSlowSaveWithinDeadline(t *testing.T) {
	r := &shutdownRecorder{}
	s := r.stepsFor(false)
	s.saveState = func() { time.Sleep(60 * time.Millisecond); r.step("save") }

	if code := shutdown(s, time.Second, 500*time.Millisecond); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	order, logs := r.joined()
	if order != "sessions,save,stop,unmount" {
		t.Errorf("steps = %q, want save,stop,unmount", order)
	}
	if strings.Contains(logs, "ERROR") {
		t.Errorf("unexpected ERROR in logs:\n%s", logs)
	}
}

func TestDeferToShutdown(t *testing.T) {
	shutdownStarted.Store(false)
	if deferToShutdown() {
		t.Error("deferToShutdown() = true without a signal, want false")
	}
	shutdownStarted.Store(true)
	defer shutdownStarted.Store(false)
	if !deferToShutdown() {
		t.Error("deferToShutdown() = false after shutdown started, want true")
	}
}
