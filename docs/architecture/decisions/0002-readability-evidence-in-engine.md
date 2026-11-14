# 2. Record readability in Flodni, choose releases outside it

Date: 2026-11-12

## Status

Accepted

## Context

A deployment around the engine ran jobs outside it to cover three gaps.

**Cold first reads.** A periodic job pre-reads the first and last few
megabytes of new files through the mount. A cold header read takes about
6 s; a cached one about 0.01 s. The job has three limits: a file added
between runs stays cold until the next run, the job cannot see the cache
evicting what it warmed, and a bulk import leaves a large backlog.

**Dead torrents.** A periodic job outside the engine needs evidence that a
torrent's files do not read. Today it scrapes log lines, and there they
vanish when the container is recreated. Most failures analysed by hand were
slow swarms that recovered, so a verdict from one bad session is not
enough.

**Whole-file readers.** Tools that read entire files (subtitle extraction,
media-server analysis jobs) pull complete files through the swarm, though
only playback needs that.

The engine already knows the facts these jobs reconstruct from outside:
which path served each read (a cache window, the read-ahead cache or the
engine), whether a session got anything from the swarm, and which sessions
ended with a failed read and nothing served. It counts bytes by serving
path (the engine, the read-ahead cache, the head and tail windows on disk)
and records whether the swarm served a read, but it cannot say which bytes
the swarm delivered: bytes fetched earlier reappear as cache hits.

## Decision drivers

- Reliability of playback matters more than convenience.
- Evidence must survive a container recreate and name torrents exactly.
- A false positive cuts real playback, so the rule for success must be
  strict.
- One maintainer: no new integration with named external tools; the media
  server webhook stays the only one.

## Considered options

For cache filling:

1. Keep an external reader through the mount.
2. Warm in process when a file is added, in index mode.
3. Warm on the media server's first scan.

For readability evidence:

- a. Keep scraping log lines.
- b. Add a second store for readability.
- c. Extend the existing per-torrent failure counter in the state database.

## Decision

Mechanism lives inside Flodni; policy stays outside. Which release to use,
and when to replace one, is decided by tools outside Flodni.

Cache filling: option 2. Option 1 is the status quo and keeps all three
limits, because a reader on the mount cannot see the cache and competes
with playback for the same read path. Option 3 is partly delivered by index
mode already, where hash-checked data reaches the cache and tail coverage
survives a restart, but it fills a window only when something reads the
file, so a file nobody has scanned or played stays cold.

Readability evidence: option c. Option a ties consumers to log wording, and
the lines do not name a torrent exactly: one carries the file name, the
other the first eight characters of the hash. They survive a recreate only
where the log directory is a volume. Option b adds a store to keep
consistent with a table that already holds the same per-torrent key.

**What Flodni does**

- Records readability per torrent (exists: the failure counter, the time of
  the last swarm-served read, and the reason and source of the last
  failure), and per file (exists, experimental).
- Fills head and tail cache windows in index mode: when a file is read, and
  on add and, for the library's existing files, by catch-up (both exist,
  experimental, off by default).
- Bounds the bytes that readers other than playback may pull from the
  swarm (designed).
- Carries out a replacement it is handed through the library API, and keeps
  the existing files unless the new release delivers data (exists).

**What Flodni does not do**

- Choose or grab replacement releases.
- Talk to media managers, indexers or trackers by name.
- Scrape trackers.
- Schedule backup windows.
- Rename stubs.

**The success rule.** Only data the swarm delivered counts as success. A
cache hit is never evidence that a swarm is alive. Today two things clear
the failure counter: a session in which a read was served through the
engine while at least one peer was connected (`servedByNet`, set in
`recordRead` and applied by `reportSwarmVerdict` in `internal/daemon/ttff.go`), and a wake
that had to wait for the metainfo and got it from the swarm (`Wake` in
`internal/gostorm/native/native.go`). Every new writer of the readability
record follows the same rule.

**The failure rule.** A failure is counted once per session, and only when
a read gave up, the swarm served nothing and the session lasted at least
`sessionVerdictFloor` (25 s). A read Flodni refuses on purpose is not a
failure: a budget refusal never counts as a failed read, as a cancelled
read and a full read slot do not count today.
A wake the engine refuses because it keeps no record of the torrent is not a
failure either: it is recorded as the reason `not_stored`, without touching the
failure count, and never becomes a verdict.

**Defaults.** Warming and budget enforcement are off by default:

- Warming starts downloads and announces nobody asked for, which private
  trackers may count against the account.
- Without a playback webhook there is no reliable signal that a reader is
  not playback.
- A false positive in budget enforcement cuts real playback.

## What external consumers may rely on

Consumers identify a torrent by its full info hash, never by the tag in a
stub name, and a file by its index.

Status of each part:

| Part | Status |
|---|---|
| Per-torrent failure counter with first and last failure time, in the state database | Exists; a success keeps the row, resets the count and stamps the time |
| Session outcomes at `/api/sessions` (the last 200 closed sessions, in memory, lost on restart) | Exists |
| `[DeadSwarm]` log lines | Exists; the wording is not a contract |
| `GET /api/torrents/health` with the readability fields | Exists; the fields are in the README |
| Time of the last swarm-served read, reason and source of the last failure | Exists |
| One `[Session]` log line per closed session: bytes by serving path, `swarm_served`, file index (the README documents it as a contract) | Exists; sessions open at shutdown are not counted; a read after a session closes opens a linked session (`resumed_from`) |
| Prometheus text exposition at `GET /metrics/prometheus`: session latency histograms, read counts and bytes by serving path, session outcomes, dead-swarm verdicts, warmup cache size and evictions (the README lists the series) | Exists |
| Per-file warm record: readability and cache state per file, served as the experimental `files` array on `GET /api/torrents/health` (state database schema 14); the engine stamps each piece with its completion time, a read through the mount that completes a file's windows records it, and warm on add and catch-up record each of their outcomes | Exists, **experimental**; written by the mount path in index mode (`warmed`, `cached`) and by warm on add (all outcomes: `present`, `failed`, `dropped_*`, `lost`, `write_failed`; catch-up writes all but `dropped_*`) |
| Played and evicted state per file (state database schema 13); quota eviction takes head and tail together, files never played first, then played files, oldest first; one `[WarmupEvict]` log line per eviction; `flodni_playback_starts_total` and `flodni_warmup_cold_reopen_total` | Exists; "played" is a playback confirmed by the media server webhook, so without webhooks nothing is ever played and the order is oldest first |
| Internal list of files whose head and tail windows are on disk; `flodni_warmup_files{window}` and `flodni_readability_torrents{state}` built on it and on the record | Exists; the per-file list is internal and not served, only the aggregates are exported |
| Background fills evict only files never played, or nothing | Exists for warm on add: it evicts never-played files, oldest first, only with the state database attached and at least one play recorded, and nothing otherwise. Catch-up never evicts and never re-warms an evicted file |
| The fill primitive: completes one file's head and tail windows in chunks from a torrent the engine already has, writing only verified bytes, with room made first under the policy above | Exists; warm on add and catch-up call it |
| Warm on add: after a library add that wrote stubs, a background queue (two workers, 4096 files, in memory) fills each new file's windows; off by default (`warm_on_add`, needs `warmup_dir`); pauses while a playback is open; records each outcome per file; retries cache-side failures at 10 minutes, 1 hour and 6 hours | Exists, **experimental**; tested against fakes and by the `warm-on-add` end-to-end variant (one title: fill, evidence, windows read back with the seeder dead; pause, wake and removal while queued are unit tests only) |
| Catch-up: a pass at start (one pace interval after the daemon starts) and every 6 hours lists the stubs and feeds the files not warm on disk, not evicted and not failing to the warmer, one title at a time, newest stub first, at `warm_catchup_per_hour` (default 20, 0 off), only while the warmer is idle; it needs `warm_on_add`, `warmup_dir` and the state database, never evicts (a full cache ends the pass), leaves evicted files to their next play, and retries a failed file no sooner than 20 hours later and at most 4 failed attempts; `warm_catchup_scope` `retries` (default `all`) feeds only files warm on add attempted and did not finish (a failed read, `dropped_*`, `lost`, `write_failed`, or queued over an hour with no outcome, from `file_state.queued_at`, schema 16), with the same evidence rules; `flodni_warm_fills_total{trigger="catchup"}` and `flodni_warm_catchup_files{state}` | Exists, **experimental**; tested against fakes and by the `catch-up` end-to-end variant (one title: silent with the switch off, one pass after a restart that warms it, a second restart that feeds nothing, windows read back with the seeder dead); the quota stop, the eviction exclusion, the stale-mark overwrite and the 20-hour rule are unit tests only |
| Byte budget for non-playback readers: bytes past the windows served to unconfirmed sessions are counted and logged (`[ReaderBudget]`, `flodni_unconfirmed_read_*`) | Counting exists; refusal exists, **opt-in, off by default** (`refuse_unconfirmed_reads`), tested against a fake engine and end to end (`budget-refusal`); the pump park is fake-engine only |

What the endpoint does:

- It sits behind the same opt-in token as the other control routes: open
  when no token is set, bearer header only when one is. Unlike
  `/api/health` it is not among the routes that stay open.
- It returns hashes, counts, reasons and times, plus the experimental
  per-file records (file index, readability, cache state, times).
- It answers 503 when the state database is off. Nothing is recorded then:
  no verdict is written, no `[DeadSwarm]` line is logged and
  `/api/sessions` never reports `dead_swarm`.
- A torrent with no row has no failure on record. That is not a verdict
  that it reads: a torrent nobody opened has none. A success keeps the row
  with the count at 0 and the time of the read.
- Rows go when a torrent is removed from the library and are pruned at
  startup, after a complete scan of the library.
- Per-file records exist (experimental): the mount path, warm on add and catch-up write
  them, the warmer with each of its outcomes per file. Sessions
  carry a path, a hash and the file index their stub holds, but the
  per-torrent table has no file key.

The endpoint and the fields landed in the roadmap step
[Readability evidence](../../roadmap.md#readability-evidence), step 2. The
cache-filling items belong to
[Readiness and protection](../../roadmap.md#readiness-and-protection).
Nothing in the second group may be relied on until its step ships; this ADR
is updated when it does.

## Consequences

- Positive: outside tools get durable, exact evidence without parsing logs
  or stub names, and the policy about which release to replace can change
  without a Flodni release.
- Positive, once the warmer ships: a new file can start from the SSD on its
  first play, including files added between scheduled runs, and bulk
  imports are worked off across restarts.
- Positive, once the budget ships: whole-file readers stop pulling complete
  files through the swarm.
- Negative: warming puts load on swarms and adds announces and downloads
  nobody requested, which is why it is off by default.
- Negative: warming competes for the cache quota. The rule that background
  fills evict only files never played, or stop at a full quota, is built for
  warm on add. A new head file made by a read still evicts files never played
  first, then played files, each oldest first; a play protects its file for
  90 days by default.
- Negative: Flodni grows a public read-only contract that has to stay
  stable, and the success rule has to be kept by every writer of the
  record.
- Warm on add and catch-up waited for the read stall in Reliability step 6,
  now fixed in the cache. Warm on add pauses while a playback session is
  open (built).
- Index mode (`warmup_dir` set; the cache's verified-write mode) is a
  prerequisite for warming. The code checks only `warmup_dir`: warm on add
  also runs with `pump_on_open` on, which changes how scans read, not where
  the windows are written. The cache-window list and `flodni_warmup_files`
  exist wherever the warmup cache is on; outside index mode tail coverage
  does not survive a restart, so the tail count starts at 0 again.
- The model has the warmer: a "Warm on Add" component in the daemon, fed by
  the library manager, filling the index cache and reading from the engine
  (`docs/architecture/model/containers.dsl`).

## Links

- Roadmap: [Readability evidence](../../roadmap.md#readability-evidence),
  [Readiness and protection](../../roadmap.md#readiness-and-protection)
- Ideas: [ideas](../../ideas.md) entries "Warm the next episode during
  playback", "Warm again after eviction", "A swarm signal from the engine"
  and "Per-file replacement check for packs"
