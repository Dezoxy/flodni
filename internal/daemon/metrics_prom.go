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
	"flodni/internal/gostorm/torr"
	"flodni/internal/promtext"
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
		"Open to the first read at 8 MiB or later or in the tail, per session classed as real playback..",
		promHistogram(&ttffStats.openToDeepRead))
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

	collectWarmupProm(w)
	collectPlaybackProm(w)
	collectReaderBudgetProm(w)
	collectWebhookHealthProm(w)
	collectReadabilityProm(w)
	collectUnstoredProm(w)
	collectUnlinkedProm(w)
	collectSwarmProm(w)
	collectScrapeProm(w)
	collectReadAheadProm(w)
	collectStubIndexProm(w)

	w.Counter("flodni_fuse_short_reads_total",
		"Reads served with fewer bytes than requested away from EOF.",
		promSample(fuseShortReadCount.Load()))
	w.Counter("flodni_fuse_short_read_results_total",
		"Outcome of the refill after a short read.",
		promSample(fuseShortReadRepaired.Load(), lbl("result", "repaired")),
		promSample(fuseShortReadFailed.Load(), lbl("result", "failed")))
	w.Counter("flodni_fetch_dedup_total",
		"Concurrent fetches of the same chunk that shared one fetch.",
		promSample(fetchFlightDedupCount.Load()))
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
	w.Counter("flodni_read_stalls_total",
		"Served reads that took longer than one second. Failed reads are not counted. confirmed as in flodni_torrent_idle_closes_total.",
		promSample(keepaliveCounters.stalls[1].Load(), lbl("confirmed", "true")),
		promSample(keepaliveCounters.stalls[0].Load(), lbl("confirmed", "false")))
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
	w.Counter("flodni_readahead_evictions_total",
		"Chunks the read-ahead cache dropped on a Put to stay under its budget. The stale sweep is not counted.",
		promSample(raCache.Evictions()))
	if b := gc(); b != nil {
		w.Gauge("flodni_readahead_cache_budget_bytes", "Size budget of the read-ahead cache.", promSample(b.ReadAheadBudget))
	}
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
