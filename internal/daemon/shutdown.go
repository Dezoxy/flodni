package daemon

import (
	"sync/atomic"
	"time"
)

// Shutdown timing. Docker sends SIGTERM and then SIGKILL after 10 s by
// default (stop_grace_period). shutdownDeadline stays under that, so the
// process exits on its own terms; shutdownDetachAfter leaves the unmount 5 s
// to finish before a lazy detach frees a busy mount, with 3 s left for the
// detach to take effect.
const (
	shutdownDeadline    = 8 * time.Second
	shutdownDetachAfter = 5 * time.Second
)

// shutdownStarted is set when a signal starts the shutdown sequence.
var shutdownStarted atomic.Bool

// deferToShutdown reports whether main, once the FUSE server stops serving,
// should leave the exit to the shutdown sequence. A successful unmount ends
// the server before the sequence logs and picks the exit code, so exiting
// here would lose restartExitCode. Without a signal (an external unmount)
// main exits as before.
func deferToShutdown() bool { return shutdownStarted.Load() }

// shutdownSteps holds the shutdown actions, so tests can replace them.
type shutdownSteps struct {
	closeSessions   func() // give open sessions their verdict while the state DB is open
	saveState       func() // persist state first: it is the part that matters
	stopBackground  func()
	unmount         func() error
	detach          func() error // lazy detach of a mount that will not unmount
	restartRequired bool         // exit with restartExitCode instead of 0
	logf            func(format string, args ...any)
}

// shutdown runs the steps in order and returns the process exit code. If the
// unmount has not returned after detachAfter, it lazily detaches the mount.
// If the whole sequence has not finished after deadline, it returns anyway
// with the code it would have used: restartExitCode for a requested restart,
// else 0, because a stop was asked for and state is saved before the steps
// that can hang. The supervisor remounts either way.
func shutdown(s shutdownSteps, deadline, detachAfter time.Duration) int {
	code := 0
	if s.restartRequired {
		code = restartExitCode
	}
	var current atomic.Value
	current.Store("close sessions")
	done := make(chan struct{})

	run := func(name string, f func()) {
		current.Store(name)
		s.logf("Shutdown: %s...", name)
		f()
		s.logf("Shutdown: %s done", name)
	}

	go func() {
		defer close(done)
		run("close sessions", s.closeSessions)
		run("save state", s.saveState)
		run("stop background work", s.stopBackground)

		current.Store("unmount")
		s.logf("Shutdown: unmount...")
		unmounted := make(chan error, 1)
		go func() { unmounted <- s.unmount() }()
		var err error
		select {
		case err = <-unmounted:
		case <-time.After(detachAfter):
			s.logf("WARNING: unmount still running after %v, detaching the mount lazily", detachAfter)
			current.Store("lazy detach")
			if derr := s.detach(); derr != nil {
				s.logf("WARNING: lazy detach failed: %v", derr)
			}
			current.Store("unmount (after detach)")
			err = <-unmounted
		}
		if err != nil {
			s.logf("WARNING: unmount failed: %v", err)
		}
		s.logf("Shutdown: unmount done")
	}()

	select {
	case <-done:
	case <-time.After(deadline):
		s.logf("ERROR: shutdown not finished after %v, still in step %q; exiting anyway", deadline, current.Load())
	}
	return code
}
