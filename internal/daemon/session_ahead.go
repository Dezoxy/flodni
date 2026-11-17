package daemon

import (
	"flodni/internal/gostorm/native"
	"flodni/internal/promtext"
	"sort"
	"time"
)

// Bytes ready ahead of the player, per open session. Computed when /api/sessions or the
// metrics are asked for, never on the read path.
const (
	// aheadScanCap bounds how far past the last read offset the scan looks, so ahead_bytes
	// never exceeds it and the piece snapshot stays small.
	aheadScanCap = int64(1 << 30)
	// aheadWarmupGrace is how long after open a session is left out of
	// flodni_sessions_min_ahead_bytes: a cold start has nothing buffered and would make the
	// minimum flap on every play.
	aheadWarmupGrace = 60 * time.Second
)

// aheadBoundsBytes are the flodni_session_ahead_bytes buckets: 1, 4, 16, 32, 64, 128, 256,
// 512, 768 and 1024 MiB.
var aheadBoundsBytes = []float64{1 << 20, 4 << 20, 16 << 20, 32 << 20, 64 << 20, 128 << 20, 256 << 20, 512 << 20, 768 << 20, 1024 << 20}

// sessionView is one entry of the open list: the record plus fields computed per request.
// The record's fields keep their names and order; the new ones follow.
type sessionView struct {
	sessionRecord
	SessionAgeSeconds *int64 `json:"session_age_seconds,omitempty"`
	LastReadOffset    *int64 `json:"last_read_offset,omitempty"`
	LastReadEnd       *int64 `json:"last_read_end,omitempty"`
	AheadBytes        *int64 `json:"ahead_bytes,omitempty"`
	FrontierOffset    *int64 `json:"frontier_offset,omitempty"`
}

var (
	sessionFilePieces = native.FilePieces
	sessionCacheRun   = func(path string, off, limit int64) int64 { return raCache.CoveredRun(path, off, limit) }
)

// engineEnd returns the end (exclusive, file offset, at most limit) of the run of verified
// complete pieces that starts at pos, or pos itself when the piece under pos is not complete.
func engineEnd(pm *native.FilePieceMap, pos, limit int64) int64 {
	for pos < limit {
		idx := (pm.FileOffset + pos) / pm.PieceLen
		k := int(idx) - pm.First
		if k < 0 || k >= len(pm.Complete) || !pm.Complete[k] {
			break
		}
		pos = min((idx+1)*pm.PieceLen-pm.FileOffset, limit)
	}
	return pos
}

// computeAhead measures from file offset from up to limit (exclusive; the caller clamps it
// to the file size and the scan cap).
//
// frontier is the end of the run of verified complete pieces that starts at from, the
// engine alone: from itself when the piece under it is not complete.
// ahead is how many bytes from from on can be read without waiting: it walks forward over
// the read-ahead cache and the engine's complete pieces alternately and stops at the first
// byte neither has, or at limit. So ahead >= frontier-from.
func computeAhead(from, limit int64, pm *native.FilePieceMap, cacheRun func(off, limit int64) int64) (ahead, frontier int64) {
	if pm == nil || pm.PieceLen <= 0 || from >= limit {
		return 0, from
	}
	frontier = engineEnd(pm, from, limit)
	pos := from
	// Each pass moves pos forward or ends the loop; the bound is only a guard against a
	// cache and an engine that keep trading one byte.
	for i := 0; i < 4096 && pos < limit; i++ {
		next := pos
		if run := cacheRun(next, limit); run > 0 {
			next += run
		}
		next = engineEnd(pm, next, limit)
		if next == pos {
			break
		}
		pos = next
	}
	return pos - from, frontier
}

// fillAhead adds the fields that need the engine. It leaves them unset for a session that
// has read nothing yet, whose stub carries no file index, or whose torrent the engine does
// not hold.
func (s *TTFFSession) fillAhead(v *sessionView) {
	off := s.lastOff.Load()
	if off < 0 {
		return
	}
	// Written lastEnd first, lastOff second (recordRead), so a torn read sees an end at or
	// past the one that belongs to off; never an end before off.
	end := max(s.lastEnd.Load(), off)
	v.LastReadOffset, v.LastReadEnd = &off, &end
	if s.fileIndex <= 0 || s.size <= 0 {
		return
	}
	if end >= s.size {
		ahead, frontier := int64(0), s.size
		v.AheadBytes, v.FrontierOffset = &ahead, &frontier
		return
	}
	limit := min(s.size, end+aheadScanCap)
	pm := sessionFilePieces(s.hash, s.fileIndex, end, limit)
	if pm == nil {
		return
	}
	ahead, frontier := computeAhead(end, limit, pm, func(o, l int64) int64 { return sessionCacheRun(s.path, o, l) })
	v.AheadBytes, v.FrontierOffset = &ahead, &frontier
}

// openSessionViews lists the open sessions, oldest first, with the per-request fields.
func openSessionViews(now time.Time) []sessionView {
	out := []sessionView{}
	sessions.Range(func(_, val any) bool {
		s := val.(*TTFFSession)
		v := sessionView{sessionRecord: s.record(time.Time{}, false)}
		age := max(int64(now.Sub(time.Unix(0, s.openedAt.Load())).Seconds()), 0)
		v.SessionAgeSeconds = &age
		s.fillAhead(&v)
		out = append(out, v)
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

// minAheadBytes is the smallest ahead_bytes over the sessions that are playing: confirmed by
// a play webhook, older than aheadWarmupGrace, with a computed ahead_bytes. false when none
// qualifies, so the gauge has no sample rather than a 0 that would read as "about to run dry".
func minAheadBytes(vs []sessionView) (int64, bool) {
	var best int64
	found := false
	for _, v := range vs {
		if !v.Confirmed || v.AheadBytes == nil || v.SessionAgeSeconds == nil ||
			*v.SessionAgeSeconds < int64(aheadWarmupGrace/time.Second) {
			continue
		}
		if !found || *v.AheadBytes < best {
			best, found = *v.AheadBytes, true
		}
	}
	return best, found
}

// aheadHistogram counts ahead_bytes of every open session that has one.
func aheadHistogram(vs []sessionView) promtext.Histogram {
	h := promtext.Histogram{Bounds: aheadBoundsBytes, Counts: make([]int64, len(aheadBoundsBytes)+1)}
	for _, v := range vs {
		if v.AheadBytes == nil {
			continue
		}
		i := sort.SearchFloat64s(aheadBoundsBytes, float64(*v.AheadBytes))
		h.Counts[i]++
		h.Sum += float64(*v.AheadBytes)
	}
	return h
}

// collectSessionAheadProm writes the two ahead families. Both are computed here, at scrape.
func collectSessionAheadProm(w *promtext.Writer) {
	views := openSessionViews(time.Now())
	var minSample []promtext.Sample
	if v, ok := minAheadBytes(views); ok {
		minSample = append(minSample, promSample(v))
	}
	w.Gauge("flodni_sessions_min_ahead_bytes",
		"Smallest ahead_bytes over open sessions that a play webhook confirmed and that are over 60 seconds old. No sample while there is none. Alert on it falling under 16 MiB.",
		minSample...)
	w.Histogram("flodni_session_ahead_bytes",
		"ahead_bytes of each open session at scrape: bytes the player can read from its last read offset without waiting, capped at 1 GiB.",
		aheadHistogram(views))
}
