package metadb

import (
	"fmt"
	"strings"
	"time"
)

// RecordMetadataFailure counts one failed reachability check for a torrent: a
// session that ended with a read that gave up and nothing served by the swarm.
// The reason and source are kept for the last failure. first_fail is the start of
// the current streak: it is kept while the count runs and restarted after a reset,
// so a burst inside one window cannot reach a threshold: what proves a dead swarm
// is failures spread over time, not their number. A metainfo that never arrives is
// counted by the daemon's recorder as reason metainfo_timeout, once per occasion. The table name predates the readability record and is kept
// for the databases already in production.
func (d *DB) RecordMetadataFailure(hash, reason, source string, at time.Time) error {
	now := at.Unix()
	_, err := d.db.Exec(`
		INSERT INTO metadata_failures (hash, fail_count, first_fail, last_fail, last_fail_reason, last_fail_source)
		VALUES (?, 1, ?, ?, ?, ?)
		ON CONFLICT(hash) DO UPDATE SET
			first_fail       = CASE WHEN fail_count = 0 THEN excluded.first_fail ELSE first_fail END,
			fail_count       = fail_count + 1,
			last_fail        = excluded.last_fail,
			last_fail_reason = excluded.last_fail_reason,
			last_fail_source = excluded.last_fail_source`,
		strings.ToLower(hash), now, now, reason, source)
	return err
}

// RecordNotStored notes that a wake was refused because the engine keeps no record of
// the torrent. It is not a failed swarm read: fail_count and first_fail stay as they
// are (a row it inserts has both 0), so no streak starts and no dead-swarm threshold
// is ever reached by it. Only the last failure's time, reason and source are set, so
// the record says why the torrent is not readable.
func (d *DB) RecordNotStored(hash, source string, at time.Time) error {
	_, err := d.db.Exec(`
		INSERT INTO metadata_failures (hash, fail_count, first_fail, last_fail, last_fail_reason, last_fail_source)
		VALUES (?, 0, 0, ?, ?, ?)
		ON CONFLICT(hash) DO UPDATE SET
			last_fail        = excluded.last_fail,
			last_fail_reason = excluded.last_fail_reason,
			last_fail_source = excluded.last_fail_source`,
		strings.ToLower(hash), at.Unix(), NotStoredReason, source)
	return err
}

// NotStoredReason is the last_fail_reason RecordNotStored writes.
const NotStoredReason = "not_stored"

// RecordSwarmRead notes that the swarm served a read: the streak ends and the time
// is stamped. The row is kept (inserted when absent) so the time survives; the last
// failure, its reason and its source stay as history.
func (d *DB) RecordSwarmRead(hash string, at time.Time) error {
	_, err := d.db.Exec(`
		INSERT INTO metadata_failures (hash, fail_count, first_fail, last_fail, last_swarm_read)
		VALUES (?, 0, 0, 0, ?)
		ON CONFLICT(hash) DO UPDATE SET
			fail_count      = 0,
			first_fail      = 0,
			last_swarm_read = MAX(last_swarm_read, excluded.last_swarm_read)`,
		strings.ToLower(hash), at.Unix())
	return err
}

// ResetMetadataFailures ends a streak without stamping a read, for a metainfo taken
// from the swarm: that proves peers exist, not that a file reads. It never inserts.
func (d *DB) ResetMetadataFailures(hash string) error {
	_, err := d.db.Exec(`
		UPDATE metadata_failures SET fail_count = 0, first_fail = 0
		WHERE hash = ? AND fail_count > 0`, strings.ToLower(hash))
	return err
}

// ClearMetadataFailure deletes the whole row, for a torrent that left the library:
// the same release added again later must not arrive already condemned. The
// library manager reaches it through an interface assertion on the registry, so the
// name and signature are pinned by a test.
func (d *DB) ClearMetadataFailure(hash string) error {
	_, err := d.db.Exec(`DELETE FROM metadata_failures WHERE hash = ?`, strings.ToLower(hash))
	return err
}

// Readability is one hash's stored record, times in Unix seconds, 0 when unset.
type Readability struct {
	Hash          string
	FailCount     int64
	FirstFail     int64
	LastFail      int64
	LastReason    string
	LastSource    string
	LastSwarmRead int64
}

// ListReadability returns every stored record with at least minFailures consecutive
// failures (all records when minFailures is 0 or less), ordered by hash.
func (d *DB) ListReadability(minFailures int) ([]Readability, error) {
	rows, err := d.db.Query(`
		SELECT hash, fail_count, first_fail, last_fail, last_fail_reason, last_fail_source, last_swarm_read
		FROM metadata_failures WHERE fail_count >= ? ORDER BY hash`, max(minFailures, 0))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Readability{}
	for rows.Next() {
		var r Readability
		if err := rows.Scan(&r.Hash, &r.FailCount, &r.FirstFail, &r.LastFail,
			&r.LastReason, &r.LastSource, &r.LastSwarmRead); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneReadability deletes records whose hash is not in keep (lower-case) and whose
// last failure and last swarm read are both older than before. The time guard spares
// a torrent that was added or read while the caller built its set. Returns the number
// of rows deleted.
func (d *DB) PruneReadability(keep map[string]bool, before time.Time) (int, error) {
	cutoff := before.Unix()
	rows, err := d.db.Query(`SELECT hash FROM metadata_failures WHERE last_fail < ? AND last_swarm_read < ?`, cutoff, cutoff)
	if err != nil {
		return 0, err
	}
	var drop []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if !keep[strings.ToLower(h)] {
			drop = append(drop, h)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(drop) == 0 {
		return 0, nil
	}
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	for _, h := range drop {
		if _, err := tx.Exec(`DELETE FROM metadata_failures WHERE hash = ?`, h); err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("prune %s: %w", h, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(drop), nil
}
