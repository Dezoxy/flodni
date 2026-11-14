package daemon

import (
	"log"
	"strings"
	"sync"
	"time"

	"flodni/internal/gostorm/native"
	"flodni/internal/metadb"
)

// Values of last_failure_reason and last_failure_source in the readability record.
// New values may be added; the README tells readers to ignore the ones they do not
// know.
const (
	failReasonNoPeers   = "no_peers"   // the engine had the torrent, with no connection
	failReasonTimeout   = "timeout"    // peers were connected and the read still gave up
	failReasonNotActive = "not_active" // the engine did not have the torrent running
	failReasonUnknown   = "unknown"    // the classification itself timed out

	// failReasonMetainfoTimeout: the wake's wait for the metainfo ran out, so the swarm
	// could not even deliver it. It counts as a swarm-side failure (a private torrent
	// with no peers looks like this), once per metainfoTimeoutWindow per torrent.
	failReasonMetainfoTimeout = "metainfo_timeout"

	// metainfoTimeoutWindow: one occasion. A wake waits 45s and a player retries, so a
	// burst runs several minutes; the 2-minute session close would split it into two
	// or three counts. Failures that prove a dead swarm are spread over time anyway.
	metainfoTimeoutWindow = 10 * time.Minute

	// failSourceWake: a wake, from a mount open or a warm-up, whose metainfo never arrived.
	failSourceWake = "wake"

	// failSourceMount: a reader through the FUSE mount. The verdict comes from any
	// mount session of sessionVerdictFloor or more, scanners included.
	failSourceMount = "mount"

	// classifyTimeout bounds the engine calls made on a failing FUSE read's way out:
	// Stats() takes an engine lock, and that read is already late.
	classifyTimeout = time.Second
)

// deadSwarmVerdicts counts the verdicts the recorder saw since start, by reason and
// source, for /metrics/prometheus. Verdicts are rare, so a mutex is fine. The
// recorder is installed only with the state database, so without one these stay 0.
var deadSwarmVerdicts = struct {
	sync.Mutex
	n map[[2]string]int64
}{n: map[[2]string]int64{
	{failReasonNoPeers, failSourceMount}:        0,
	{failReasonTimeout, failSourceMount}:        0,
	{failReasonNotActive, failSourceMount}:      0,
	{failReasonUnknown, failSourceMount}:        0,
	{failReasonMetainfoTimeout, failSourceWake}: 0,
}}

// readabilityNow is the clock of the metainfo-timeout window; tests replace it.
var readabilityNow = time.Now

func countDeadSwarmVerdict(reason, source string) {
	deadSwarmVerdicts.Lock()
	deadSwarmVerdicts.n[[2]string{reason, source}]++
	deadSwarmVerdicts.Unlock()
}

// deadSwarmVerdictCounts returns a copy of the counts.
func deadSwarmVerdictCounts() map[[2]string]int64 {
	deadSwarmVerdicts.Lock()
	defer deadSwarmVerdicts.Unlock()
	out := make(map[[2]string]int64, len(deadSwarmVerdicts.n))
	for k, v := range deadSwarmVerdicts.n {
		out[k] = v
	}
	return out
}

// classifyFailure says why a read gave up on hash. It asks the engine under a short
// bound and answers unknown when the engine does not reply in time.
func classifyFailure(hash string) string {
	if hash == "" {
		return failReasonUnknown
	}
	res := make(chan string, 1)
	live, peers := torrentLive, sessionActivePeers
	go func() {
		switch {
		case !live(hash):
			res <- failReasonNotActive
		case peers(hash) == 0:
			res <- failReasonNoPeers
		default:
			res <- failReasonTimeout
		}
	}()
	select {
	case r := <-res:
		return r
	case <-time.After(classifyTimeout):
		return failReasonUnknown
	}
}

// readabilityStore is the part of the state database the recorder writes to.
type readabilityStore interface {
	RecordMetadataFailure(hash, reason, source string, at time.Time) error
	RecordSwarmRead(hash string, at time.Time) error
	ResetMetadataFailures(hash string) error
	RecordNotStored(hash, source string, at time.Time) error
}

// newReachabilityRecorder returns the native.ReachabilityOutcome hook that writes the
// readability record. Only bytes from the swarm stamp a read; a metainfo taken from it
// only ends the streak.
func newReachabilityRecorder(store readabilityStore, logger *log.Logger) func(native.Outcome) {
	var mu sync.Mutex
	lastTimeout := map[string]time.Time{} // hash -> last metainfo timeout counted
	// countTimeout reports whether this timeout starts a new occasion for hash.
	countTimeout := func(hash string, at time.Time) bool {
		mu.Lock()
		defer mu.Unlock()
		if last, ok := lastTimeout[hash]; ok && at.Sub(last) < metainfoTimeoutWindow {
			return false
		}
		for h, last := range lastTimeout { // bounded: only occasions inside the window stay
			if at.Sub(last) >= metainfoTimeoutWindow {
				delete(lastTimeout, h)
			}
		}
		lastTimeout[hash] = at
		return true
	}
	return func(o native.Outcome) {
		var err error
		switch o.Kind {
		case native.OutcomeSwarmRead:
			err = store.RecordSwarmRead(o.Hash, time.Now())
		case native.OutcomeMetainfo:
			err = store.ResetMetadataFailures(o.Hash)
		case native.OutcomeNotStored:
			// Deliberately not countDeadSwarmVerdict and not RecordMetadataFailure: the
			// swarm was never asked.
			err = store.RecordNotStored(o.Hash, o.Source, time.Now())
		case native.OutcomeMetainfoTimeout:
			at := readabilityNow()
			if !countTimeout(strings.ToLower(o.Hash), at) {
				return
			}
			countDeadSwarmVerdict(failReasonMetainfoTimeout, failSourceWake)
			err = store.RecordMetadataFailure(o.Hash, failReasonMetainfoTimeout, failSourceWake, at)
		case native.OutcomeFailed:
			short := o.Hash
			if len(short) > 8 {
				short = short[:8]
			}
			// Deliberately does not say why: the session layer that condemned
			// already logged its own reason, with duration and outcome.
			logger.Printf("[DeadSwarm] %s did not answer", short)
			// Counted before the store call, so a database error does not hide a verdict.
			countDeadSwarmVerdict(o.Reason, o.Source)
			err = store.RecordMetadataFailure(o.Hash, o.Reason, o.Source, time.Now())
		}
		if err != nil {
			logger.Printf("WARNING: reachability bookkeeping for %s: %v", o.Hash, err)
		}
	}
}

// Compile-time pins: the library manager asserts this signature on the registry at
// run time, so a rename would stop the deletes without a compile error.
var (
	_ readabilityStore                                = (*metadb.DB)(nil)
	_ interface{ ClearMetadataFailure(string) error } = (*metadb.DB)(nil)
)
