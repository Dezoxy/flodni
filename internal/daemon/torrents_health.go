package daemon

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"

	"flodni/internal/metadb"
)

// readabilityLister is the read side of the readability record.
type readabilityLister interface {
	ListReadability(minFailures int) ([]metadb.Readability, error)
}

type torrentHealth struct {
	Hash                string  `json:"hash"`
	ConsecutiveFailures int64   `json:"consecutive_failures"`
	FirstFailureAt      *string `json:"first_failure_at"`
	LastFailureAt       *string `json:"last_failure_at"`
	LastFailureReason   *string `json:"last_failure_reason"`
	LastFailureSource   *string `json:"last_failure_source"`
	LastSwarmReadAt     *string `json:"last_swarm_read_at"`
	// Files and Swarm are EXPERIMENTAL (README). Both stay after last_swarm_read_at: the
	// e2e test matches "hash":"..."[^}]*"last_swarm_read_at", which a nested object before
	// it would break. Swarm is the last key.
	Files []fileHealth `json:"files"`
	Swarm *swarmHealth `json:"swarm"`
}

// fileHealth is the per-file warm record as served. Times are RFC 3339 or null; an
// empty readability or cache is served as "unknown". Readability is about the swarm,
// cache about the windows on disk; eviction changes only cache.
type fileHealth struct {
	FileIndex           int     `json:"file_index"`
	Readability         string  `json:"readability"`
	Cache               string  `json:"cache"`
	FailedAttempts      int64   `json:"failed_attempts"`
	FirstFailedAt       *string `json:"first_failed_at"`
	LastAttemptAt       *string `json:"last_attempt_at"`
	LastSwarmDeliveryAt *string `json:"last_swarm_delivery_at"`
}

func unixOrNil(sec int64) *string {
	if sec <= 0 {
		return nil
	}
	s := time.Unix(sec, 0).UTC().Format(time.RFC3339)
	return &s
}

func filesOrEmpty(fs []fileHealth) []fileHealth {
	if fs == nil {
		return []fileHealth{}
	}
	return fs
}

func strOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// newTorrentsHealthHandler serves GET /api/torrents/health: the readability record
// as stored, per torrent, each with its experimental per-file records (files may be nil).
// A torrent that has file records and no failure row is listed with a count of 0 and
// nulls, unless a failure filter is set: ?failing and ?min_failures keep their
// per-torrent meaning. A nil store (state database off or failed to open) answers
// 503, because nothing is recorded then.
func newTorrentsHealthHandler(store readabilityLister, files func() map[string][]fileHealth, logger *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if store == nil {
			http.Error(w, "readability record unavailable: state database is off", http.StatusServiceUnavailable)
			return
		}
		q := r.URL.Query()
		minFailures := 0
		switch q.Get("failing") {
		case "", "0":
		case "1":
			minFailures = 1
		default:
			http.Error(w, "failing must be 0 or 1", http.StatusBadRequest)
			return
		}
		if v := q.Get("min_failures"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				http.Error(w, "min_failures must be a non-negative integer", http.StatusBadRequest)
				return
			}
			minFailures = max(minFailures, n)
		}
		rows, err := store.ListReadability(minFailures)
		if err != nil {
			logger.Printf("WARNING: readability record: %v", err)
			http.Error(w, "cannot read the readability record", http.StatusInternalServerError)
			return
		}
		var byHash map[string][]fileHealth
		if files != nil {
			byHash = files()
		}
		out := make([]torrentHealth, 0, len(rows))
		listed := make(map[string]bool, len(rows))
		for _, row := range rows {
			listed[row.Hash] = true
			out = append(out, torrentHealth{
				Hash:                row.Hash,
				ConsecutiveFailures: row.FailCount,
				FirstFailureAt:      unixOrNil(row.FirstFail),
				LastFailureAt:       unixOrNil(row.LastFail),
				LastFailureReason:   strOrNil(row.LastReason),
				LastFailureSource:   strOrNil(row.LastSource),
				LastSwarmReadAt:     unixOrNil(row.LastSwarmRead),
				Files:               filesOrEmpty(byHash[row.Hash]),
				Swarm:               swarmFor(row.Hash),
			})
		}
		if minFailures == 0 {
			for hash, fs := range byHash {
				if !listed[hash] {
					out = append(out, torrentHealth{Hash: hash, Files: filesOrEmpty(fs), Swarm: swarmFor(hash)})
				}
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Hash < out[j].Hash })
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Total    int             `json:"total"`
			Torrents []torrentHealth `json:"torrents"`
		}{Total: len(out), Torrents: out})
	}
}
