package daemon

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sort"
	"sync/atomic"

	"github.com/anacrolix/torrent"

	"flodni/internal/gostorm/native"
	"flodni/internal/gostorm/settings"
	"flodni/internal/gostorm/torr"
	"flodni/internal/gostorm/torr/storage/torrstor"
	"flodni/internal/promtext"
	"flodni/internal/readahead"
	"flodni/internal/tlsverify"
	"flodni/internal/warmup"
)

// handlePrometheus serves GET /metrics/prometheus: the text exposition of the
// counters and histograms the daemon already keeps. It reads counters the daemon
// keeps in memory and never calls the engine, so a scrape takes no engine lock. It sits behind the
// control token like every route but /control and /api/health.
func handlePrometheus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var pw promtext.Writer
	collectProm(&pw)
	if err := pw.Err(); err != nil {
		logger.Warn(fmt.Sprintf("WARNING: metrics/prometheus not served: %v", err), "err", err)
		http.Error(w, "metrics unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", promtext.ContentType)
	_, _ = w.Write(pw.Bytes())
}

// ttffBoundsSeconds are the histogram bounds in seconds. The histograms keep
// microseconds, so a read under 1 ms lands in le="0.001" and adds its fraction to _sum.
var ttffBoundsSeconds = func() []float64 {
	out := make([]float64, len(ttffBucketsMS))
	for i, ms := range ttffBucketsMS {
		out[i] = float64(ms) / 1000
	}
	return out
}()

func promHistogram(h *TTFFHistogram, labels ...promtext.Label) promtext.Histogram {
	counts, sumUS := h.snapshot()
	return promtext.Histogram{
		Labels: labels,
		Bounds: ttffBoundsSeconds,
		Counts: counts[:],
		Sum:    float64(sumUS) / 1e6,
	}
}

func promSample(v int64, labels ...promtext.Label) promtext.Sample {
	return promtext.Sample{Labels: labels, Value: float64(v)}
}

func lbl(name, value string) promtext.Label { return promtext.Label{Name: name, Value: value} }

// collectProm writes every series. Label values are emitted at 0 from the start so
// a rate() has a series to start from.
func collectProm(w *promtext.Writer) {
	w.Gauge("flodni_build_info", "Always 1; the version is in the label.",
		promSample(1, lbl("version", AppVersion)))
	w.Gauge("process_start_time_seconds", "Start time of the process, seconds since the Unix epoch.",
		promSample(startTime.Unix()))

	w.Histogram("flodni_session_first_byte_seconds",
		"Open to the first byte served, per session classed as real playback..",
		promHistogram(&ttffStats.openToHeader))
	w.Histogram("flodni_session_first_deep_read_seconds",
		"Open to the first read at 8 MiB or later or in the tail, per session classed as real playback. confirmed is whether a file of the torrent had a webhook-confirmed playback when that read happened (as in flodni_torrent_idle_closes_total). Sums over confirmed to what it was before the label.",
		promHistogram(&ttffStats.openToDeepReadBy[1], lbl("confirmed", "true")),
		promHistogram(&ttffStats.openToDeepReadBy[0], lbl("confirmed", "false")))
	w.Histogram("flodni_session_first_deep_read_blocked_seconds",
		"The sum of the durations of a session's served reads from open up to and including its first deep read (8 MiB or later, or in the tail), per session classed as real playback: how long the reads kept the player waiting before it got there, where flodni_session_first_deep_read_seconds is the wall time. Reads that overlap each add their own time. confirmed as in flodni_session_first_deep_read_seconds.",
		promHistogram(&ttffStats.deepReadBlocked[1], lbl("confirmed", "true")),
		promHistogram(&ttffStats.deepReadBlocked[0], lbl("confirmed", "false")))
	w.Histogram("flodni_seek_read_duration_seconds",
		"Duration of the read that followed a seek..",
		promHistogram(&ttffStats.seekLatency))

	sources := []struct {
		src  ttffSource
		hist *TTFFHistogram
	}{
		{srcWarmupHead, &ttffStats.warmupHead},
		{srcWarmupTail, &ttffStats.warmupTail},
		{srcRACacheHit, &ttffStats.raCacheHit},
		{srcFetchBlock, &ttffStats.fetchBlock},
	}
	hists := make([]promtext.Histogram, 0, len(sources))
	bytesBySource := make([]promtext.Sample, 0, len(sources))
	for _, s := range sources {
		hists = append(hists, promHistogram(s.hist, lbl("source", s.src.String())))
		bytesBySource = append(bytesBySource, promSample(ttffStats.readBytes[s.src].Load(), lbl("source", s.src.String())))
	}
	w.Histogram("flodni_read_duration_seconds",
		"Duration of each served read by the path that answered it, not by where the bytes came from..",
		hists...)
	w.Counter("flodni_read_bytes_total",
		"Bytes handed to the kernel by the path that answered. Counts reads outside any session too, so it differs from the [Session] lines.",
		bytesBySource...)

	closed := make([]promtext.Sample, 0, len(sessionOutcomes))
	for i, o := range sessionOutcomes {
		closed = append(closed, promSample(sessionsClosedByOutcome[i].Load(), lbl("outcome", o)))
	}
	w.Counter("flodni_sessions_closed_total", "Closed sessions by outcome.", closed...)

	verdicts := deadSwarmVerdictCounts()
	keys := make([][2]string, 0, len(verdicts))
	for k := range verdicts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][1] != keys[j][1] {
			return keys[i][1] < keys[j][1]
		}
		return keys[i][0] < keys[j][0]
	})
	verdictSamples := make([]promtext.Sample, 0, len(keys))
	for _, k := range keys {
		verdictSamples = append(verdictSamples, promSample(verdicts[k], lbl("reason", k[0]), lbl("source", k[1])))
	}
	w.Counter("flodni_dead_swarm_verdicts_total",
		"Dead-swarm verdicts by reason and source. Stays 0 when the state database is off.",
		verdictSamples...)

	collectEngineReadsProm(w)
	collectProcessProm(w)
	w.Gauge("flodni_sessions_open", "Sessions open now: the open list of /api/sessions.",
		promSample(int64(openSessionCount())))
	collectSessionAheadProm(w)

	collectWarmupProm(w)
	collectPlaybackProm(w)
	collectPumpWatchdogProm(w)
	collectReaderBudgetProm(w)
	collectWebhookHealthProm(w)
	collectReadabilityProm(w)
	collectUnstoredProm(w)
	collectUnlinkedProm(w)
	collectSwarmProm(w)
	collectScrapeProm(w)
	collectReadAheadProm(w)
	collectStubIndexProm(w)
	collectAuthRefusalsProm(w)

	w.Counter("flodni_fuse_short_reads_total",
		"Reads served with fewer bytes than requested away from EOF.",
		promSample(fuseShortReadCount.Load()))
	w.Counter("flodni_fuse_short_read_results_total",
		"Outcome of the refill after a short read.",
		promSample(fuseShortReadRepaired.Load(), lbl("result", "repaired")),
		promSample(fuseShortReadFailed.Load(), lbl("result", "failed")))
	collectFuseReadAheadProm(w)
	w.Counter("flodni_fetch_dedup_total",
		"Concurrent fetches of the same chunk that shared one fetch.",
		promSample(fetchFlightDedupCount.Load()))
}

// collectFuseReadAheadProm writes the mount's kernel read-ahead in bytes, read from sysfs at
// each scrape (one small file). No series before the mount's BDI is known, on a
// platform without sysfs, or when the file cannot be read: a zero would read as
// "read-ahead off".
func collectFuseReadAheadProm(w *promtext.Writer) {
	bdi := mountBDI.Load()
	if bdi == nil {
		return
	}
	kb, err := readAheadKB(sysBDIRoot, *bdi)
	if err != nil {
		return
	}
	w.Gauge("flodni_fuse_read_ahead_bytes",
		"Kernel read-ahead of the FUSE mount in bytes (sysfs read_ahead_kb times 1024), read at each scrape. Flodni never writes it; the host may tune it. Absent when the file cannot be read.",
		promSample(kb*1024))
}

func collectWarmupProm(w *promtext.Writer) {
	st := warmup.CounterStats()
	if d := warmup.DiskWarmup; d != nil {
		w.Gauge("flodni_warmup_cache_bytes",
			"Approximate size of the warmup cache on disk: a running total, resynced from the disk on each eviction walk, that can drift.",
			promSample(d.TrackedBytes()))
		w.Gauge("flodni_warmup_cache_quota_bytes", "Size limit of the warmup cache.", promSample(st.QuotaBytes))
		heads, tails := d.WarmCounts()
		w.Gauge("flodni_warmup_files",
			"Media files with a warm window on disk now, by window: heads at or above the ready floor, tails fully covered. Read from memory, so it can lag a removal made behind the cache's back.",
			promSample(heads, lbl("window", "head")),
			promSample(tails, lbl("window", "tail")))
	}
	whCounts, whSum := warmup.WarmupDurationSnapshot()
	w.Histogram("flodni_warmup_head_fill_duration_seconds",
		"Time from the first chunk to the last of a head fill, per file whose head completed.",
		promtext.Histogram{Bounds: warmup.WarmupDurationBoundsSeconds, Counts: whCounts[:], Sum: whSum})
	w.Counter("flodni_warmup_evictions_total", "Files removed by quota eviction, by window.",
		promSample(st.EvictedHeads, lbl("window", "head")),
		promSample(st.EvictedTails, lbl("window", "tail")))
	w.Counter("flodni_warmup_evicted_bytes_total", "Bytes (allocated size) removed by quota eviction, by window.",
		promSample(st.EvictedHeadBytes, lbl("window", "head")),
		promSample(st.EvictedTailBytes, lbl("window", "tail")))
	w.Counter("flodni_warmup_unverified_drops_total",
		"Warmup blocks, head or tail, never written because their pieces did not verify in time.",
		promSample(warmup.UnverifiedDrops()))
	w.Counter("flodni_warmup_write_queue_drops_total",
		"Head chunks dropped because the warmup write queue was full.",
		promSample(st.WriteQueueDrops))
	add := addWarmer.Stats()
	w.Counter("flodni_warm_fills_total",
		"Warm fills by trigger and outcome. trigger=mount: per-file records written when reads through the mount completed a file's windows (outcome=warmed when the engine holds a swarm delivery stamp, cached when it does not; needs the state database and index mode). trigger=add: attempts of warm on add (warm_on_add), one per file attempt; trigger=catchup: attempts of catch-up for files already in the library (warm_catchup_per_hour), which never evicts, so a full cache counts no_room; trigger=next: fills of the next episode during a playback (warm_next_episode), which never evicts and does not pause for a playback; every outcome of these triggers is present from the start; a pause, a shutdown and a removal count nothing.",
		append([]promtext.Sample{
			promSample(warmFillsMountWarmed.Load(), lbl("trigger", "mount"), lbl("outcome", "warmed")),
			promSample(warmFillsMountCached.Load(), lbl("trigger", "mount"), lbl("outcome", "cached")),
		}, addWarmFillSamples(add)...)...)
	collectAddWarmProm(w, add)
	collectCatchupProm(w, catchupFiles)
	collectWarmNextProm(w, warmNext)
}

// collectReadabilityProm writes the library by readability. The numbers come from the
// ticker, not from the database, so a scrape stays in memory. No series without the
// state database: nothing is recorded then, and a zero would read as "all fine".
func collectReadabilityProm(w *promtext.Writer) {
	c := readabilityGauge.Load()
	if c == nil {
		return
	}
	w.Gauge("flodni_readability_torrents",
		"Library torrents by readability, refreshed every minute: failed has a failure streak, ok has none and a read served by the swarm, never_read is the rest.",
		promSample(c.ok, lbl("state", "ok")),
		promSample(c.failed, lbl("state", "failed")),
		promSample(c.neverRead, lbl("state", "never_read")))
}

// collectUnstoredProm writes the startup count of library torrents without a stored
// engine record that holds metainfo. No series until the startup walk has counted.
func collectUnstoredProm(w *promtext.Writer) {
	n := libraryUnstored.Load()
	if n == nil {
		return
	}
	w.Gauge("flodni_library_torrents_unstored",
		"Library torrents with no stored engine record holding metainfo, counted once at startup (a library add or an engine add with save_to_db repairs one; not refreshed).",
		promSample(int64(*n)))
}

// libraryUnlinkedFn reads the unlinked-stub count; ok is false while there is none.
type libraryUnlinkedFn struct{ get func() (int, bool) }

// libraryUnlinked is set once the library manager exists.
var libraryUnlinked atomic.Pointer[libraryUnlinkedFn]

// collectUnlinkedProm writes the number of stubs no library symlink points at, as of
// the last list of each kind. No series while there is no linked set (no walk yet, no
// library root configured, or the set is too old) or a kind has not been listed: a zero
// would read as "every stub is linked".
func collectUnlinkedProm(w *promtext.Writer) {
	f := libraryUnlinked.Load()
	if f == nil {
		return
	}
	n, ok := f.get()
	if !ok {
		return
	}
	w.Gauge("flodni_library_unlinked_stubs",
		"Stubs no library symlink points at (linked=false in GET /api/library/list), as of the last list of movies and of tv; absent while there is no linked set.",
		promSample(int64(n)))
}

func collectPlaybackProm(w *promtext.Writer) {
	w.Counter("flodni_playback_starts_total",
		"Playbacks the media server webhook confirmed (not playing to playing).",
		promSample(playbackStarts.Load()))
	marks := make([]promtext.Sample, 0, len(playSources))
	for i, src := range playSources {
		marks = append(marks, promSample(playMarks[i].Load(), lbl("source", src)))
	}
	w.Counter("flodni_played_marks_total",
		"Plays recorded on a file (the played mark that protects it from eviction), by source: webhook (the media server resolved a playback to one file) or manual (POST /api/library/confirm). Both present from 0.",
		marks...)
	w.Counter("flodni_torrent_idle_closes_total",
		"Torrents the engine closed because no reader had them for the idle timeout. confirmed is true when a file of the torrent had a webhook-confirmed playback (the label is refreshed once a second).",
		promSample(keepaliveCounters.idleCloses[1].Load(), lbl("confirmed", "true")),
		promSample(keepaliveCounters.idleCloses[0].Load(), lbl("confirmed", "false")))
	w.Counter("flodni_torrent_reopens_total",
		"Closed torrents a read on an open handle woke again. confirmed as in flodni_torrent_idle_closes_total.",
		promSample(keepaliveCounters.reopens[1].Load(), lbl("confirmed", "true")),
		promSample(keepaliveCounters.reopens[0].Load(), lbl("confirmed", "false")))
	w.Counter("flodni_wake_refused_total",
		"Wakes the engine refused because it keeps no record of the torrent: one per handle at a read, one per open. Not a failed read and not a dead swarm; 0 from start.",
		promSample(wakeRefusedCount.Load()))
	stalls := make([]promtext.Sample, 0, 2*len(stallCauseNames)*numStallOrigins*numStallRoles)
	for _, conf := range []string{"true", "false"} {
		for cause, name := range stallCauseNames {
			for origin, oname := range stallOriginNames {
				for role, rname := range stallRoleNames {
					stalls = append(stalls, promSample(keepaliveCounters.stalls[confirmedIdx(conf == "true")][cause][origin][role].Load(),
						lbl("confirmed", conf), lbl("cause", name), lbl("origin", oname), lbl("role", rname)))
				}
			}
		}
	}
	w.Counter("flodni_read_stalls_total",
		"Served reads that took longer than one second. Failed reads are not counted. confirmed as in flodni_torrent_idle_closes_total. cause is seek when the stall began after a non-sequential jump of the session's read position (the same jump the session seek count uses) and before the first read the read-ahead cache served after it, no more than 15 s after the jump; playback for every other stall. origin is what the engine was doing, checked in this order: evicted when a piece of the read's span completed again while the read ran (it had been evicted from the in-memory piece cache and came twice), pump_behind when a pump exists for the session and its fill offset is at or below the read offset, no_pump when the session has no pump, swarm for everything else (a pump ahead of the read, a slow swarm). A read of a file the session has no file index for is never evicted. role is primary when the handle that served the read was its file's primary (the one the pump follows) when the read finished, second for every other handle, including one on a file with no primary, and for a probe read. Present from 0 for every confirmed, cause, origin and role combination; summed over role it is the count the combination had before role, summed over origin the count the pair had before origin.",
		stalls...)
	w.Counter("flodni_reannounce_total",
		"Tracker re-announces actually sent for a torrent with a confirmed playback whose swarm starved (fewer than 3 connected peers for 60 s with a read stall, a hung read or a short, shrinking buffer), by reason (starved) and private (true for a private torrent, sent only when its tracker's min interval allowed it). Present from 0.",
		promSample(reannounces[1].Load(), lbl("reason", "starved"), lbl("private", "true")),
		promSample(reannounces[0].Load(), lbl("reason", "starved"), lbl("private", "false")))
	early := countEarlyAnnounce(loadedEarlyAnnounce())
	w.Gauge("flodni_private_torrents_early_announce",
		"Loaded private torrents by whether any of their trackers allows an early (starved) re-announce: eligible when a tracker's last successful announce sent a min interval below its interval, ineligible when none did, unknown before a successful announce. Passive: read from regular announces, no extra tracker request.",
		promSample(int64(early[0]), lbl("state", "eligible")),
		promSample(int64(early[1]), lbl("state", "ineligible")),
		promSample(int64(early[2]), lbl("state", "unknown")))
	w.Counter("flodni_keepalive_releases_total",
		"Holds that ended, by reason: stop is a stop webhook (or the playback state dropped), ceiling is playback_keepalive_minutes after the last read or play webhook. The torrent then closes at its next idle check.",
		promSample(keepaliveCounters.releases[0].Load(), lbl("reason", "stop")),
		promSample(keepaliveCounters.releases[1].Load(), lbl("reason", "ceiling")))
	w.Counter("flodni_keepalive_evicted_total",
		"Confirmed torrents that lost their hold to playback_keepalive_max_torrents, counted once per episode; they idle-close as without the keep-alive.",
		promSample(keepaliveCounters.evicted.Load()))
	w.Gauge("flodni_keepalive_held_torrents",
		"Idle torrents the keep-alive is keeping open right now.",
		promSample(int64(keepaliveHeld())))
	w.Counter("flodni_warmup_cold_reopen_total",
		"Confirmed plays that opened with no ready head on a file quota eviction had removed, counted once per eviction. previously_played says whether the file had been played before.",
		promSample(coldReopensPrevPlay.Load(), lbl("previously_played", "true")),
		promSample(coldReopensNotPlayed.Load(), lbl("previously_played", "false")))
}

func collectReaderBudgetProm(w *promtext.Writer) {
	w.Counter("flodni_unconfirmed_read_bytes_total",
		"Bytes past the head and tail windows that readers took from files with no confirmed playback, whichever path served them.",
		promSample(app.unconfirmedReadBytes.Load()))
	w.Counter("flodni_unconfirmed_read_sessions_total",
		"Sessions that crossed the unconfirmed-read margin, counted once at close: confirmed_late if a playback webhook confirmed the file afterwards.",
		promSample(app.unconfirmedSessions[rbUnconfirmed].Load(), lbl("outcome", "unconfirmed")),
		promSample(app.unconfirmedSessions[rbConfirmedLate].Load(), lbl("outcome", "confirmed_late")))
	w.Counter("flodni_unconfirmed_read_refused_reads_total",
		"Reads the reader budget refused with EIO (refuse_unconfirmed_reads on only).",
		promSample(app.unconfirmedRefusedReads.Load()))
	w.Counter("flodni_unconfirmed_read_holds_total",
		"Holds of a refused handle, counted when they end: lifted by a confirmation, or refused when the time ran out.",
		promSample(app.unconfirmedHolds[rbHoldLifted].Load(), lbl("result", "lifted")),
		promSample(app.unconfirmedHolds[rbHoldRefused].Load(), lbl("result", "refused")))
	w.Counter("flodni_unconfirmed_read_confirmed_after_refusal_total",
		"Sessions that had a read refused and were then confirmed by a playback webhook, counted once each (plays the budget broke).",
		promSample(app.unconfirmedConfirmedAfterRefusal.Load()))
	w.Counter("flodni_reader_budget_readahead_overshoot_bytes_total",
		"Bytes the engine was asked to fetch as read-ahead past the head window edge, for a read that started inside the window of a file with no confirmed playback. Asked for, not downloaded. With unconfirmed_readahead_clamp on it stays at 0.",
		promSample(app.readaheadOvershootBytes.Load()))
	w.Counter("flodni_reader_budget_attributed_fill_bytes_total",
		"Bytes the pump and the prefetches filled past the head and tail windows of a file with a session and no confirmed playback. They count toward the margin only with attribute_fills_to_reader on.",
		promSample(app.attributedFillBytes.Load()))
	w.Counter("flodni_playback_webhooks_received_total",
		"Playback start and resume webhooks for a movie or an episode, matched to one file or not.",
		promSample(app.playbackWebhooksMatched.Load(), lbl("matched", "true")),
		promSample(app.playbackWebhooksUnmatched.Load(), lbl("matched", "false")))
	rules := make([]promtext.Sample, 0, numWebhookRules)
	for i, name := range webhookRuleNames {
		rules = append(rules, promSample(app.playbackWebhookRules[i].Load(), lbl("rule", name)))
	}
	w.Counter("flodni_playback_webhook_match_rule_total",
		"Playback start and resume webhooks by the rule that resolved them: basename, path_map, imdb, fuzzy, or none.",
		rules...)
}

func collectReadAheadProm(w *promtext.Writer) {
	total, active, entries := raCache.Stats()
	w.Gauge("flodni_readahead_cache_bytes", "Bytes held in the read-ahead cache.", promSample(total))
	w.Gauge("flodni_readahead_cache_active_bytes", "Bytes in the read-ahead cache that are not stale.", promSample(active))
	w.Gauge("flodni_readahead_cache_entries", "Entries in the read-ahead cache.", promSample(int64(entries)))
	byReason := raCache.EvictionsByReason()
	evictions := make([]promtext.Sample, 0, len(byReason))
	for i, n := range byReason {
		evictions = append(evictions, promSample(n, lbl("reason", readahead.ReasonNames[i])))
	}
	w.Counter("flodni_readahead_evictions_total",
		"Chunks the read-ahead cache dropped on a Put to stay under its budget, by reason: idle (path not read for 120 s), behind (already consumed by the player) or ahead (farthest ahead on the path holding the most bytes). The idle sweep is not counted.",
		evictions...)
	w.Counter("flodni_readahead_evictions_near_player_total",
		"Evictions of a chunk starting within the player's fair share of the budget, for a path whose player is known. The harmful kind: it stays near 0.",
		promSample(raCache.NearPlayerEvictions()))
	if b := gc(); b != nil {
		w.Gauge("flodni_readahead_cache_budget_bytes", "Size budget of the read-ahead cache.", promSample(b.ReadAheadBudget))
	}
	pumps := playingPumpCount()
	w.Gauge("flodni_playing_pumps", "Read-ahead pumps of playing files, the streams that read_ahead_budget_mb is shared among. Scan pumps and pumps of paused or unconfirmed files are not counted.", promSample(pumps))
	var lead int64
	if b := gc(); b != nil {
		lead = streamLeadLimit(b.ReadAheadBudget, pumps, b.ReadAheadBase)
	}
	w.Gauge("flodni_pump_lead_limit_bytes",
		"How far one pump may read ahead of its player: its share of read_ahead_budget_mb (the budget divided by the live pumps, minus a few chunks of slack), for a chunk of read_ahead_base.",
		promSample(lead))
	collectPaceProm(w)
}

// openSessionCount counts the sessions /api/sessions lists as open, without building
// their records.
func openSessionCount() int {
	n := 0
	sessions.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// collectEngineReadsProm writes the engine's short-read counters. They are atomics
// kept per process, so a scrape takes no engine lock.
func collectEngineReadsProm(w *promtext.Writer) {
	stream, fetch := native.ShortReadCounts()
	w.Counter("flodni_engine_short_reads_total",
		"Engine reads that ended short and were reported as success, by path: stream is the sequential reader, fetch the stateless block fetch behind the FUSE fallback.",
		promSample(stream, lbl("path", "stream")),
		promSample(fetch, lbl("path", "fetch")))
	w.Counter("flodni_engine_peer_redials_total",
		"Peers the opt-in redial (redial_dropped_peer) added back to a private torrent after its only connected peer dropped. 0 with the switch off.",
		promSample(torr.RedialCount()))
	tlsFails := tlsverify.Counts()
	w.Counter("flodni_tracker_tls_verify_failures_total",
		"Announces and scrapes to an https tracker that failed certificate verification, by reason: unknown_authority, hostname (the certificate's names do not cover the host), expired (also not yet valid) or other. No host label. 0 from start; see Tracker TLS.",
		promSample(tlsFails["unknown_authority"], lbl("reason", "unknown_authority")),
		promSample(tlsFails["hostname"], lbl("reason", "hostname")),
		promSample(tlsFails["expired"], lbl("reason", "expired")),
		promSample(tlsFails["other"], lbl("reason", "other")))
	setErrs, remErrs, clearErrs := settings.DBWriteErrors()
	w.Counter("flodni_engine_db_write_errors_total",
		"Engine database commits that failed, by operation: set (a record save), rem (a record removal) or clear. The record is then not saved; alert on any increase. 0 from start.",
		promSample(setErrs, lbl("op", "set")),
		promSample(remErrs, lbl("op", "rem")),
		promSample(clearErrs, lbl("op", "clear")))
	pc := torrent.ProcessCounters()
	w.Counter("flodni_engine_hedge_triggers_total",
		"Tail-hedge duplicate requests the engine fired. Counted per process, so unlike hedge_trigger_count in /metrics it does not fall when a torrent expires.",
		promSample(pc.HedgeTriggers))
	w.Counter("flodni_engine_peer_ejects_total",
		"Outlier peers the engine proactively dropped for a low rate against the swarm median, by transport. Per process, not per active torrent.",
		promSample(pc.PeerEjectsTCP, lbl("transport", "tcp")),
		promSample(pc.PeerEjectsUTP, lbl("transport", "utp")))
	w.Counter("flodni_engine_peer_churn_drops_total",
		"Peers dropped by the warmup-probe churn, by transport. Per process, not per active torrent.",
		promSample(pc.PeerChurnsTCP, lbl("transport", "tcp")),
		promSample(pc.PeerChurnsUTP, lbl("transport", "utp")))
	w.Counter("flodni_engine_gradient2_backoff_samples_total",
		"Gradient2 limiter samples that found the peer's latency gradient below 1 and shrank its growth. Stays 0 with the limiter off (the default).",
		promSample(pc.Gradient2Backoffs))
	w.Counter("flodni_readahead_refetched_bytes_total",
		"Bytes the engine fetched again for a piece that had completed and been evicted from the in-memory piece cache. Baseline for deciding whether eviction is costing downloads; nothing acts on it.",
		promSample(torrstor.RefetchedBytes()))
	refetched := torrstor.RefetchedBytesByReader()
	refetchSamples := make([]promtext.Sample, 0, len(refetched))
	for i, n := range refetched {
		refetchSamples = append(refetchSamples, promSample(n, lbl("reader", torrstor.RefetchReaderNames[i])))
	}
	w.Counter("flodni_engine_refetched_bytes_total",
		"flodni_readahead_refetched_bytes_total split by the readers the torrent's cache had open when the bytes were written. The peer that writes a piece is not a reader, so this is not the reader that asked for it: open_ended when any open-ended reader (the pump's stream) was open on that cache at that moment, else bounded when any bounded-range reader was, else none. Present from 0; sums over reader to the unsplit total.",
		refetchSamples...)
	evicted := torrstor.EvictionCounts()
	evictSamples := make([]promtext.Sample, 0, len(evicted)*len(evicted[0]))
	for z, byReaders := range evicted {
		for b, n := range byReaders {
			evictSamples = append(evictSamples, promSample(n, lbl("zone", torrstor.EvictionZoneNames[z]), lbl("readers", torrstor.EvictionReaderBuckets[b])))
		}
	}
	w.Counter("flodni_engine_cache_evictions_total",
		"Completed pieces the in-memory piece cache evicted, by where the piece lay against the readers open on that cache at that moment and by how many were open (0, 1, 2+). zone is behind (the piece lies wholly before every open reader's current offset), ahead_unread (the piece lies at or after some open-ended reader's current offset; unread means at or after the reader's current offset, not that the reader never read it) or no_reader (no reader open, or the piece lies outside every reader in any other way, for example after a bounded-range reader only). The zone is measured from the reader offsets as the eviction scan saw them. Present from 0 for every combination.",
		evictSamples...)
	ends := torr.StreamEndCounts()
	endSamples := make([]promtext.Sample, 0, len(ends))
	for i, n := range ends {
		endSamples = append(endSamples, promSample(n, lbl("reason", torr.StreamEndNames[i])))
	}
	w.Counter("flodni_engine_stream_ends_total",
		"Open-ended engine streams (the pump's, a GET with no closed byte range) that ended, by reason: eof (served to the end of the file), timeout (the stream timeout ran out), read_error (the engine could not read) or cancel (the client went away or cancelled). Bounded-range fetches are not counted. Present from 0.",
		endSamples...)
	w.Gauge("flodni_readahead_stranded_partial_bytes",
		"Bytes held by partially written pieces outside every reader's protected window, summed over open torrents. Such a piece is never evicted, so after a seek these stay until that piece is fetched to the end. Sampled at scrape under each cache's reader read lock only, never the engine's client lock.",
		promSample(torrstor.StrandedPartialBytes()))
	repaired, unfilled := native.ShortReadRepairCounts()
	w.Counter("flodni_engine_short_read_repairs_total",
		"Outcome of the stream reader's refill loop, per read that needed it: repaired after a reconnect, or unfilled and left a gap.",
		promSample(repaired, lbl("result", "repaired")),
		promSample(unfilled, lbl("result", "unfilled")))
}

// collectProcessProm writes the Go runtime and process gauges under the names the
// standard Go and process collectors use, without importing a client library. The
// process start time is flodni's own series above. ReadMemStats stops the world for
// a moment; a scrape every few seconds does not notice.
func collectProcessProm(w *promtext.Writer) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	w.Gauge("go_goroutines", "Number of goroutines that currently exist.",
		promSample(int64(runtime.NumGoroutine())))
	w.Gauge("go_memstats_heap_inuse_bytes", "Bytes in in-use heap spans.", promSample(int64(m.HeapInuse)))
	w.Gauge("go_memstats_sys_bytes", "Bytes obtained from the system by the Go runtime.", promSample(int64(m.Sys)))
	w.Counter("go_gc_cycles_total", "Completed garbage collection cycles.", promSample(int64(m.NumGC)))
	if n, ok := openFDs(); ok {
		w.Gauge("process_open_fds", "Number of open file descriptors.", promSample(int64(n)))
	}
}

// openFDs counts /proc/self/fd, minus the descriptor the listing itself holds. It
// reports false where there is no /proc (anything but Linux), and the series is then
// left out.
func openFDs() (int, bool) {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil || len(ents) == 0 {
		return 0, false
	}
	return len(ents) - 1, true
}

// collectAuthRefusalsProm writes flodni_auth_refusals_total{route,scope,reason}. The
// labels are bounded (apiauth.Refusals): a pattern from the route table, "unmatched"
// or "pre_route"; the scope the route needs; a fixed reason. Series appear once seen.
func collectAuthRefusalsProm(w *promtext.Writer) {
	snap := authRefusals.Snapshot()
	samples := make([]promtext.Sample, 0, len(snap))
	for _, s := range snap {
		samples = append(samples, promtext.Sample{
			Labels: []promtext.Label{lbl("route", s.Route), lbl("scope", s.Scope), lbl("reason", s.Reason)},
			Value:  float64(s.Count),
		})
	}
	w.Counter("flodni_auth_refusals_total",
		"Requests the auth gates refused, by route pattern, the scope the route needs and the reason. Counts the Host, Origin and cross-site refusals in every mode.",
		samples...)
}
