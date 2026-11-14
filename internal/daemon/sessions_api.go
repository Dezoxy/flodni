package daemon

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

const sessionRingSize = 200

// Outcomes reported by /api/sessions. They are derived at close time from what
// the session already knows, with the same tests the TTFF histograms and the
// swarm verdict use:
//
//	dead_swarm  reportSwarmVerdict logged the [DeadSwarm] line (returned true)
//	unserved    otherwise, no byte reached the caller (bytesRead == 0)
//	served      otherwise, ttffIsReal: 1MB+, or a tail/deep read over 64KB
//	probe       otherwise: only scan/probe-sized reads
//	open        not closed yet (open list only)
const (
	outcomeServed    = "served"
	outcomeProbe     = "probe"
	outcomeDeadSwarm = "dead_swarm"
	outcomeUnserved  = "unserved"
	outcomeOpen      = "open"
)

func sessionOutcome(dead bool, bytesServed int64, tailOrDeep bool) string {
	switch {
	case dead:
		return outcomeDeadSwarm
	case bytesServed == 0:
		return outcomeUnserved
	case ttffIsReal(tailOrDeep, bytesServed):
		return outcomeServed
	}
	return outcomeProbe
}

// sessionRecord is an immutable snapshot of one session.
type sessionRecord struct {
	Path        string `json:"path"`
	Hash        string `json:"hash"`
	Size        int64  `json:"size"`
	OpenedAt    string `json:"opened_at"`
	ClosedAt    string `json:"closed_at,omitempty"`
	BytesServed int64  `json:"bytes_served"`
	FirstByteMS *int64 `json:"first_byte_ms"`
	Reads       int64  `json:"reads"`
	FailedReads int64  `json:"failed_reads"`
	Outcome     string `json:"outcome"`
	// Added with the [Session] log line. Existing keys keep their names and order.
	FileIndex       int     `json:"file_index"`
	BytesFetchBlock int64   `json:"bytes_fetch_block"`
	BytesRACacheHit int64   `json:"bytes_racache_hit"`
	BytesWarmupHead int64   `json:"bytes_warmup_head"`
	BytesWarmupTail int64   `json:"bytes_warmup_tail"`
	SwarmServed     bool    `json:"swarm_served"`
	DurationMS      int64   `json:"duration_ms"`
	FailureReason   *string `json:"failure_reason"`
	// Added with the keep-alive counters. Per-session values are lower bounds: a play
	// with long gaps spans several sessions.
	Reopens    int64 `json:"reopens"`
	Stalls     int64 `json:"stalls"`
	MaxStallMS int64 `json:"max_stall_ms"`
	Confirmed  bool  `json:"confirmed"`
	// Added with resumed sessions. A read after a close opens a new session; ids are
	// per process (they restart at 1), resumed_from is 0 for a session opened fresh.
	ID          uint64 `json:"id"`
	ResumedFrom uint64 `json:"resumed_from"`
}

// record snapshots the session. closedAt is the zero time for an open session.
func (s *TTFFSession) record(closedAt time.Time, dead bool) sessionRecord {
	opened := s.openedAt.Load()
	r := sessionRecord{
		Path:        s.path,
		Hash:        s.hash,
		Size:        s.size,
		OpenedAt:    time.Unix(0, opened).UTC().Format(time.RFC3339),
		BytesServed: s.bytesRead.Load(),
		Reads:       s.reads.Load(),
		FailedReads: s.failedReads.Load(),
		Outcome:     outcomeOpen,

		FileIndex:       s.fileIndex,
		BytesFetchBlock: s.bytesFetchBlock.Load(),
		BytesRACacheHit: s.bytesRACacheHit.Load(),
		BytesWarmupHead: s.bytesWarmupHead.Load(),
		BytesWarmupTail: s.bytesWarmupTail.Load(),
		SwarmServed:     s.servedByNet.Load(),
		FailureReason:   s.failReason.Load(),
		Reopens:         s.reopens.Load(),
		Stalls:          s.stallCount.Load(),
		MaxStallMS:      s.maxStallMS.Load(),
		Confirmed:       s.confirmed.Load(),
		ID:              s.id,
		ResumedFrom:     s.resumedFrom,
	}
	end := time.Now()
	if fd := s.firstDataAt.Load(); fd > 0 {
		ms := max(time.Duration(fd-opened).Milliseconds(), 0)
		r.FirstByteMS = &ms
	}
	if !closedAt.IsZero() {
		end = closedAt
		r.ClosedAt = closedAt.UTC().Format(time.RFC3339)
		r.Outcome = sessionOutcome(dead, r.BytesServed, s.tailOrDeep.Load())
	}
	r.DurationMS = max(end.Sub(time.Unix(0, opened)).Milliseconds(), 0)
	return r
}

// sessionLogPrefix starts every session log line; the JSON object follows it.
const sessionLogPrefix = "[Session] "

// logSessionRecord writes one closed session as a single line. json.Marshal
// escapes control characters, so a path cannot add a second line.
func logSessionRecord(rec sessionRecord) {
	line, err := json.Marshal(rec)
	if err != nil {
		logger.Printf("WARNING: session log line for %s not written: %v", rec.Path, err)
		return
	}
	logger.Printf("%s%s", sessionLogPrefix, line)
}

// sessionRing keeps the most recent closed sessions, overwriting the oldest.
type sessionRing struct {
	mu   sync.Mutex
	buf  [sessionRingSize]sessionRecord
	next int // slot the next record goes to
	n    int // records held, up to sessionRingSize
}

var closedSessions sessionRing

func (r *sessionRing) add(rec sessionRecord) {
	r.mu.Lock()
	r.buf[r.next] = rec
	r.next = (r.next + 1) % sessionRingSize
	r.n = min(r.n+1, sessionRingSize)
	r.mu.Unlock()
}

// newestFirst returns a copy of the held records, newest first.
func (r *sessionRing) newestFirst() []sessionRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]sessionRecord, 0, r.n)
	for i := 1; i <= r.n; i++ {
		out = append(out, r.buf[(r.next-i+sessionRingSize)%sessionRingSize])
	}
	return out
}

func openSessionRecords() []sessionRecord {
	out := []sessionRecord{}
	sessions.Range(func(_, val any) bool {
		out = append(out, val.(*TTFFSession).record(time.Time{}, false))
		return true
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].OpenedAt != out[j].OpenedAt {
			return out[i].OpenedAt < out[j].OpenedAt
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func filterSessions(in []sessionRecord, outcome string, limit int) []sessionRecord {
	out := []sessionRecord{}
	for _, r := range in {
		if outcome != "" && r.Outcome != outcome {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, r)
	}
	return out
}

// handleSessions serves GET /api/sessions: {"open":[...],"closed":[...]}, closed
// newest first. outcome=<name> filters both lists, limit=N (1..200) caps closed.
func handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	outcome := q.Get("outcome")
	switch outcome {
	case "", outcomeServed, outcomeProbe, outcomeDeadSwarm, outcomeUnserved, outcomeOpen:
	default:
		http.Error(w, "unknown outcome", http.StatusBadRequest)
		return
	}
	limit := sessionRingSize
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		limit = min(n, sessionRingSize)
	}
	open := openSessionRecords()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Open   []sessionRecord `json:"open"`
		Closed []sessionRecord `json:"closed"`
	}{
		Open:   filterSessions(open, outcome, len(open)),
		Closed: filterSessions(closedSessions.newestFirst(), outcome, limit),
	})
}
