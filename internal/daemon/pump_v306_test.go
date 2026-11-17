package daemon

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"flodni/internal/flog"
)

// lockedBuf lets the pump goroutine log while the test reads.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureLog points the package logger at a buffer. Call it after newPumpSim: the sim's
// test app owns the logger global and restores it after the pump has ended.
func captureLog() *lockedBuf {
	buf := &lockedBuf{}
	logger = flog.NewText(buf)
	return buf
}

const (
	pumpEnded = "[V239] Native Pump Goroutine Ended"
	v306EOF   = "[V306] Pump reached end of file during playback"
	v306Early = "[V306] WARNING: pump goroutine exited during healthy playback"
)

// A pump that reads a file to its end during confirmed playback is a normal end, not a
// warning (upstream found most V306 warnings were exactly this).
func TestV306PumpReachedEOFLogsInfoNotWarning(t *testing.T) {
	sim := newPumpSim(t, "/test/v306-eof.mkv", 12*mb, 0, 0)
	buf := captureLog()
	sim.open("A")
	waitUntil(t, "the pump's last log line", func() bool { return strings.Contains(buf.String(), pumpEnded) })

	out := buf.String()
	if !strings.Contains(out, v306EOF) {
		t.Fatalf("no end-of-file line in:\n%s", out)
	}
	if strings.Contains(out, v306Early) {
		t.Fatalf("a pump that read to the end was warned about:\n%s", out)
	}
}

// A pump cancelled mid-file during confirmed playback is a real early exit and keeps the warning.
func TestV306PumpCancelledMidFileWarns(t *testing.T) {
	sim := newPumpSim(t, "/test/v306-early.mkv", 1000*mb, 0, 0)
	buf := captureLog()
	sim.open("A")
	sim.settle() // parked at its lead limit, far from the end
	sim.endPump()
	waitUntil(t, "the pump's last log line", func() bool { return strings.Contains(buf.String(), pumpEnded) })

	out := buf.String()
	if !strings.Contains(out, v306Early) {
		t.Fatalf("no early-exit warning in:\n%s", out)
	}
	if strings.Contains(out, v306EOF) {
		t.Fatalf("a cancelled pump was reported as reaching the end:\n%s", out)
	}
}
