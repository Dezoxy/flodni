# Roadmap

What is being worked on, in order. Each step is one pull request, merged only
when every required check passes. [ideas.md](ideas.md) holds what is not
planned yet; it has been empty since 2026-11-14.

## Trim to the streaming core

Flodni keeps what streams a torrent through the FUSE mount: the virtual file
system and read pump, the embedded GoStorm engine, the library add and remove
API, the index cache, the playback webhook, the control panel and the
streaming metrics. Everything else is removed, starting with what runs without
a switch or calls out to third parties.

1. **Stop calling home and close open endpoints.** [FINISHED] Remove telemetry
   (on by default, posts to the upstream project's endpoint), the update check
   (polls the upstream repository), the MCP server (unauthenticated, always on)
   and the `net/http/pprof` import (unauthenticated debug endpoints).
2. **Remove the dashboard.** [FINISHED] `/dashboard` and its API routes, the
   collector (polls every 5 s, looks up the public IP, broadcasts on the LAN)
   and the macOS toolbar app that wraps it.
3. **Remove dead and switched-off code.** [FINISHED] NAT-PMP (and `iptables` in
   the image), the IP blocklist, index seeding, the Samba watchdog,
   `internal/preload`, `internal/syncer/statestore`, `Config.EngineScripts` and
   the configuration keys nothing reads.
4. **Remove upstream packaging.** [FINISHED] `docker-windows/`, `install.sh`,
   `tiramisu.service`. The container image is the only supported install.
5. **End-to-end streaming test.** [FINISHED] A local tracker seeds a small
   torrent, Flodni mounts it and the test reads the file back and compares
   hashes. It must pass before steps 7 and 8, which touch the read path.
6. **Keep private torrents private.** [FINISHED] A library add without a magnet
   merges public UDP trackers into every release, private or not, and GoStorm's
   default retracker mode appends public trackers to every torrent; a DHT
   announce may also start before the private flag is known. Fix all three and
   pin them with unit tests at those seams. The end-to-end test's private
   fixture then guards that streaming still works.
7. **Remove catalogue sync.** [FINISHED] The scheduler, the movie, TV and
   watchlist engines, the TMDB, Torrentio and Rotten Tomatoes clients, the
   built-in Prowlarr client, their control panel cards and `Config.LogDir`,
   which only the engines' log files use.
8. **Remove music and audiobooks.** [FINISHED] `internal/musicimport`,
   `cmd/musicimport`, `internal/audio` and the audio parts of
   `internal/library`, `internal/metadb`, `internal/daemon/startup.go` and
   `internal/daemon/main.go`, then
   `internal/prowlarr`, `Config.Prowlarr` and `music_discovery`, which only the
   music import still uses.

## Own identity and must-fix bugs

1. **Rename to Flodni.** [FINISHED] Go module, binary, image default paths,
   `FLODNI_*` environment variables with the old names kept as aliases.
   Persisted file names stay, so existing state carries over.
2. **Keep working stubs when a pack replaces them.** [FINISHED] Replacing a pack
   deletes stubs that played before the new release is known to be readable
   (`internal/library/manager.go`). A pack still wins, but the old stubs go only
   once the new ones are confirmed readable.

3. **Atomic writes and bounded servers.** [FINISHED] Stub and config writes go
   through a temporary file, sync and rename; the control HTTP server gets read,
   write and idle timeouts.

Releases and deployment are scheduled separately.

## Maintainability

1. **Clear the dependency backlog.** [FINISHED] Merge or supersede the open
   Dependabot pull requests, align the vendored engine's `go.mod` with patched
   versions, and run the engine's private-torrent tests in CI.
2. **End-to-end coverage for replacement.** [FINISHED] A second fixture filed
   over the first as the same TV episode, so the readability check before a
   replacement runs against the real engine.
3. **Break up `main.go`.** [FINISHED] Phase a: split it into files by concern in
   the same package, as a pure move. Phase b: move the read-ahead cache into its
   own package with unit tests, and give the HTTP routes their own mux. The
   `App` struct that replaces the shared package-level maps waits for tests on
   the read path (Hardening step 2); the move is Ideas round, step 21.
4. **Make failures visible.** [FINISHED] Phase a: triage every unchecked error,
   fix the ones that hide failures, and gate new ones with errcheck. Phase b:
   report sessions where a file was opened but never served bytes through the
   API. Structured logging with `slog` is Ideas round, step 20.

## Hardening

1. **Opt-in token on the control API.** [FINISHED] `api_token` or
   `FLODNI_API_TOKEN` enables bearer-token auth on every control route except
   the static panel page; webhooks may pass the token as a query parameter.
   Unset keeps today's open API.
2. **Unit tests on the read path.** [FINISHED] Cover the read decision tree in
   `internal/daemon/handle.go` and the pump's decision helpers, the
   prerequisite for the `App` struct.
3. **Stubs land where the mount serves them.** [FINISHED] The library writes
   stubs under the configured `physical_source_path` while FUSE serves the path
   given on the command line; make them one value, or refuse to start when they
   differ.
4. **Bound and protect GoStorm's server.** [FINISHED] Read-header and idle
   timeouts on port 8090, and the same opt-in token, since it exposes torrent
   control and shutdown.

## Reliability

1. **A health endpoint.** [FINISHED] `GET /api/health` reports the mount, the
   engine and the state database, each with a status and a reason, and the
   container health check uses it instead of `mountpoint` alone.
2. **A shutdown deadline.** [FINISHED] Shutdown saves state and unmounts within
   a bounded time and exits even when the unmount hangs, so a restart cannot
   block forever.
3. **Reads against slow seeders.** [FINISHED] Reproduce in the end-to-end test
   the stalls seen against a seeder with a shallow request pipeline, then fix
   the leecher's request timeouts so a sequential read completes.
4. **Characterization tests for the library.** [FINISHED] Pin stub naming, stub
   contents and one-stub-per-episode filing for movies, episodes and season
   packs, so later restructuring cannot change them unnoticed.
5. **An interface for the native bridge.** [FINISHED] The read path calls the
   engine through an interface instead of the concrete client, and tests cover
   the fetch, prefetch and pump branches that only the end-to-end test reaches
   today. This is the last prerequisite for the `App` struct.
6. **No stall after a tail probe.** [FINISHED] A read that follows a tail
   read stalled for about 34 s, and in some runs about 58 s, in the
   end-to-end test. Both causes were in the cache and are fixed (see the
   Done list); what the engine still leaves open is Ideas round,
   step 25.
7. **Say when no play webhook arrives.** [FINISHED] Three features act only
   on a playback the media server's webhook confirmed: played-file
   protection, the reader budget's refusal and the keep-alive below.
   Without a webhook configured and reachable they silently do nothing.
   Flodni logs at start whether a play webhook has matched and which of the
   built features are active, and warns when readers play through the mount
   but no play webhook has matched for `playback_webhook_warn_days`: a log
   line, `warnings` in `/api/health` (advisory, never a status change) and
   `flodni_warning{name="no_play_webhook"}`. The README's "Play webhook"
   section says plainly that these features need the webhook. The keep-alive
   is not part of this step; it is step 8.
8. **Keep a playing torrent connected.** [FINISHED] The engine closes a
   torrent that has had no reader for its idle timeout
   (`TorrentDisconnectTimeout`, 15 s by default), which keeps idle titles
   cheap: they neither leech nor seed. A media server reads in bursts, and a
   direct play or a throttled transcoder can wait longer than that between
   reads. The torrent then closes and the next read reopens it, with a new
   announce and a new search for peers; on a swarm of a few seeders each
   reopening costs seconds. A play webhook used to mark the torrent as not to
   expire, but the mark sat on the engine's torrent object: it was set only
   if the torrent was loaded when the webhook arrived, and a reopened torrent
   came back without it. Phase a counts the engine's idle closes, the
   reopens of a closed torrent for a read, and the reads slower than one
   second, each split by whether a file of the torrent had a
   webhook-confirmed playback, and adds them to the session record. Phase b
   is built (2026-11-13): while a file has a confirmed playback its torrent is
   not closed for idleness. The hold is decided at the moment of expiry from
   the playback state, so it survives a reopening, and it replaces the old
   mark for the idle timeout. A stop webhook ends it at once; otherwise it
   ends 15 minutes after the last read or play or resume webhook
   (`playback_keepalive_minutes`, the one off switch: 0 is off), and a pause
   does not extend it. At most 4 torrents are held at once
   (`playback_keepalive_max_torrents`; the cap times the engine's
   per-torrent cache must stay well under the memory limit), and a torrent
   over the cap falls back to the plain idle close. Releases are counted by
   reason and losses to the cap are counted. It is on by default, and the
   effective settings are logged at start next to the webhook status of step
   7. A reader with no confirmed playback, a scan, warm on add and catch-up
   keep the plain idle close, and a held torrent gets no tracker or peer
   source it would not have had. Tested by unit tests and the end-to-end
   `keep-alive` variant; the 15 minute ceiling is unit-tested only.

## Readability evidence

Flodni records facts about bytes: did a file read, is it cached, who reads
it. Which release to replace, and when, is decided by tools outside it.
These steps do not touch the read path and do not wait for Reliability
step 6.

1. **Record the decision.** [FINISHED] An ADR: Flodni fills and protects its own
   cache and records whether its files read; choosing and replacing releases
   stays outside. It weighs an external reader through the mount, warming in
   process and warming on the media server's first scan, and names what outside
   tools may rely on. Everything in it is labelled designed until the step that
   builds it lands.
2. **A readability record over the API.** [FINISHED] The state database already
   counts sessions that ended with a failed read and nothing served, per
   torrent, but no endpoint reads it back. Add the time of the last read the
   swarm served and the reason and source of the last failure, keep the rule
   that only bytes from the swarm clear it, and serve it read-only at `GET
   /api/torrents/health` with the full hash, so outside tools stop parsing logs
   and stub names. Rows of torrents that left the library are pruned at startup,
   the endpoint answers 503 when the state database is off, and the README
   documents the fields as the supported contract.
3. **One log line per closed session.** [FINISHED] A JSON line with the hash,
   file index, path, outcome, bytes served by serving path, whether the swarm
   served a read, time to first byte and duration, so a log store keeps what the
   200-entry ring behind `/api/sessions` forgets. The existing `[DeadSwarm]` and
   `[TTFF]` lines stay as they are.
4. **Scrapeable metrics.** [FINISHED] Prometheus text exposition on its own
   path, next to the JSON the panel reads: the time-to-first-byte histograms,
   dead-swarm verdicts, warmup cache hits by source, cache bytes and evictions.
   Later steps add their own series in their own pull requests.

## Readiness and protection

A file should start from the SSD the first time it is opened, and a reader
that is not playback should not pull a whole file through the swarm. These
steps build on index mode (`warmup_dir` set, `pump_on_open` off), where
only hash-checked data reaches the cache and tail coverage survives a
restart. Steps 2 and 3 waited on Reliability step 6, which is now
finished.

1. **Honest cache state and eviction.** [FINISHED] Phase a: the cache
   remembers per file when a playback confirmed by the media server's
   webhook last played it, and whether the quota evicted it; the state
   database keeps both across restarts. A scanner's read does not mark a
   file played, and one webhook marks at most one file (an episode only when
   series title, season and episode all match; with several equally good
   candidates, none). A play protects its file for 90 days from the last play
   (`played_protection_days`); after that it ranks as never played. Eviction
   takes a file's head and tail together: files never played first, oldest
   written first, then played files, oldest first. It removes bytes only;
   the readability record stays. Each eviction is logged, and two counters
   report how many plays started on a file the quota had evicted, out of how
   many plays. Phase b: one internal call lists which files have their head
   and tail windows on disk now, without a `stat` per file. It is not
   served; it feeds a gauge of warm files by window, next to the cache
   bytes, quota and evictions. A gauge of torrents by readability (ok,
   failed, never read) is built on the readability record. Evicting the
   least recently read file is built opt-in as Ideas round, step 22, which
   waits for those counters to decide whether to switch it on.
2. **Warm on add.** [FINISHED] After a library add returns, a background
   queue with a small fixed worker pool fills the head and tail windows of
   each new file through the verified path, straight from the engine and not
   through the mount. At a full quota it may evict only files never played,
   oldest first, and it checks for room before it fetches; with none left,
   or with no evidence of plays (state database off, or no play ever
   recorded), it drops the new file and counts it. It is off by default
   (`warm_on_add`), pauses while a playback session is open and records each
   outcome per file in the readability record; only pieces the swarm
   delivered count as success. The end-to-end test (`warm-on-add`) adds a
   fixture with the switch on and reads nothing through the mount, waits for
   the fill, checks the metric, the `[Warm]` line, the per-file row and the
   torrent record, stops the seeder and reads both windows back from the
   SSD against the seeder's hashes. Pause, wake of a closed torrent and
   removal while queued rest on unit tests here; the `warm-lifecycle`
   variant (Ideas round, step 10) covers them end to end.
3. **Catch-up and retries.** [FINISHED] A pass one pace interval after
   start, then every 6 hours, feeds the library files the warmer has never
   warmed to it, at a configurable pace, so a bulk import is worked off
   across restarts without a persistent queue. Catch-up never evicts: at
   a full quota it stops and counts what it left. A file the cache has
   evicted is not warmed again; it is left to its next play. A failed warm
   is retried no sooner than most of a day later and a capped number of
   times; after that the record keeps the evidence and the warmer leaves the
   file alone. The end-to-end test (`catch-up`) adds a fixture with the switch
   off and sees no catch-up, turns it on and restarts the daemon, waits for the
   pass to warm the title with no read through the mount, restarts again and
   sees a pass with nothing to do, then stops the seeder and reads both windows
   back from the SSD. The quota stop, the exclusion of evicted files, the
   overwrite of a stale eviction mark and the 20-hour rule rest on unit tests.
4. **A byte budget for readers that are not playback.** [FINISHED] Phase a is
   built: Flodni counts the bytes a file serves, from the engine or the
   read-ahead cache, to a session with no confirmed playback, past its two
   windows, and logs each session once when it crosses a margin
   (`unconfirmed_read_margin_mb`); it measures only and refuses nothing.
   Phase b is built, opt-in and off by default (`refuse_unconfirmed_reads`):
   past the budget it stops fetching from the swarm for such a reader and
   serves only what is cached; a refused read waits 15 s for a webhook, then
   fails with EIO, and the pump is parked, not cancelled. It is tested against
   a fake engine and end to end (`budget-refusal`, below); the pump park is
   fake-engine only. The budget lifts as soon as a session registers, applies
   only once a matched playback webhook has been seen since start
   (`flodni_playback_webhooks_received_total{matched="true"}`), and a refusal
   never counts as a failed read in the readability record. Entry condition
   for phase b: the end-to-end test sends a play webhook in the middle of an
   unconfirmed read that has crossed the margin, and shows the read
   completes, `flodni_unconfirmed_read_sessions_total{outcome="confirmed_late"}`
   counts it and `flodni_playback_webhooks_received_total{matched="true"}`
   counts the webhook. That condition is met: the end-to-end variant
   `late-confirm` does exactly that. The end-to-end variant `budget-refusal`
   runs the refusal against the real mount: a scan is cut with EIO at the
   windows plus the margin (20 to 32 MiB of a 150 MiB file), with one
   `refused` line and no failed read, the seeder is asked for nothing more,
   a late play lifts the hold, and a stop gives the next reader a fresh
   margin; while a reader is held, other opens and reads of the file answer
   in under 1 s. The pump park rests on fake-engine tests only. The step is
   finished; the switch stays off by default, and the owner turns it on after
   reading `late_ms` and `matched="false"` on the host.

## Before a deployment relies on it

These came out of reviews of the removal path, the cache directory and the
file mapping. Steps 1 to 4 should land before a deployment depends on
Flodni; steps 5 to 7 and 9 can land at any time. Step 8 is owner-ordered
ahead of the move to a deployment. The steps marked [FINISHED] are built, the
others are designed, not built.

1. **One wake path, and removals that stay removed.** [FINISHED] The mount's
   open and its read-time rewake used to wake a closed torrent from a bare
   magnet without checking that the engine still stores it; only warm on
   add's `WakeStored` checked. A handle left open across a removal could then
   re-add the hash on its next read, with no stored metainfo, so a private
   torrent got DHT and PEX until its metainfo arrived (hard rule 3), and a
   removed torrent could be stored again. Built (unit-tested; tests that need
   the engine or the mount run on Linux only): unlinking one episode of a pack
   through the mount follows the library's reference check (#138); a library add
   is saved at once and the startup counts the stubs the engine does not store
   (#136); every wake goes through one path, `torr.LoadStored`, that adds a
   torrent from the engine's stored record alone and refuses a hash with no
   record or no metainfo (a refusal answers `EIO`, is counted in
   `flodni_wake_refused_total` and recorded as the reason `not_stored`, never as
   a dead swarm); and a load, a save and a removal are serialised in the engine,
   so a removal racing a wake leaves the torrent neither loaded nor stored.
   Explicit adds (`add`, upload) stay adds; `GET /stream` adds only with a
   token (Durable records step 10). The end-to-end
   variant `removal` runs it against the real mount and engine: four movies are
   filed, one is read in the background, a descriptor is open on a second, and
   two are removed through the library API back to back and the third with `rm`
   through the mount. The read title keeps its sha256, the engine answers 404
   for the three at once and again after its idle timeout, the read on the open
   descriptor at an uncached offset fails with `EIO` and is counted as a
   refused wake (not a failed read, no dead-swarm verdict, reason `not_stored`),
   and no index-cache file of the three remains.
2. **A missing cache disk means no cache.** [FINISHED] With `warmup_dir` on
   a separately mounted disk that is absent at start, Flodni used to create
   the directory on the filesystem underneath and fill it to the quota, and
   to re-create it at runtime if it vanished. Now it warns when `warmup_dir`
   is set but the cache did not start, never re-creates the directory after
   start, does not count a failed deletion as an eviction (and keeps the old
   total when the quota walk cannot read the directory), serves reads from a
   read-only directory without writing, has a fill check the directory before
   it fetches (a distinct error, counted as write_failed), and warns when
   `warmup_dir` shares a filesystem with its parent. A missing
   `.dense-migrated` next to a populated cache no longer drops every head:
   each head is checked for holes and only those go. The startup inode prune
   has the same "the walk was whole" guard as the other prunes. The README
   names the deployment side (the "Cache disk" section). A disk that is
   absent at start still gets a cache on the filesystem underneath, with at
   most a WARNING; keeping it off that filesystem is the deployment's job
   (README, "Cache disk").
3. **Never serve another file's bytes.** [FINISHED] An open trusted the
   stub's file index when a warm head existed under it, and otherwise
   resolved the file by size and name, so a stub whose index was stale could
   be served from the wrong file. Flodni's own stubs resolve to themselves; a
   stale index needs a stub from elsewhere or a rare tie in the file order.
   `flodni_stub_index_mismatch_total` counts the paths whose resolved index
   differs from the stub's, once each. The warmer (warm on add and catch-up)
   refuses a file whose engine length differs from the stub's size
   (outcome `index_mismatch`, not retried, no swarm evidence). With the
   torrent in the engine, or its saved record (read without a wake) lists the
   files, a warm head is used only when the file at the stub's index has the
   stub's length; otherwise the open resolves by size and name against that
   list, and fails with `EIO` when no file has the stub's length. With no
   list, the head is trusted only if it is no longer than the stub's file.
   File ids come from the engine's own list, and hashes are lower-cased. One
   hole stays; `GET /api/library/audit` (Ideas round, step 8) finds its stubs.
4. **Align the tail window to a page.** [FINISHED] The kernel reads whole
   4 KiB pages, so a read of exactly the last N MiB of a file whose size is
   not page-aligned starts a page before an N MiB tail window, and that page
   came from the engine instead of the cache. The end-to-end test measured
   12 KiB so. The tail window's start is rounded down to a page boundary
   (`warmup.TailStart`, used by the writers, the readers, the fills and the
   reader budget), so the window grows by up to 4095 bytes. Covers written
   before (exactly the configured size from the end) still load and serve
   their own window as complete; they lack the leading page, which comes from
   the engine as before.
5. **End-to-end variants as parallel jobs.** [FINISHED] CI runs each variant
   on its own runner, so wall time stays near the longest variant as variants
   are added, and variants can no longer collide on container names.
6. **Free cache space on request.** [FINISHED] The quota is read once at
   start, and a lower quota evicts only when a new head is written.
   `POST /api/warmup/evict` evicts down to a given size now, in the normal
   order: never-played files first, oldest first, and played files only after
   them.
7. **Remove by the full hash.** [FINISHED] Removal by hash matched stub names
   on eight hex digits; stubs from older tools carry the first or the last
   eight, so some were missed, and a stub of another torrent could match. It
   matches the full hash in the stub's own link instead, as do the
   already-present check of an add and the reference check before a torrent is
   dropped.
8. **Catch-up scope: retries only.** [FINISHED] `warm_catchup_scope` is
   `"all"` (the default: every library file not warm, evicted or failing) or
   `"retries"`. With warm on add on and no backlog to walk, a file warm on add
   timed out on, dropped for quota, failed to wake or lost to a restart was
   never tried again. With `"retries"`, catch-up feeds only files warm on add
   attempted and did not finish: a failed read (the same 20 hour wait and 4
   failed attempts), `dropped_quota`, `dropped_no_evidence`, `lost`,
   `write_failed`, and a file warm on add queued over an hour ago with no
   outcome recorded (a restart mid-warm, a wake error, a full queue). The
   queued time is `file_state.queued_at` (schema 16), written when warm on add
   enqueues and cleared when any attempt records an outcome. Files never
   attempted are left alone. The evidence rules do not change: a retried read
   the swarm served, failed or not, counts as today; `dropped_quota`,
   `write_failed`, `lost` and a refused wake never count as swarm evidence and
   never touch the readability or the per-torrent streak.
9. **Match webhooks through library symlinks.** [FINISHED] The media
   server reports the path of its own library, and a library of Sonarr or
   Radarr named symlinks into the mount never shares a base name with the
   stub, so those webhooks fell to the IMDb id or a fuzzy title guess. The
   opt-in `plex_path_map` maps a media server path prefix to a path in the
   container; when no file name matched, the longest prefix is applied, the
   path is read with one `lstat` and `readlink`, and the target's base name is
   matched exactly. The same name picks the file a play marks played and a
   stop releases. A missing path or a regular file changes nothing. New:
   `flodni_playback_webhook_match_rule_total{rule}`. Unit-tested against
   symlinks in a temporary folder; not run against a media server.

## Ideas round (after v0.1.0)

The owner moved every entry of [ideas.md](ideas.md) here on 2026-11-14.
Waves 1 to 3 are built in that order; steps 22 to 25 are gated and were built
opt-in and off by default, so the measurement they name decides whether to
switch them on. Steps 1 to 19 and 22 to 26 are merged; steps 20 (`slog`) and 21
(`App` struct) are in flight, not merged (hard rule 5).

**Wave 1**

1. **Config file permissions.** [FINISHED] `POST /api/config` writes
    `config.json` with mode 0644, though the file holds `api_token` and
    `plex.token`. Keep the existing file's mode on a rewrite and use 0600 when
    the file is created. The README recommends `FLODNI_API_TOKEN` and
    `FLODNI_PLEX_TOKEN` for secrets. Built (#169).
2. **Prune guards.** [FINISHED] The periodic inode prune in
    `internal/daemon/cleanup.go` prunes ghost inodes after its own walk, without
    the "the walk was whole" guard the start-up prune has; the walk adds the
    root path itself, so an empty or unmounted library still leaves a non-empty
    set. Give it the same check. At start-up, a stub that parses but has a size
    outside the valid range is counted and logged separately and no longer
    blocks the prunes, while a real walk error (I/O, permission) still does.
    Built (#167).
3. **Keep verdicts across a shutdown.** [FINISHED] Sessions still open at
    shutdown never reach a verdict; only the release path and the cleanup loop
    close them, so a stop within two minutes of a session loses its evidence and
    its `[Session]` log line is never written. Close them in the shutdown path.
    Built (#170).
4. **Warmup accounting drift sources, and quota by allocated blocks.**
    [FINISHED] The head write and removal race is closed, and
    `GetAvailableRange` no longer stores a stale-low size over a writer's newer
    one. `totalSize` can still drift: the eviction walk stores an absolute total
    outside `tailMu`; `dropResidue` unlinks and subtracts without `tailMu`; a
    CORRUPT CACHE removal never subtracts; and the head create's `handles.Store`
    should be a `Swap` that closes the old descriptor, or it leaks one.
    `flodni_warmup_cache_bytes` exports this total as approximate. Separately,
    the quota counts apparent file sizes, so sparse files make it count more
    than the disk holds; count allocated blocks. Built (#174).
5. **More Prometheus series** [FINISHED] on `/metrics/prometheus`. Now: short
    reads (`native.ShortReadCounts`, `ShortReadRepairCounts`, already
    per-process atomic counters with no engine lock, so two families and their
    README rows); Go runtime and process gauges (goroutines, memory,
    descriptors) and a gauge of open sessions, in the hand-written encoder in
    `internal/promtext` (a private `client_golang` registry would add a
    dependency); and `promtool check metrics` in the end-to-end test, which
    needs a new image pin in the `Makefile`. Later parts of this step, each for
    its own reason: engine and peer series (hedge, eject, churn, the gradient
    loop), because the JSON `/metrics` sums them over active torrents under
    engine locks and the sums fall when a torrent expires, which a counter must
    not do, so they need per-process counters in the engine first; read-ahead
    evictions, which need a counter inside `Put` under the shard lock; the
    head-fill duration histogram (`warmupDurationBuckets`), which has no sum and
    cannot be a Prometheus histogram without a new counter; and sub-millisecond
    precision, since the histograms keep whole milliseconds and cache hits add
    nothing to `_sum` (it means storing microseconds). All built: the Now parts
    in #166, the later parts afterwards. Engine and peer series are per-process
    atomics in the engine (`process-counters.go`, bumped where each event
    happens) exported as counters; read-ahead evictions count inside `Put`; the
    head-fill histogram has its sum; the histograms keep microseconds and export
    seconds with the same bucket bounds.
6. **Two flaky tests under full-suite load.** [FINISHED] `internal/warmup`
    `TestHookFiresOnceFromWriteChunkAndWriteTail` (a race) and `internal/daemon`
    `TestClosedTorrentStoredLengthDisagreeingMakesTheHeadSuspect` (a timeout
    waiting for the head write). Find the cause of each and fix the test or the
    code. Fixed (#171 and #184): the warmup test was a test-only race; the
    daemon test exposed a real race in the warmup read path.
7. **Docs and CI.** [FINISHED] A threat model for the control API and its opt-in
    token. CodeQL, with a manual cgo build step because the FUSE build may
    defeat the automatic one. Stale comments: `Open` in
    `internal/daemon/fuse_nodes.go` says it needs head and tail ready, but its
    warm path uses `headReady` only and `tailReady` feeds the `[TTFF]` log line;
    comments in other packages name `main.go` or `webhook_status.go` where the
    code has moved. The settings page hints about a Media Server card, sessions
    and posters that no code provides. Built (#165).
8. **Close the stale-stub hole.** [FINISHED] An open of a stub whose index is
    stale, on a torrent the engine has closed and whose record has no file list
    (saved before the metadata), still takes the warm head under that index when
    it is no longer than the stub's file; a sibling's head of a plausible length
    passes. The audit that closes it walks every stub's `link=` and `index=`
    against the file list in the engine's record and reports each stub whose
    index names a file of another length than the stub's size, for a library
    remove and add to repair; records with no list need one wake to be read.
    Built (#172).
9. **Dead packs: a pack-level give-up.** [FINISHED] After the head file of a
    pack gives up (4 failed attempts), the next file becomes the head and gets
    its own four attempts, so a dead pack costs one wake a day for days per
    file. Bounded, and it announces only to the stored trackers; a pack-level
    give-up ends it. Built (#168).
10. **Warm on add in the end-to-end test.** [FINISHED] The `warm-on-add` variant
    files one title. A canary public tracker for the wake of a closed private
    torrent, and a removal while a title is queued or paused, need a second
    title held open; the seeder has no such mode. Add one. Built (#173).

**Wave 2**

11. **Count a metainfo that never arrives.** [FINISHED] A torrent whose metainfo
    times out fails the open with EIO before any session exists, and `Wake`
    never condemns it, so the readability record shows nothing for it. Give it a
    failure reason of its own; weigh the retries of a player counting several
    times for one occasion. Built (#176).
12. **Count reads after a session closes.** [FINISHED] A session closes after 60
    s idle (30 s once it is older than 30 minutes), or when a probe releases
    while a player without a pump slot has read nothing. Reads on a handle that
    is still open then find no session and are counted nowhere, so a paused
    playback is cut short in `/api/sessions` and the `[Session]` line. Open a
    session on the first read after a close; weigh a new session per pause
    against one long one. Built (#180).
13. **Stop replaced torrents leaving an entry behind.** [FINISHED] A session
    still open on a replaced torrent writes its readability entry back after the
    library cleared it. Close the session where the stub is invalidated
    (`invalidateSyncRemovedPath`), before the library clears the row, to fix
    replacement; clear the row in the FUSE unlink path to cover deleted stubs.
    If `movies` and `tv` can be separate mounts, the start-up prune also needs a
    per-directory non-empty guard. Built (#178).
14. **Library naming and filing quirks.** [FINISHED] Pinned as they are by the
    characterization tests (`internal/library/*_char_test.go`), so each change
    is a deliberate one. Built (#183); only new adds change, existing stubs
    are never renamed or re-keyed:
    - A pack file named `S00E01` (a special) is filed under the requested
      season as that season's episode 1 and competes with the real one.
    - Samples are dropped only when a larger file names the same episode; a
      lone sample is filed as the episode.
    - A year in the title is read as the release year when no date is given
      (`Blade_Runner_2049_2049_...`, `Dune_2021_2021_...`); a title with no
      ASCII letters starts with `_`.
    - `EpisodeKey` drops non-ASCII letters (an accented leading letter is
      lost), so two shows can share a key.
15. **Per-file replacement check for packs.** [FINISHED] The check before a
    replacement reads one file of the release; a pack whose other episodes do
    not read passes. Weigh the extra reads against the risk. Built (#179).
16. **A swarm signal from the engine.** [FINISHED] For awake torrents: peers
    connected, time of the last piece received, observed rate; what the swarm
    does, as opposed to what a tracker reports. It covers only torrents that are
    awake. Built (#177).
17. **Less container privilege.** [FINISHED] The image runs with `SYS_ADMIN` and
    AppArmor unconfined for the FUSE mount; a narrower profile may be enough.
    Built as an opt-in profile and a guide (#175), not verified on a host.
18. **Warm again after eviction.** [FINISHED] The catch-up leaves evicted files
    to their next play; warming them again needs a quota that fits the library,
    or a rule for which files deserve it. Built (#182).
19. **A proper file-lifecycle API.** [FINISHED] Add, replace with a policy,
    confirm and remove, as explicit operations instead of side effects. Built
    (#181 and #189): ADR 3 is Accepted.

**Wave 3 (cross-cutting, last)**

20. **Structured logging with `slog`.** [FINISHED] 222 log call sites use
    `log.Printf`. Moving them to `slog` with fields helps log search, but each
    line needs its own key choices; it pays when logs ship to a central store.
    Built: part a, the machine-readable startup report (#185), and 20b, the
    `slog` migration (#193).
21. **An `App` struct instead of package-level state.** [FINISHED] The FUSE
    handle, the pump, the webhook and the cleanup manager share seven
    package-level maps (`activeHandles`, `activePumps`, `playbackRegistry`,
    `pumpTimers`, `priorityTimers`, `inFlightFetches`, `inFlightPrefetches`).
    The move was gated on unit tests of the read path; those now cover the
    decision helpers, the cache hit, short-read repair, `Release`, and the
    fetch, prefetch and pump branches through a fake engine behind the
    `engineBridge` interface, so the prerequisites are met. Built: those seven
    maps, the control token, the startup reporter and all nine reader-budget
    counters are fields of `App` (the last four moved in #212). The `App` is
    created once in `Run()`; tests install a fresh one with `newTestApp`.

**Gated on measurement (built opt-in, off by default)**

22. **Evict the least recently read file.** [FINISHED] Eviction takes files
    never played first, then the oldest written. Whether read recency would keep
    more starts warm is measured first: `flodni_warmup_cold_reopen_total` with
    `previously_played="true"` over `flodni_playback_starts_total`. Build it if
    that share reaches 5% of plays over a few weeks at a full quota. The
    `previously_played="false"` series is a different problem: warm data evicted
    before its first play points at the quota or the eviction order, not at
    recency. Built opt-in, off by default (#187).
23. **Warm the next episode during playback.** [FINISHED] While episode N plays,
    fill episode N+1's windows so a binge starts at once. A second reader on the
    playing torrent splits the engine's read-ahead, so this needs warm on add in
    place and a measurement that the playing stream does not slow down. Built
    opt-in, off by default (#187).
24. **Reader budget follow-ups.** [FINISHED] Measure the read-ahead overshoot
    first: a miss inside a window is never refused, and its read-ahead can
    extend past the window edge into the middle the budget is meant to protect;
    then decide whether to clamp it. Attribute pump and prefetch fills to the
    reader that caused them, after Reliability step 6. Built opt-in, off by
    default (#190).
25. **The two engine-side stall gaps.** [FINISHED] Both stalls after a tail
    probe are fixed in the cache (see the Done lines below); two gaps stay open.
    An order-only change wakes no idle peer: in the engine a priority or
    request-order change that leaves the pending set as it was triggers no
    request update. The stale cache priorities exposed it; an engine-side wake
    removed the 34 s stall too (0 of 11 runs) but was not shipped, since fixes
    belong in Flodni's own code. Another producer of an order-only change could
    expose it again. An earlier note that waking peers did not help came from an
    attempt that was never committed; unverified. A dropped only peer waits for
    the next announce: the engine floors a tracker's announce interval at one
    minute (`internal/anacrolix-torrent/tracker_scraper.go`) and does not keep a
    dropped peer to redial, so a private torrent with one seeder and one tracker
    that loses its peer waits up to a minute. Not covered: a cache over capacity
    with nothing evictable (everything sits in a reader window or a file guard)
    can still reach "have all". To measure, reapply the instrumentation: `git
    apply docs/ideas/stall-dump.patch` ([patch](ideas/stall-dump.patch)) on
    `main` adds a dump at the first fetch timeout; set `FLODNI_STALL_DUMP=<n>`
    for the dump and `FLODNI_STALL_POKE=1` for the wake experiment (`tests/e2e`
    passes them through as `E2E_STALL_DUMP` and `E2E_STALL_POKE`). Built opt-in,
    off by default (#190).
26. **Tracker scrape in `/api/torrents/health`.** [FINISHED] Built (#186): a
    periodic and on-demand scrape of each stored torrent's own trackers (BEP 48
    over HTTP, BEP 15 over UDP) in `internal/scrape`, one worker per tracker
    host with its own pacing and backoff, reported per torrent as `scrape` plus
    a top-level `scrape_pass`, `POST /api/torrents/scrape` and `GET
    /api/torrents/scrape/status`, settings `tracker_scrape_hours`,
    `tracker_scrape_batch` and `tracker_scrape_pace_ms`, and four metrics. A
    private torrent is asked at its own trackers only; no log, field or metric
    holds an announce URL. Experimental in the README until a consumer runs
    against it; tested against local fake trackers only.

The vendored engine stays as it is apart from fixes, and advisories for
`anacrolix/torrent` are watched.

## After v0.2.0

Requests from running v0.2.0 on a real host.

1. **Pick the linked file when a webhook matches several.** [FINISHED] Built
    (#199). Plex Media Server's
    own webhook names no file, so a play is matched by IMDb id, the library-type
    bootstrap or a fuzzy title. In a library of Sonarr or Radarr symlinks into
    the mount, several stubs can share one IMDb id: a release an upgrade
    replaced stays as its own stub, and no symlink points at it any more. The
    `imdb` rule can then confirm a play, protect a file and hold a torrent for
    the wrong stub. The owner keeps Plex as the sender and gives Flodni no Plex
    token. Design: the targets `plex_path_map` already declares are the library
    roots. A background walker, never the webhook handler, reads them with
    `lstat` and `readlink` only (no following, no file reads) and builds the set
    of stub base names some library symlink points at. It rebuilds every few
    minutes, and early when a webhook finds several candidates. When a rule
    yields more than one candidate, only the linked ones are kept; exactly one
    left is the match, counted as `rule="linked"`. With no candidate linked, or
    no roots set, matching is as today. A test holds two stubs sharing an IMDb
    id, one linked: the linked one wins, for a play and for a stop. Not urgent:
    an IMDb match is exact while one stub holds the id.

## Durable records (after v0.2.2)

From a review after v0.2.2 and from running it. Steps 1 to 3 were must-fix:
data loss and an open-port gap. Steps 1 to 20 are built; 21 and 22 are
designed, not built.

1. **Make the engine database durable.** [FINISHED] The engine's `config.db` is
    opened with `NoSync` and mode 0666, and is neither synced nor closed at
    shutdown. Since the strict wake it holds the only copy of every torrent's
    metainfo, so an OS crash or power loss that corrupts it leaves every stub
    answering EIO until each torrent is re-added. Open it with fsync and mode
    0600, sync and close it at shutdown, and measure wake latency during a
    library scan before and after. If the fsync shows, keep `NoSync` and write a
    periodic hot snapshot (`tx.WriteTo` to a temporary file, fsync, rename)
    instead. Document a restore. A test kills the process mid-save and reopens
    the file. Built (#202): fsync on every commit (two fdatasyncs), mode 0600
    with a wider file tightened at start, a clean close at shutdown, the read
    cache holding committed data only, and an unchanged record not rewritten;
    #213 made a failed commit reach its caller.
2. **Remove the engine's `wipe` action.** [FINISHED] `POST /torrents` with
    `"action":"wipe"` removes every stored torrent in one request, and nothing
    calls it. Delete the case and the function; a test pins that the action is
    refused. Built (#204); `GET /shutdown` and the play webhook became POST-only
    in the same change.
3. **Refuse cross-origin requests on an open port.** [FINISHED] With no token
    set, both servers pass every request, the engine allows all origins and
    private-network CORS, and the JSON handlers do not check `Content-Type`, so
    a page in a browser on the network can post to the library and engine
    routes. With no token set, refuse a request whose `Origin` does not match
    its `Host` (a request without `Origin`, such as a media server's webhook or
    a script, is unchanged), and drop the permissive CORS. The threat model
    gains the browser actor. Tests pin the refusal and the unchanged cases.
    Built (#204): the check compares host names only, because a TLS-terminating
    tunnel keeps the original Host; #209 added the Host allowlist (step 11).
4. **Keep tracker passkeys out of stubs.** [FINISHED] Each stub stores the
    request's magnet or the `.torrent`'s announce list, passkeys included, at
    mode 0644, and nothing reads the field. A new stub carries the bare info
    hash only. An opt-in scrub, run by the owner, rewrites existing stubs and
    restores each file's mtime, so the media server sees no change. Confirm
    first that no external tool reads the field. Built (#207), with `flodni
    scrub-stubs`: it sets the times and owner on its own temporary file before
    the rename, re-checks the stub just before it, and refuses root without
    `--allow-root`.
5. **Tell "engine not ready" from "not stored".** [FINISHED] A wake before the
    engine is up is reported as not stored, recorded as `not_stored` and latched
    by the warmer, which can tell an operator to re-add a healthy torrent. It
    gets its own error that records and latches nothing, and the fixed start-up
    sleep becomes a bounded wait for readiness. The start-up warning that says
    an unstored torrent is re-added from its bare magnet is corrected. Built
    (#206).
6. **Keep a torrent when a stub could not be read.** [FINISHED] The check
    whether a torrent is still used skips a stub it cannot read, so removing one
    episode can drop the record its sibling episodes still need. An unreadable
    stub keeps the torrent; a test injects the read error. Built (#203).
7. **Report what nothing links to.** [FINISHED] The library list says whether a
    library symlink points at each stub (true, false, or null before the first
    walk), with a count gauge, and the audit lists engine records no stub uses.
    Report only: what to remove stays the deployment's decision. The new fields
    are additive. Built (#203).
8. **One fuzzy matcher for play and stop.** [FINISHED] The webhook handler
    carries the same fuzzy pass twice and logs every registered path at INFO on
    each play. Extract one tie-finding function with table tests, log the
    registry at DEBUG, and cache a webhook's IMDb id only on an exact match.
    Built (#205).
9. **Evict to a low-water mark, and fuzz the bencode decoder.** [FINISHED] At
    quota, eviction frees exactly enough, so every new head walks the cache
    directory again; evict a little under the quota instead. The hand-written
    bencode decoder that parses tracker responses gets a fuzz target seeded from
    its test vectors. Built (#208): eviction goes down to 95% of the quota, the
    headroom from never-played files only.
10. **`GET /stream` never adds from a link.** [FINISHED] With no token, a GET or
    HEAD `/stream` refuses a link whose hash the engine does not hold, any link
    without an inline hash (`http(s)`, `file:`), and `&save` or `&preload`, so a
    page cannot make Flodni add, fetch or download. A stored hash wakes from its
    record in both modes, so a link's trackers never reach it; `/play` takes a
    strict hash and never fetches. Built (#210).
11. **An optional Host allowlist against DNS rebinding.** [FINISHED]
    `allowed_hosts` (empty means off) refuses a request whose Host is not
    listed, on both servers; IP literals, `localhost`, the machine's name and
    the engine URL's host always pass; an all-invalid or mistyped list keeps the
    check on. With no token, `Sec-Fetch-Site: cross-site` is refused except on
    the open routes, and the API cannot change the effective list. Built (#209).
12. **Keep tracker passkeys out of logs.** [FINISHED] One redactor keeps only
    scheme and host of every URL in a log line, including URLs inside errors,
    and the engine's own logger is routed through it, so the vendored code is
    untouched; the media server token is redacted at the source of its request
    errors. Built (#211).
13. **A stored private torrent's own trackers win over an add's (hard rule 3).**
    [FINISHED] An add of a hash the engine stores or holds no longer merges the
    request's trackers into a record or a live torrent that is not known public;
    a record without metainfo counts as private, and changing trackers is
    remove, then add. Built (#214).
14. **Audit and remove foreign trackers in stored records.** [FINISHED] `flodni
    trackers audit` lists, per stored record, the tracker hosts outside its
    primary tracker's domain (hosts and hashes only, never a URL), and `flodni
    trackers remove-host` removes one named host, refusing a record's primary;
    both run with the daemon stopped. Built (#215).
15. **Keep the media server token out of the API.** [FINISHED] `GET /api/config`
    blanks the Plex token, an empty post keeps the file's, an environment token
    is never written to the file, a changed `plex.url` needs the token typed
    again, and an open port cannot change either. Built (#216).
16. **A failed engine-database commit reaches its caller.** [FINISHED] A failed
    save no longer stamps the debounce, the read cache never holds an
    uncommitted value through the router, and
    `flodni_engine_db_write_errors_total{op}` counts failures. Built (#213).
17. **Make the `App` test isolation enforced.** [FINISHED] The last four
    counters moved onto `App`, and a test that leaves pumps, prefetches or
    timers registered on its `App` fails. Built (#212).
18. **Harden the fetches an add makes.** [FINISHED] `POST /torrents` refuses
    `file:` links, caps an `http(s)` body at 10 MiB, follows at most 3
    redirects and only on the host it started at, and the poster check has one
    10 s deadline for its HEAD and GET. Built (#235).
19. **Answer an add once its record exists.** [FINISHED] With `save_to_db`,
    `POST /torrents` answers only after the record is committed: 504 when no
    metadata arrived, 500 when the save failed. This covers `POST /torrents`
    only: `POST /torrent/upload` with `save` still saves in the background and
    answers 200 first. The library add maps the engine's 504 to its own 504 and
    its `metadata_wait` bounds the engine add. Built (#235).
20. **A string-aware comment stripper for `config.json`.** [FINISHED] The `//`
    stripper was line-based, so a hand-edited line holding two URLs was cut at
    the second; panel saves write one value per line and were unaffected.
    Built (#225).
21. **An opt-in private-address block for add fetches.** A config option,
    default off, that refuses a first URL resolving to a loopback, private or
    link-local address, and checks the dialled IP so DNS rebinding does not get
    around it. It is opt-in because a link to a LAN indexer is a legitimate
    use. Designed, not built.
22. **A body cap on `/torrent/upload`.** A `MaxBytesReader` on the upload route,
    with a handler test that drives the real upload and add handlers together.
    Designed, not built.

## Authentication (v0.3.0)

Scoped tokens, a hashed token file and a panel sign-in that needs no token in
the browser. Design: [ADR 0004](architecture/decisions/0004-scoped-authentication.md).
With no token configured, behaviour stays as v0.2.6, except that a
`config.json` that exists but cannot be read now stops the start. All steps are
built in v0.3.0; ADR 0004 is Accepted and implemented.

1. **One route-to-scope table, a registrar, and no silent open mode.** Every
   route on both servers gets a scope in one table (unknown routes and methods
   mean `admin`), the control mux is built through a registrar that records
   its patterns, `GET /api/live` is added, and an unreadable or unparsable
   `config.json` refuses to start (only `main` exits; a save over it is 409).
   A test fails on any route without a scope, and a hand-written golden file
   catches a silent change. Enforcement came with step 3. Done, #225.
2. **The token file, its loader and `flodni auth new-token`.** JSON with names,
   scopes and SHA-256 hashes, read once at start and refused if anything is
   wrong (never open mode; `auth_jwt` without a file, a non-loopback
   `gostorm_url`, `api_token` together with a tokens file, and duplicate or
   reserved entries refuse too); the CLI prints
   a token once and the entry to add; the `[Startup] report` gains an additive
   `auth` section. Done, #224.
3. **Enforce scopes on both ports.** Hash lookup in constant time with the
   token shape checked first, deny by default, `?token=` on the webhook routes
   for webhook-only tokens, the engine's body `action`
   read after authentication with a handler cross-check, `/stream` adds only
   for `admin`, auth settings startup-only, Flodni's own engine calls through
   an in-process `RoundTripper` with no credential, the library add and
   inspect accepting only an anchored `magnet:` hash before any engine call,
   `api_token` plus a tokens file refusing to start, and
   `flodni_auth_refusals_total{route,scope,reason}`. The scope matrix runs on
   both ports. Done, #227.
4. **Keep tokens out of every log.** A `token=` rule in the redactor, the
   engine's request logger routed through it, the engine's recovery handler
   replaced (it dumps headers and cookies), refusal paths quoted, and a test
   that no log, metric or refusal line holds a token. Done, #222.
5. **Verify a proxy-signed identity for the panel.** Optional `auth_jwt`: RS256
   and ES256 only, issuer, audience and expiry checked, keys from the provider's
   JWKS with a strict parser, a cache, one refresh in flight and a fail-closed
   outage, an optional email list. Standard-library verifier. Done, #223.
6. **A panel sign-in cookie.** `POST /control/login` with an `admin` token sets
   an HttpOnly, SameSite=Strict, HMAC-signed session cookie; logout revokes
   every session; the panel stops keeping a token in local storage and stops
   sending an empty bearer; cookie and proxy-header requests need a same-origin
   `Origin` (host and port), `Sec-Fetch-Site` and a JSON content type. Done, #226.
7. **Documentation and the threat model.** README, `SECURITY.md`, the changed
   threat-model rows and four new ones, the upgrade notes (the config-parse
   alarm, the stricter keys), a gitleaks rule for the token shape (it arrived
   with step 2), the panel's `frame-ancestors` header, and an end-to-end
   scoped start on both ports. A deployment then moves from `api_token` to the tokens file
   in one restart, each caller on its own token. Done, #228.

## Engine hardening from upstream (after v0.3.0)

From a review of upstream Tiramisu v1.11.9 to v1.11.11 (eleven commits) against
Flodni's diverged tree. Each step says whether Flodni still has the code path
upstream fixed; that is a read of the code, not a reproduction. Port with
upstream's tests where they exist, test first. Steps 1 to 7 are built.

Order. The step numbers stay stable because other changes refer to them; the
order of work follows what a deployment gains. Step 1 first (a peer port
reachable from the internet), then step 6 (a media server's analysis during
playback). Steps 5 and 3 next, but measured first (see their counters). Then
step 4, then step 2 (low priority where libraries reach the mount through
symlinks from a real filesystem; kept for correctness), and step 7 last.

1. **Refuse malformed input from peers and trackers.** [FINISHED] A peer can
   send a negative metadata piece index, which panics on the request table; a
   chunk is not checked against the request it answers, so a misaligned one is
   written past the piece; `validateInfo` has no bounds on piece or file
   lengths; a tracker response is read without a size cap (cap it at 4 MiB); a
   bad webseed URL panics instead of failing the request; every `Port` message
   sends a DHT ping to a port the peer picks (ping on the first only); and the
   client fails to start where IPv6 is unavailable instead of skipping it. One
   PR, with the `-std=gnu17` cgo flag in CI so the build survives the runner's
   move to a C23-default compiler. Flodni has every one of these paths. Built
   (#234).
2. **Real directory times.** [FINISHED] `fillAttrFromStat` gives every directory
   `time.Now()`, so a library scanner sees every folder modified on each stat
   and re-walks everything. Take the time from the physical directory. A test
   shows a stub add changes the directory time and nothing else does. Built
   (#232).
3. **Keep the whole readahead safe from eviction.** [FINISHED] The protected
   window still uses the old percentage split, so pieces between the two bands
   are fetched with deadlines and then evicted (about 11% re-download with 4 to
   8 MB pieces). Port upstream's `protectedWindow` and its test. Add a counter
   of bytes fetched twice for the same piece, to measure before and after. Built
   (#240).
4. **Write a stub only when it changes; save on wake only without metadata.**
   [FINISHED] An identical stub rewrite still bumps its time and makes the media
   server re-analyse it: compare before the atomic write. A wake still snapshots
   the torrent to the database under the client lock during playback: save only
   while its metadata is missing. Built (#232).
5. **Free partial pieces a seek abandons.** [FINISHED] Seek-heavy playback
   strands partially written in-memory pieces until eviction of complete ones
   reaches them (upstream measured 40 seeks holding 480 MB in a 128 MB cache).
   Port `ForgetPartialPiece` with its guards (wanted, hashing, queued, pending
   writes, outstanding requests) and a sweep at most every 5 s, after step 3,
   once the diverged `piece` fields it needs are confirmed. Add a metric of
   partial-piece bytes held after a seek, alertable; measure before porting.
   Built (#241).
6. **Keep probe reads from moving the pump.** [FINISHED] A media server's probe
   and analysis reads on a file someone is watching move the pump and take the
   primary stream. First the two small safety parts: an idempotent handle
   release and interrupting the pump reader when the request context ends. The
   header-read and takeover logic only behind a simulator seam that drives the
   handle and pump code without the engine; Flodni's pump has diverged and
   resume positions depend on it. Built (#237).
7. **Measure first, then decide.** [FINISHED] Request refill (a candidate cap,
   stealing from stalled holders, half-queue refill) touches the same functions
   as Flodni's adaptive pipeline, so it is a performance project with a
   benchmark on a real swarm. Mount tuning (1 MB kernel readahead, longer
   attribute and entry TTLs with kernel invalidation, splice off) needs
   privileges and changes the live mount. Flodni never writes sysfs: it logs the
   mount's BDI id once at start and exposes `read_ahead_kb` as the gauge
   `flodni_fuse_read_ahead_bytes` (observe only, built), so the host can tune it
   and the effect can be seen. The TTLs and splice come only as config options
   with defaults unchanged: the ADR is
   [0005](architecture/decisions/0005-fuse-timeouts-splice-readahead.md); the
   options exist (`attr_timeout_seconds`, `entry_timeout_seconds`,
   `fuse_disable_splice`, startup-only, a timeout outside 0 to 60 s refuses the
   start), defaults unchanged. A TTL above 1 s warns at start until kernel
   dentry invalidation is built. Raising them is for the deployment to A/B with
   numbers. Built (#233, #238, #239).

Not needed: the cold-open wake grace (`StartWake` exists), disabling WebTorrent
(already off), and the TV-sync duplicate-name fix (no syncer in this tree).

## Ideas from upstream v1.11.12 to v1.11.15 (after v0.3.5)

From a review of upstream Tiramisu v1.11.12 to v1.11.15 against Flodni's
tree, and from a field report: a private-tracker season pack whose only
connected seeder dropped mid-play left 0 peers for about 8 minutes, and
episode starts whose middle was not cached stalled 10 to 12 times per
session, up to 20 s each, with no failed reads. The order puts the
measurement before the change it is meant to measure. Steps 1 to 4 are built.

1. **Panel security headers.** [FINISHED] The panel sets only `frame-ancestors
   'none'` and `X-Frame-Options: DENY`. Add `X-Content-Type-Options: nosniff`
   and a `Referrer-Policy` on the panel and the control API's responses, and
   `Strict-Transport-Security` only when the panel is known to be https (TLS,
   `X-Forwarded-Proto`, or `auth_panel_https`). Upstream 53aa761. Built (#244).
2. **Tell the end of a file apart from an early pump exit.** [FINISHED] The V306
   check warns whenever a pump exits during confirmed playback, including when
   it simply read the file to the end; upstream found most of its warnings were
   that. Record a reached-end-of-file flag and log the end of the file as
   information, keeping the warning for a real early exit. Upstream c32cd25.
   Built (#246).
3. **Show how far ahead of the player data is ready.** [FINISHED] During a stall
   the operator cannot tell how much of the file past the player's position is
   already cached or verified, or where the download frontier is. Add both to
   each open session in `GET /api/sessions` and as metrics: bytes contiguously
   available ahead of the last read offset (cache plus verified pieces) and the
   download frontier offset. Bounded labels only. This is also how steps 4 and
   the request-refill switch get measured. Inspired by upstream 7eb7000 (a piece
   map in its UI); no UI here. Built (#247).
4. **A connection boost at cold start, opt-in.** [FINISHED] On a player's first
   read past the file header (never a scan, a probe or warm-on-add), when the
   per-torrent connection limit is below 35, raise it to 35 until 1 MiB of
   useful data has arrived or 30 s have passed, then restore the configured
   limit. It helps swarms with more peers than the limit and does little for
   small private swarms, so it ships behind a switch, default off, measured with
   step 3. A test pins the raise and the restore, including a torrent closed
   during the boost. Upstream 2b0df97. Built (#248).

Not needed: upstream's season-pack name parsing (Flodni's), its shield and
sign-in changes (Flodni has both, broader), and the commits for parts Flodni
removed (UI, themes, macOS, music).

## Where it hurts (after v2026.11.1)

From two days of production metrics on one host and a code review of the
read path. CPU is not the problem (0.06 cores on average). What hurts:

- Confirmed playback stalls, about 70 a day. They cluster in the busy
  evening hours, when the engine re-downloads pieces it had completed and
  evicted (`flodni_readahead_refetched_bytes_total`: up to 27 GiB in one
  hour, 20 to 160 MiB per pump stream reconnect).
- Heap peaks of 5 to 8 GiB against about 3 GiB of configured caches.
- Slow starts cannot be judged yet: about 98 % of the sessions in
  `flodni_session_first_deep_read_seconds` are scans or analysis, and the
  histogram stops at 10 s.

The mechanism behind the refetch is in the code (designed, not yet
measured): each open reader shrinks every reader's protected window at once
(`capacity / readers`) while the engine's read-ahead is corrected only on the
next tick, eviction takes the oldest `Accessed` first so unread read-ahead
goes before pieces just read, and a pump reconnect or a per-read probe stream
briefly leaves the window unprotected. Measurement comes before each change.

1. **Say why a piece was evicted, a stream ended and a read stalled.**
   [FINISHED] No behaviour change. Add `flodni_engine_cache_evictions_total{zone,readers}`
   (zone `behind`, `ahead_unread` or `no_reader`; readers `0`, `1`, `2+`),
   `flodni_engine_stream_ends_total{reason}` (`eof`, `timeout`, `read_error`,
   `cancel`), split the refetched bytes by an open-ended or bounded reader, add
   an `origin` label to `flodni_read_stalls_total` (`evicted`: a piece in the
   read span had completed before; `pump_behind`; `no_pump`; `swarm`), and
   give `flodni_session_first_deep_read_seconds` a `confirmed` label, buckets
   at 30, 60 and 120 s, and a sibling histogram of the time actually blocked
   in reads before the first deep read. Read-only atomics, no engine lock.
   Built (#275): `evicted` means a piece in the read's span completed a second
   time since the read began.
2. **Keep a playing stream's unread read-ahead in the engine cache.** [FINISHED]
   Count only open-ended readers in the protected-window divisor (a bounded
   reader protects its own range), evict by class before age (behind a reader or
   unrelated first, an open-ended reader's unread read-ahead last), and keep a
   closed reader's window for about 2 s so a reconnect inherits it. An ordering
   preference, not hard protection, so a full cache can still evict; RAM only;
   lock order unchanged. A test evicts while a second reader attaches. Judged on
   step 1: `zone="ahead_unread"` near 0 and refetch under 5 % of pump fill.
   Built (#277), with review fixes: bounded readers share only the 15 % slack, a
   parked reader takes no share, a bounded reader's engine read-ahead is clamped
   to its range, a new reader's first read ends the grace (so a seek frees the
   old window at once), and the cleaner tick drops priorities a grace kept on a
   silent swarm.
3. **End a pump stream on idleness, not age.** [FINISHED] The open-ended
   stream's timeout counts from the stream's start; make it reset on progress so
   a long play does not reconnect (and drop its window) on a timer. Extends the
   idle-close tests; no tracker or wake change. Judged on
   `flodni_engine_stream_ends_total{reason="timeout"}`. Built (#276): one timer
   per open-ended GET, reset on every engine read that returned bytes; bounded
   ranges and HEAD keep the absolute deadline.
4. **Count what the engine cache really holds.** A partial piece allocates a
   whole piece buffer on its first chunk but counts only written bytes toward
   capacity, so a probe storm leaves many partial pieces outside eviction until
   the stranded sweep. Count a partial piece at its full length, cap the
   read-ahead cache's idle buffer pool at a quarter of its budget, and add a
   gauge of allocated piece-buffer bytes. Judged on the heap peak. The
   deployment side (a lower `GOGC`, a `GOMEMLIMIT` near the real need) goes in
   the README's tuning notes, not in code.

Needs the owner's decision, not ordered yet:

- A second reader on a playing file (another viewer, or a probe) gets one
  engine stream per FUSE read today. Is a second viewer first-class, with its
  own lead, or kept as a probe with one reused reader? The next section
  covers a second reader that is not a viewer (subtitle extraction); a
  second real viewer stays open here.
- Warm the first 16 MiB instead of 4 MiB for next episodes and recent adds,
  if step 1 shows confirmed starts waiting past the warm head. Costs warmup
  quota.
- A smaller engine cache for an unconfirmed open (scans and probes).

## A second reader on a playing file (after v2026.11.3)

From production evidence on one host, 2026-11-18 to 2026-11-22 (v2026.11.1
to v2026.11.3). Designed, not built; measurement comes before each change.

- About half of the confirmed stalls under v2026.11.3 (159 of 324 in about
  30 watch-hours) came from six sessions that read the whole file in 3 to 16
  minutes while the viewer was a few minutes in. The reader is the media
  server's subtitle extraction: for a direct play with an embedded text
  subtitle selected on a client that cannot read it from the stream, Plex runs
  its transcoder with `-map 0:<subtitle> -codec copy -f segment
  -segment_format srt`, which demuxes the whole container as fast as reads
  return. Through Flodni that is the whole torrent at link rate (up to about
  480 Mbit/s), so every stream on the host stalls (`origin` `pump_behind` and
  `swarm`) and TailHedge trips its "saturated link" breaker.
- The extractor opens a new handle every few seconds, each further into the
  file. `[V284]` promotes each one to primary after 3 sequential reads, so
  the pump follows the extractor, not the player.
- Those jumps arrive on fresh handles, so `cause` labels their stalls
  `playback`, not `seek`.
- `origin="evicted"` is still 21 % of confirmed stalls under v2026.11.3 (62 of
  300), although step 2 of the previous section aimed at it.
- `flodni_playing_pumps` changed meaning with v2026.11.3's pump lifetime, so
  stalls per watch-hour can only be compared across releases with an outside
  source of watch time.

1. **Tell the player from a second reader on the same file.** Choose the
   primary by which handle tracks the player (`player_off`, read rate near
   the file's average bitrate), not by 3 sequential reads, so a reader far
   ahead of the player never takes the pump. Tests, each a scenario: a
   transcode where the transcoder is the player and reads ahead of real time;
   a seek past the extractor, whose restarted handle must not become primary;
   two viewers on one file, both first-class; a file with no confirmed
   position yet.
2. **Pace a second reader that runs ahead of the player.** Move it to the
   `bulk` fill class and cap it at a multiple of the file's average bitrate
   (size over duration; about 4x), so a subtitle job still finishes well ahead
   of the viewer without filling the link. Never pace the primary. A test
   holds the extractor ahead of a player reading at the bitrate. Judged on
   full-file sessions: other streams' `pump_behind` and `swarm` stalls during
   one fall to their usual rate.
3. **Count a jump on a new handle as a seek** when the file already has a
   primary: such a stall within 15 s of the open is `cause="seek"`. Extends
   the stall-cause tests.
4. **Count watch time.** A `flodni_confirmed_playback_seconds_total` counter
   (seconds in which a confirmed session read), so stalls per watch-hour can
   be compared across releases without an outside source.
5. **Find what still evicts ahead of the player.** Break the `evicted` stalls
   since v2026.11.3 down by `flodni_engine_cache_evictions_total` zone and
   readers before changing eviction again.

## Ideas from upstream v1.11.16 to v1.13.1 (after v2026.11.3)

From a review of upstream Tiramisu's release notes v1.11.16 to v1.13.1
(2026-11-17 to 2026-11-22) against Flodni's tree. Not ordered against the
previous section yet; each step first checks what Flodni already does. Designed,
not built.

1. **Keep the inode map when the source folder is empty or partly mounted.**
   `InodeMap.saveToDB` (`internal/vfs/inodemap.go`) deletes every row before
   it rewrites the snapshot, with no guard against an empty or much smaller
   map, so a start against an unmounted or half-mounted volume can renumber
   every file. Refuse the rewrite when the map shrinks past a threshold, log
   it and keep the stored rows. A test saves an empty map over a full one.
   Upstream v1.12.0.
2. **Pause other torrents' speculative reads when a playback risks a stall.**
   Warm fills, catch-up and next-episode warming yield while a confirmed
   session's buffered-ahead is below a floor, and resume when it recovers.
   Complements step 2 of the previous section, which paces a second reader on
   the same file. Judged on `pump_behind` stalls while warm work runs.
   Upstream v1.13.1.
3. **Warm cache eviction by last use, never under an open handle; keep
   complete tails across restarts.** Check Flodni's warm cache against both
   first; build only what is missing. Upstream v1.13.1.
4. **Release a torrent only when nothing references it.** Removing one
   episode of a season pack must not drop the pack while another stub or
   record still cites it. Check Flodni's removal path; add the guard and a
   test if it is missing. Upstream v1.12.01.
5. **Check the native stream path for error responses read as file data.**
   Upstream v1.12.03 turned non-2xx stream responses into read errors.
   Flodni's native path appears to make no HTTP calls; confirm, and close the
   step if so.

Not taken: HTTPS on the panel port, MCP tokens, peer GeoIP, the UI changes
and the move of settings into SQLite. Flodni has its own records and scoped
auth.

Needs investigation, not ordered: **is it worth moving to Tentacle?** Upstream
v1.13.0 replaces GoStorm with Tentacle, a native engine built around FUSE, and
removes the `:8090` API. Its notes claim, from a simulator, a 2.4x faster
cached-read p95 and no reads stuck over 15 s on multi-file torrents (2 to 6 a
run before), with sustained download on par. Before any decision, answer:
how much of Tentacle is new code versus a reshaped GoStorm; whether it keeps
the vendored anacrolix engine and Flodni's private-torrent guarantees (hard
rule 3); what Flodni's own fixes since the fork would have to be re-done; how
its read path and eviction compare with Flodni's on the evidence above; and
whether a port is a cherry-pick, a rebase or a rewrite. The answer is an ADR,
not code.

## Done

- 2026-11-11, trim step 1: telemetry, the update check, the MCP server and
  the pprof endpoints are removed.
- 2026-11-11, trim step 2: the dashboard, its collector and LAN discovery,
  and the macOS toolbar app are removed. Done before the dead-code step
  because the collector read NAT-PMP and blocklist state.
- 2026-11-11, trim step 3: NAT-PMP and `iptables` in the image, the IP
  blocklist, index seeding, the Samba watchdog, the never-filled preload
  cache, `syncer/statestore`, `EngineScripts` and seven unread
  configuration keys are removed.
- 2026-11-11, trim step 4: the Windows installer, `install.sh` and the
  systemd unit are removed; the container image is the only install.
- 2026-11-11, trim step 5: `make e2e` and the CI image job seed a private
  torrent locally and read it back through FUSE in two configurations.
- 2026-11-11, trim step 6: private torrents get no public trackers from the
  library, GoStorm's retrackers or a stored spec on wake, and the engine
  knows a torrent is private before its DHT announcer can run.
- 2026-11-11, trim step 7: the scheduler, the movie, TV and watchlist
  engines, the TMDB, Torrentio and Rotten Tomatoes clients, the sync caches,
  their control panel cards and `Config.LogDir` are removed. The Prowlarr
  client and `prowlarr` config key stay for `cmd/musicimport` until step 8;
  the control panel no longer exposes them. The GoStorm client moved into
  `internal/library`.
- 2026-11-11, trim step 8: music and audiobooks (import, cue splitting,
  audio library API, projections and their FUSE paths), the Prowlarr client
  and the music-only configuration are removed. Existing `music/` and
  `audiobooks/` folders stay on disk; the mount lists them as directories
  without their audio files. Trim to the streaming core is complete.
- 2026-11-11, own identity step 1: the module, binary, image paths and
  environment variables are named Flodni; old names still work.
- 2026-11-11, own identity step 2: a release that replaces existing stubs
  must deliver data first; if it does not, the existing files stay.
- 2026-11-11, own identity step 3: stub and config writes are atomic, and
  the control server has read, write and idle timeouts.
- 2026-11-11, maintainability step 1: the vendored engine's `go.mod` is
  aligned with patched versions (gRPC, OpenTelemetry, x/crypto, websocket),
  its private-torrent tests run in `make go-test` and CI, and the Dependabot
  backlog is merged or superseded.
- 2026-11-11, maintainability step 2: the end-to-end test files a second
  release over a TV episode and a third that never delivers, checking that
  the first replaces and the second is refused.
- 2026-11-11, maintainability step 3a: `main.go` is split into nine files
  by concern (FUSE nodes, handle, pump, read-ahead cache, playback, webhook,
  profiling, inode map, wiring); a line-multiset check proves no code
  changed.
- 2026-11-11, maintainability step 3b: the read-ahead cache is
  `internal/readahead` with unit tests, and the control routes have their own
  mux; `/debug/vars` is no longer served.
- 2026-11-11, maintainability step 4a: 43 unchecked errors that could hide
  failures now return or log; the rest are marked intentional, and errcheck
  gates new ones in `make go-check` and CI.
- 2026-11-11, maintainability step 4b: `GET /api/sessions` lists open
  sessions and the last 200 closed ones, each with bytes served, time to first
  byte and an outcome (`served`, `probe`, `dead_swarm`, `unserved`).
- 2026-11-11, hardening step 1: opt-in bearer-token auth on the control API;
  the panel asks for the token, webhooks may pass it in the URL.
- 2026-11-11, hardening step 2: table tests pin the pump and handle decision
  helpers, and unit tests cover the read path's cache hit, short-read repair
  and release; handle.go and pump.go coverage went from 0.6% to 32.0%.
- 2026-11-11, hardening step 3: the library writes stubs under the path the
  mount serves; a different path in config.json is logged and overridden.
- 2026-11-11, hardening step 4: GoStorm's server on 8090 has read-header and
  idle timeouts and honours the same opt-in token (except `/echo`).
- 2026-11-11, reliability step 1: `GET /api/health` reports the mount, engine
  and state database, and the image's health check runs `flodni healthcheck`
  against it.
- 2026-11-11, reliability step 2: shutdown saves state, then unmounts with a
  lazy detach after 5 s and exits by 8 s at the latest, inside Docker's
  default 10 s grace period.
- 2026-11-11, reliability step 3: the engine's upload side waited on a request
  it could not yet allocate buffer for while granted requests went unread, so a
  seeder at the default 1 MiB per-peer allowance sent nothing until the
  leecher's ~35 s timeout; it now skips ungranted requests and restarts on a
  release; the end-to-end test gains a slow-seeder variant.
- 2026-11-11, reliability step 4: characterization tests pin stub naming,
  stub contents, movie, episode and season-pack filing, replacement and
  removal; internal/library coverage went from 48.1% to 68.9%.
- 2026-11-11, reliability step 5: the read path reaches the engine through an
  interface, and tests with a fake engine cover fetch, singleflight, prefetch
  and the pump; handle.go and pump.go coverage went from 32% to 80%.
- 2026-11-12, readability evidence step 1: ADR 0002 records that Flodni
  fills and protects its cache and records readability while the choice of
  release stays outside; what is not built yet is labelled designed.
- 2026-11-12, readability evidence step 2: `GET /api/torrents/health` serves
  the readability record (state database schema 12): consecutive failures,
  first and last failure, reason and source of the last failure and the
  close time of the last swarm-served read. A success keeps the row and
  resets the count. Rows of torrents that left the library are pruned at
  startup after a complete library scan; README documents the fields.
- 2026-11-12, readability evidence step 3: Flodni logs one JSON line per
  closed session with bytes by serving path and whether the swarm served a
  read (`swarm_served`), plus the file index and failure reason; the same
  fields join `/api/sessions`. The bytes are split by the path that
  answered, not into swarm and cache. README documents the line.
- 2026-11-12, readability evidence step 4: `GET /metrics/prometheus` serves
  the time-to-first-byte and read histograms, read counts and bytes by
  serving path, session outcomes, dead-swarm verdicts, warmup cache size,
  evictions and drops, behind the control token. The README lists the series.
- 2026-11-12, readiness step 1, phase a: the cache keeps per file the time of
  the last play a media server webhook confirmed and whether the quota
  evicted it (state database schema 13). Eviction takes head and tail
  together, files never played first, then played files, oldest first; a
  play protects for `played_protection_days` (default 90) from the last play.
  Each eviction logs one `[WarmupEvict]` line, and
  `flodni_playback_starts_total` and `flodni_warmup_cold_reopen_total`
  report plays and plays that opened on an evicted file. The per-file listing
  and its gauges (phase b), warm on add and catch-up are not built.
- 2026-11-12, readiness step 1, phase b: the cache keeps an internal
  in-memory list of which files have their head and tail windows on disk (not
  served), and a tail of a file shorter than the tail window can now complete.
  `flodni_warmup_files{window}` counts warm heads and fully covered tails, and
  `flodni_readability_torrents{state}` counts library torrents that read, that
  failed and that were never read, refreshed every 60 s. A head recreated at
  offset 0 over a stale size entry no longer leaves a hole. Warm on add and
  catch-up are not built.
- 2026-11-12, readiness step 2, phase a: the per-file warm record. The engine
  stamps each piece with the time it completed (RAM storage, so a stamp is a
  swarm delivery), and a read through the mount that leaves a file's head
  and tail windows complete records the file in the state database (schema
  14) as `warmed` with the delivery time, or `cached` when no stamp exists.
  `GET /api/torrents/health` serves the records as an experimental `files`
  array, and `flodni_warm_fills_total{trigger="mount"}` counts them. Nothing
  fills windows on its own yet: the warmer, its `warm_on_add` setting and its
  outcomes are not built.
- 2026-11-12, readiness step 2, PR 3b: the warmer. With `warm_on_add` (off by
  default, needs `warmup_dir`) a library add that wrote stubs queues each new
  file; two workers fill its head and tail windows in the background, pause
  while a playback is open, wake a closed torrent only if the engine still
  stores it, evict only never-played files and only with evidence of plays, and
  record each outcome per file (`present`, `failed`, `dropped_quota`,
  `dropped_no_evidence`, `evicted`, `lost`, `write_failed`; `warmed` also stamps
  `last_swarm_read_at`). Cache-side failures retry in memory at 10 minutes, 1
  hour and 6 hours. New series: `flodni_warm_fills_total{trigger="add"}`,
  `flodni_warm_fill_bytes_total`, `flodni_warm_cache_failures_total`,
  `flodni_warm_queue_files`, `flodni_warm_paused`. Tested against fakes; the
  end-to-end test (PR 4) is not written, so step 2 is not finished.
- 2026-11-13, readiness step 2, PR 4: the end-to-end variant `warm-on-add`
  runs the warmer against the real engine and the real FUSE mount (a 16 MiB
  head window and an 8 MiB tail window). Before any mount read the warm fill
  is counted as `warmed` and `flodni_read_bytes_total` is 0; after the seeder
  is killed an 8 MiB head read and a 4 MiB tail read match the seeder's
  sha256 and are served from `warmup_head` and `warmup_tail` only. Quota
  pressure, the pause and the wake of a closed torrent, and removal while
  queued stay on unit tests. Step 2 is finished.
- 2026-11-13, readiness step 3, PR A: the seams of catch-up in the warmer, with
  no caller: a room-only fill that asks the cache for room before anything is
  fetched, a per-file trigger so a catch-up file never evicts, the feeder that
  hands one title at a time to an idle warmer, and a refusal the feeder can tell
  from a busy engine.
- 2026-11-13, readiness step 3, PR B: catch-up is wired. With `warm_on_add`,
  `warmup_dir` and the state database in place, a pass one pace interval after
  start and every 6 hours lists the stubs and feeds the files that are not warm
  on disk, not evicted and not failing, newest first, at `warm_catchup_per_hour`
  (default 20, 0 off, read on each feed). It never evicts and ends the pass at
  a full cache; a failed file waits 20 hours and is given up after 4 failed
  attempts, computed from the record with no new state. New series:
  `flodni_warm_fills_total{trigger="catchup"}`,
  `flodni_warm_fill_bytes_total{trigger="catchup"}` and
  `flodni_warm_catchup_files{state}`. Tested against fakes; the end-to-end
  variant (PR C) is not written, so step 3 is not finished.
- 2026-11-13, readiness step 3, PR C: the end-to-end variant `catch-up` runs
  catch-up against the real engine across daemon restarts. With `warm_on_add`
  off nothing is logged and every series is 0. After `config.json` is
  rewritten and the daemon restarted, the first pass warms the title: one
  `trigger="catchup"` fill, every `trigger="add"` outcome 0, bytes above 0,
  `flodni_read_bytes_total` 0, one `[Warm]` line and a `warmed`, `cached` row.
  A second restart finds `pending` 0, feeds nothing and leaves the row alone;
  with the seeder killed, the windows read back from the SSD match the
  seeder's sha256. The quota stop, the eviction exclusion, the stale-mark
  overwrite and the 20-hour rule stay on unit tests. Step 3 is finished.
- 2026-11-12, readiness step 4, phase a: Flodni counts bytes past the head and
  tail windows that the engine or the read-ahead cache serve to a session with
  no confirmed playback, logs each session once at the margin
  (`unconfirmed_read_margin_mb`, default 64) and exports
  `flodni_unconfirmed_read_bytes_total`,
  `flodni_unconfirmed_read_sessions_total` and
  `flodni_playback_webhooks_received_total`. Counting only; refusal (phase b)
  is designed, not built.
- 2026-11-12, reliability step 6, part one: the 34 s stall after a tail probe
  is fixed in the cache. `torrstor` raised piece priorities but never dropped
  them on completion, so stale complete pieces used the engine's storage cap
  and the wanted piece was never requested; `MarkComplete` now drops the
  priority. 0 of 38 e2e runs stalled at 34 s, against 15 of 17 before. A 58 s
  peer-drop stall remains open.
- 2026-11-12, reliability step 6, part two: the 58 s stall is fixed in the
  cache. When the last piece completed, the capped cache briefly held every
  piece (it evicted at most once a second), so the engine advertised every
  piece and the seeder closed the connection as mutually complete; with
  that seeder the only peer and tracker, the reader waited for the next
  announce, a minute later. `MarkComplete` now evicts down to capacity
  before the engine records the piece. 0 of 40 `default` runs stalled,
  against 4 of 24 before.
- 2026-11-13, readiness step 4, entry condition for phase b: the end-to-end
  variant `late-confirm` (`make e2e`) holds a full read of the fixture half
  way once it has crossed an 8 MiB margin, posts a play webhook for the file
  in the middle of the read, and lets the read finish. The sha256 matches,
  `flodni_playback_webhooks_received_total{matched="true"}` counts the
  webhook, and when the session closes
  `flodni_unconfirmed_read_sessions_total{outcome="confirmed_late"}` counts
  it, with the `confirmed_late` `[ReaderBudget]` line. Test only; phase b is
  designed, not built.
- 2026-11-13, reliability step 7: Flodni says when no play webhook arrives. The
  state database (schema 15, `daemon_marks`) keeps the last matched webhook, the
  last unmatched webhook and the last unconfirmed reader session, written at
  most hourly. At start a `[Webhook]` line states the last matched webhook,
  whether played-file protection has a recorded play and whether the reader
  budget refusal is on. When a reader session closed unconfirmed within
  `playback_webhook_warn_days` (default 7, 0 = off) and no play webhook matched
  in that time, a `WARNING: [Webhook]` line (marked as possibly false when
  every caller was a known hasher or analysis tool), `warnings` in
  `/api/health` (never changes the status or the HTTP code) and
  `flodni_warning{name="no_play_webhook"}` report it. The README has a "Play
  webhook" section. The keep-alive remains designed, not built.
- 2026-11-13, reliability step 8, phase a: `flodni_torrent_idle_closes_total`,
  `flodni_torrent_reopens_total` and `flodni_read_stalls_total`, each split
  by `confirmed`, and `reopens`, `stalls`, `max_stall_ms` and `confirmed` on
  the session record. No behaviour change. Phase b (the hold) stays designed,
  not built, so the step is not finished. Its entry condition is met: the new
  keep-alive end-to-end variant asserts the known leak, a confirmed idle
  close after a webhook that arrived while the torrent was closed
  (`idle_closes{confirmed="true"}` at least 1). Phase b flips that
  assertion to 0.
- 2026-11-13, bug fix: a new open of a file no longer waits for a slow read
  of that file. The mount returned open flags 0, so the kernel dropped the
  file's cached pages at every open and waited for each page a read in flight
  held (a 15 s budget hold, a 30 s slot wait, fetch retries). Open now returns
  `FOPEN_KEEP_CACHE` when another handle of the path is live; the first open
  still drops the cache. Unit tests pin the flag; the real-kernel check is the
  `budget-refusal` variant (item 9: opens and reads under 1 s during a hold).
- 2026-11-13, readiness step 4: the opt-in refusal is tested end to end. The
  variant `budget-refusal` (`make e2e`) cuts a scan with EIO at the windows plus
  the margin after a 15 s hold, with no failed read and no dead-swarm verdict,
  asks the seeder for nothing more, lifts the hold with a late play, gives the
  next reader a fresh margin after a stop, and keeps other opens and reads of
  the file under 1 s while a reader is held. The test found that a held read
  froze every new open of the file; #133 fixed it. The pump park rests on
  fake-engine tests only.
- 2026-11-13, readiness step 5: CI runs each end-to-end variant on its own
  runner (a matrix after the image job, which hands over both images as an
  artifact), and one job named `End-to-end` fails if any variant failed; it is
  the check to require.
- 2026-11-13, before a deployment relies on it, step 4: the tail window's start
  is rounded down to a 4 KiB boundary, so a read of exactly the last N MiB of
  a file that is not page-aligned stays inside the window. Existing tail covers
  keep loading. The `warm-on-add` end-to-end variant is back to a 4 MiB tail
  window with the same assertion: 0 bytes through `fetch_block` for the 4 MiB
  tail read.
- 2026-11-13, before a deployment relies on it, step 7: removal by hash
  (`POST /api/library/remove`), the already-present check of an add and the
  reference check before a torrent is dropped match the full 40-hex hash in each
  stub's own link, not the eight digits in its name. Legacy stubs tagged with
  the first or the last eight are found in either folder, and a stub of another
  torrent that shares the digits is left alone. The walk reads every stub under
  the folder (a few hundred bytes each); a stub with no readable hash is skipped
  and logged once per walk.
- 2026-11-13, before a deployment, step 8: `warm_catchup_scope` (`"all"` by
  default, `"retries"`) limits catch-up to the files warm on add attempted and
  did not finish, so a deployment with warm on add and no backlog walk retries
  its own failures, drops and restarts at a low pace. New: the queued time
  (`file_state.queued_at`, schema 16, additive), the state `out_of_scope` of
  `flodni_warm_catchup_files`, `flodni_warm_catchup_scope{scope}`, and
  `scope` on the `[WarmCatchup]` lines. Tested against fakes and the migration;
  the end-to-end variant `catch-up` runs scope `"all"`.
- 2026-11-13, before a deployment relies on it, step 2: a cache disk that
  vanishes at runtime means no cache, and one absent at start gets a cache on
  the filesystem underneath with at most a WARNING (the deployment keeps it
  off that, see the README's "Cache disk"). The cache directory is created at
  start only, a failed
  unlink is not an eviction, an unreadable directory keeps the old total, a
  read-only directory serves reads, and a start warns when the cache did not
  initialise or `warmup_dir` is not a separate mount. A missing
  `.dense-migrated` checks each head for holes (`SEEK_DATA`/`SEEK_HOLE`) and
  drops only those; the startup inode prune needs a whole walk.
- 2026-11-13, CI speed-up: the image job is split. `Image build` builds and
  saves both images and the end-to-end matrix starts from it; the required
  check `Image (build, smoke test, scan)` runs the smoke test and the scan
  beside the matrix and fails if the build failed. The late-confirm and
  budget-refusal variants shorten the session idle close and cleanup tick with
  test-only environment variables.
- 2026-11-13, before a deployment relies on it, step 6: `POST /api/warmup/evict`
  with `{"target_gb": N}` (or `target_bytes`) evicts the cache down to that size
  now, by the quota's own victim order and with its log lines (trigger
  `request`), counters and eviction marks. Behind `api_token` like every route
  but `/control` and `/api/health`. It deletes cache files and nothing else.
  Tested in the cache and the handler; not run in the end-to-end test.
- 2026-11-13, before a deployment relies on it, step 1: one wake path
  (`torr.LoadStored`) that refuses a hash the engine does not store, unlink
  through the mount following the library's reference check, and a load, a
  save and a removal serialised in the engine. The end-to-end variant
  `removal` removes three titles back to back while a fourth is read, and the
  removed ones stay removed.
- 2026-11-13, before a deployment relies on it, step 3: an open no longer
  serves another file's head for a stale stub index when the torrent is in
  the engine, and the warmer refuses a size mismatch. New:
  `flodni_stub_index_mismatch_total` and the warm outcome `index_mismatch`.
  Tested against fakes; a torrent with no saved file list and a stale stub that
  collides with a sibling's warm head is still open; the audit of Ideas round,
  step 8 lists such stubs.
- 2026-11-13, reliability step 8, phase b: the keep-alive. While a file has a
  webhook-confirmed playback its torrent is not closed for idleness. The
  engine's wrapper asks an injected predicate (`torr.HoldExpiry`) before it
  closes an idle torrent; the answer derives from the playback state, so a
  reopened torrent is held again. A stop webhook releases it at once; the
  ceiling is `playback_keepalive_minutes` (default 15, 0 turns the keep-alive
  off) after the last read or play or resume webhook, and a pause does not
  extend it. `playback_keepalive_max_torrents` (default 4, minimum 1) caps the
  held torrents. `flodni_keepalive_releases_total{reason}`,
  `flodni_keepalive_evicted_total` and `flodni_keepalive_held_torrents`; the
  `[Webhook]` startup line states the settings. The end-to-end `keep-alive`
  variant flips the leak assertion: no idle close over two idle windows after
  the webhook, then a stop webhook releases the torrent and it closes. The
  engine's `IsPriority` mark no longer holds the idle timeout; it still
  drives aggressive download mode. Step 8 is finished.
- 2026-11-14, Jellyfin support removed: Plex is the only media server; a config
  naming jellyfin logs a WARNING and runs as Plex.
- 2026-11-14, repository layout: the daemon moved from the root to
  internal/daemon, with cmd/flodni as the entry point; no behaviour change.
- 2026-11-14, before a deployment, step 9: `plex_path_map`, an opt-in map from
  media server library prefixes to container paths. A play or stop webhook
  whose file name matches no registered file is resolved through the library
  symlink (one `lstat` and `readlink`) and matched by the target's base name,
  and a play marks that file played. Off while the map is empty. New metric:
  `flodni_playback_webhook_match_rule_total{rule}`. Tested against symlinks in
  a temporary folder, not against a media server.
- 2026-11-14, ideas round, step 7 (#165): a threat model for the control API,
  the GoStorm port, the FUSE mount and the play webhook, and a CodeQL workflow
  with a manual cgo build step; stale comments and settings hints corrected.
- 2026-11-14, ideas round, step 5 (#166): `/metrics/prometheus` gains the
  engine short-read counters, open sessions and Go runtime and process
  gauges; the default end-to-end variant runs `promtool check metrics`.
- 2026-11-14, ideas round, step 2 (#167): the periodic inode prune has the
  start-up prune's "the walk was whole" guard, and a stub whose size is out of
  range is counted apart and no longer blocks the prunes.
- 2026-11-14, ideas round, step 9 (#168): once one file of a pack gave up,
  catch-up reports the pack's other files as `gave_up` until a swarm delivery.
- 2026-11-14, ideas round, step 1 (#169): a panel save keeps `config.json`'s
  mode, and a new file is created with 0600.
- 2026-11-14, ideas round, step 3 (#170): shutdown closes open sessions first,
  so a stop within two minutes of a session keeps its verdict and `[Session]`
  line.
- 2026-11-14, ideas round, step 6 (#171, #184): the warmup hook test waits for
  the write worker; the daemon test exposed a race in the warmup read path (a
  negative-cache entry hid a head created during the miss for up to 10 s),
  fixed there, and cache paths stay inside the cache directory.
- 2026-11-14, ideas round, step 8 (#172): `GET /api/library/audit` compares
  every stub's index and size with the engine's stored file list and reports
  the mismatches; it wakes nothing and writes nothing.
- 2026-11-14, ideas round, step 10 (#173): the end-to-end variant
  `warm-lifecycle` covers pause, wake of a closed private torrent and removal
  while queued, with a canary tracker that only a public torrent may reach.
- 2026-11-14, ideas round, step 4 (#174): the warmup cache's drift sources are
  closed (eviction total, `dropResidue`, corrupt-cache removal, a leaked
  descriptor), and the quota counts allocated blocks with a per-path ledger
  instead of apparent size.
- 2026-11-14, ideas round, step 17 (#175): an opt-in AppArmor profile and
  `docs/deploy/least-privilege.md` for a narrower FUSE container; designed, not
  verified on a host.
- 2026-11-14, ideas round, step 11 (#176): a metainfo that never arrives is
  recorded as `metainfo_timeout`, once per 10-minute occasion per torrent.
- 2026-11-14, ideas round, step 16 (#177): `/api/torrents/health` gains an
  experimental `swarm` object (peers, seeders, last piece, rate) for awake
  torrents, and `flodni_swarm_awake_torrents`.
- 2026-11-14, ideas round, step 13 (#178): a replaced or deleted stub's open
  session is closed before its row is cleared, and the start-up prune needs
  stubs in both `movies` and `tv`.
- 2026-11-14, ideas round, step 15 (#179): the check before a replacement reads
  a bounded sample of up to 3 files of the release, concurrently.
- 2026-11-14, ideas round, step 12 (#180): the first read after a session
  closed opens a new session linked by `resumed_from`, so a paused playback is
  no longer cut short.
- 2026-11-14, ideas round, step 19, design (#181): ADR 3 proposes the explicit
  file-lifecycle API.
- 2026-11-14, ideas round, step 18 (#182): catch-up warms an evicted file again
  when a play was confirmed inside the protection window, never evicting
  another file to make room.
- 2026-11-14, ideas round, step 14 (#183): specials are filed as season 0,
  lone samples are dropped, title years stay intact and accented letters fold
  in episode keys. Only new adds change.
- 2026-11-14, ideas round, step 20, part a (#185): a machine-readable startup
  report, logged as `[Startup] report` and served at `GET /api/startup`, with
  the startup log lines pinned by a test.
- 2026-11-14, ideas round, step 26 (#186): a periodic and on-demand tracker
  scrape of each stored torrent, reported per torrent in
  `/api/torrents/health`. Experimental.
- 2026-11-14, ideas round, steps 22 and 23 (#187): opt-in read-recency
  eviction (`warmup_eviction_order: "read"`) and next-episode warming
  (`warm_next_episode`), both off by default.
- 2026-11-14, CI (#188): the image build shares one build stage and caches Go
  modules, so the seeder build no longer recompiles the daemon.
- 2026-11-14, ideas round, step 19 (#189): ADR 3 is built. `add` takes
  `replace` and `dry_run`, `GET /api/library/file` projects a file's state and
  `POST /api/library/confirm` records a manual play (fail closed without an
  enforced token).
- 2026-11-14, ideas round, steps 24 and 25 (#190): opt-in reader-budget
  follow-ups (fill attribution, separate floors, a read-ahead clamp) and a
  private torrent's redial of its last tracker peer, both off by default, and a
  change-only wake of idle peers, which is always on. The wake cost on a large
  public swarm is not measured.
- 2026-11-15, durable records steps 1 to 17: a durable engine database whose
  failed commits reach the caller; `wipe` removed; an Origin check, an optional
  Host allowlist and `Sec-Fetch-Site` on an open port, with `/shutdown`, the
  play webhook and adds by `GET /stream` closed; no tracker passkeys in stubs or
  logs, and the media server token kept out of the API; a stored private
  torrent's own trackers win over an add's (hard rule 3), with an offline audit
  and remove for foreign trackers already stored; engine-not-ready kept apart
  from not stored; a fail-closed drop check; linked and orphan reporting; one
  fuzzy matcher; eviction to a low-water mark; and enforced `App` test isolation
  (#202 to #216).
