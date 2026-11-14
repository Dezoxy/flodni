# Flodni

Flodni presents torrents as ordinary media files. Plex scans and plays them
from a FUSE mount, and the bytes are fetched
from the swarm when they are read. Nothing is downloaded to disk in full.

It is one Go binary: a torrent engine, a FUSE virtual filesystem and a small
control API.

## Status

Flodni is a hard fork of
[Tiramisu](https://github.com/MrRobotoGit/tiramisu) by Matteo Rancilio,
started on 2026-11-10. It no longer follows upstream; the reasons are in
[ADR 0001](docs/architecture/decisions/0001-hard-fork-tiramisu.md).

The code is Tiramisu's with this fork's reliability changes. The Go module,
the binary, the image paths and the environment variables are named Flodni.
It is maintained by one person for their own use. Expect breaking changes
and no support.

## Documentation

| Document | What it holds |
|---|---|
| [docs/roadmap.md](docs/roadmap.md) | What is planned, in order, and what is done. |
| [docs/ideas.md](docs/ideas.md) | What could be improved, not yet planned. |
| [docs/architecture/README.md](docs/architecture/README.md) | The C4 model, its views and the decisions. |
| [README.upstream.md](README.upstream.md) | Tiramisu's user guide as inherited: install, configuration, API. |
| [SECURITY.md](SECURITY.md) | How to report a vulnerability. |

## Building

Linux only: the FUSE layer is cgo against `libfuse3`.

```bash
sudo apt-get install -y libfuse3-dev pkg-config
make go-check   # gofmt, build, vet
make go-test    # tests with the race detector
```

The container image builds anywhere Docker runs:

```bash
docker build -f docker/Dockerfile -t flodni:dev .
```

`make e2e` runs the end-to-end test (Docker and `/dev/fuse`).
`FLODNI_TEST_SESSION_IDLE_CLOSE` and `FLODNI_TEST_SESSION_TICK` exist for that
test only; they are not configuration.

## Running

The image mounts the FUSE view at `/mnt/flodni-virtual`, reads library
files from `/mnt/flodni-real` and keeps its state under `/state`. Set the
paths with `FLODNI_ROOT_PATH`, `FLODNI_SOURCE_PATH`, `FLODNI_MOUNT_PATH`,
`FLODNI_STATE_DIR`, `FLODNI_LOG_DIR`, `FLODNI_LOG_FILE` and
`FLODNI_HOST_MOUNT_HINT`; the Plex settings use `FLODNI_PLEX_URL` and
`FLODNI_PLEX_TOKEN`. The older `TIRAMISU_*` names still work and lose to the
`FLODNI_*` ones. Existing state carries over: the database is still
`tiramisu.db` and the log file `tiramisu.log`.

Shutdown first closes every open session, so each gets its verdict and
`[Session]` line, then saves state and unmounts; a mount that will not
unmount is detached lazily after 5 s, and the process exits by 8 s at the latest. That
fits inside Docker's default 10 s `stop_grace_period`. A longer grace period
is fine; do not set it below 10 s, or Docker kills the process before the
deadline.

Deleting a title with `rm` through the mount removes its stub. The torrent
and its cache files go only when no other stub uses it, so deleting one
episode of a season pack keeps the pack for the others. The release is not
added to the blacklist. An `.mkv` outside the `movies/` and `tv/` directories
is refused with `EPERM` and left in place. A media server may rescan after the
removal.

### Page cache on a second open

The kernel drops a file's cached pages at every open unless the filesystem
says to keep them, and that drop waits for every page a read in flight still
holds, so one slow read (a fetch, a slot wait, the opt-in budget hold below)
would freeze every new open of the file. Flodni keeps the cache
(`FOPEN_KEEP_CACHE`) when another handle of the same path is already open, so
a second reader is not held up by the first one's slow read. The first open of
a path, with no handle live, still drops the cache, which is what clears the
pages of a release that was replaced. Reads on an already open descriptor were
never affected. The pages stay valid because a file's inode number comes
from its torrent file (hash and index), so a replacement release has another
inode and its own cache.

The command-line paths (the entrypoint's `FLODNI_SOURCE_PATH` and
`FLODNI_MOUNT_PATH`) win over `physical_source_path` and `fuse_mount_path` in
`config.json`; a different value there is logged and overridden.

The control server on `metrics_port` (default 9080) is open unless you set a
token. Set `api_token` in `config.json` or `FLODNI_API_TOKEN` (the variable
wins) and every route except the panel page and
`/api/health` needs `Authorization: Bearer <token>`. The Plex webhook cannot send
headers, so `/plex/webhook` and `/webhook` also accept the token in the URL,
for example `http://host:9080/plex/webhook?token=<token>`; no other route
does. `/control` loads without it and asks for the token on its first call,
then keeps it in the browser's local storage. `GET /api/config` never
returns the token, and saving from the panel leaves it unchanged. The
same token covers GoStorm's port 8090 (every route except `GET /echo`, header
only, no `?token=`); external clients of 8090 must send it. There is no TLS:
see `SECURITY.md`.

`GET /api/health` on the control port reports the mount, the engine and the
state database, each as `{"ok": ..., "reason": "..."}`, with an overall
`status` of `ok` (HTTP 200), `degraded` (engine or state database down) or
`down` (mount down), the last two with HTTP 503. It is open even when a token
is set and returns status words only. The image's health check runs
`flodni healthcheck`, which requests it on `127.0.0.1:<metrics_port>` and
exits 0 on HTTP 200 and 1 otherwise; run it with
`docker exec <container> flodni healthcheck`.

The body also carries `warnings`, an array that is always present and is `[]`
when nothing is wrong. Its one value today is `no_play_webhook`, an advisory:
it never changes `status` or the HTTP code, so a deployment without a play
webhook stays healthy. The array is computed from memory, without a check.
See "Play webhook" below for what raises it.

## Play webhook

Some features act on a play that the media server reports by webhook, and do
nothing without one. A webhook that never arrives, is refused by the token
check or matches no file looks exactly like a library nobody plays: nothing
fails, the features are just inactive.

| Feature | Needs the webhook to | State |
| --- | --- | --- |
| Played-file protection (see "Warmup cache eviction") | mark a file played, so eviction ranks it last | Built |
| Reader budget refusal (see "Reader budget") | arm: refusal starts at the first matched play webhook after start, and a webhook confirms a play | Built, opt-in, off by default (`refuse_unconfirmed_reads`) |
| Keep-alive of a confirmed play (see "Keep-alive") | confirm the play, so its torrent is not closed for idleness | Built, on by default (`playback_keepalive_minutes`) |

- **Set it up.** Point the media server's webhook at
  `http://<host>:<metrics_port>/plex/webhook` (`/webhook` answers the same
  way). With `api_token` set, the URL needs `?token=<token>`, because
  Plex cannot send headers; without it every webhook is answered
  401 and nothing is counted. A webhook counts when it is a play or resume
  of a movie or an episode. It is matched when it resolves to one file of
  the library.
- **How a webhook is matched.** The media server reports each file as a path
  of its own library, and the first match wins: (1) the base name of that
  path, byte for byte, against the registered files; (2) with
  `plex_path_map` set, the stub name behind a library symlink (below);
  (3) the IMDb id; (4) when only a library type and an IMDb id are known, the
  one registered file of that type with no id yet; (5) a fuzzy title match,
  which can pick the wrong file. A play is confirmed and marked played only
  for the file the match resolved to.
- **`plex_path_map`** in `config.json` (default empty = off, read live) is for
  a library of Sonarr or Radarr named symlinks into the mount, where the
  media server's file name never equals the stub's (`Show Name - S01E02 -
  Title WEBDL-720p.mkv` against `Show_Name_S01E02_1a2b3c4d.mkv`). It maps a
  media server path prefix to the path of the same files inside this
  container, for example `{"/movies/": "/media/movies/", "/series/":
  "/media/series/"}`; both sides must be absolute, and an entry that is not
  is logged as a `WARNING` and ignored. Only when no file name matched, each
  reported path is taken through the longest matching prefix, checked once
  with `lstat`, and when it is a symlink its target's base name is matched
  against the registered files like a file name (a relative target is read
  against the link's folder). It does not walk directories or follow a chain,
  and a missing path or a regular file matches nothing, so the later steps
  run as without it. The same name decides which file a play marks as played
  (the strictness of that choice is unchanged) and which file a stop
  releases. Use it when
  `flodni_playback_webhooks_received_total{matched="false"}` moves for a
  library of symlinks; the library must be mounted read-only in this container
  at the mapped path. The lookup is meant for a local (not network) library
  bind and runs inline in the webhook handler, at most 8 paths per webhook: a
  hung mount delays that webhook. Prefixes and targets are cleaned at load;
  a prefix given twice keeps the first in sorted order, with a warning. Built,
  unit-tested against symlinks in a
  temporary folder; not run against a media server.
- **Which rule matched.**
  `flodni_playback_webhook_match_rule_total{rule}` counts the same webhooks
  by rule: `basename`, `path_map`, `imdb` (the id, or the bootstrap), `fuzzy`
  or `none`. `fuzzy` is a guess: a library whose plays show up there mostly
  is a candidate for `plex_path_map`.
- **Check it.** The startup line, `[Webhook]`, says when the last matched
  play webhook arrived (`never` if none), whether played-file protection has
  a recorded play (`active`) or not (`inactive`), whether the reader budget
  refusal is `off` or `on` (it arms at the first matched play webhook after
  that start, never from a stored time), the warning window, and the
  keep-alive settings (`keep-alive: on (15 min ..., max 4 torrents)`, with
  `inactive until a play webhook matches` while none has matched, or
  `keep-alive: off`). At run time
  `flodni_playback_webhooks_received_total{matched}` counts the webhooks and
  `flodni_playback_webhook_last_timestamp_seconds{matched}` is the time of the
  last of each kind (0 = none seen). `matched="false"` moving while
  `matched="true"` does not means webhooks arrive but resolve to no file.
  A webhook refused for a missing or wrong token logs
  `[Control] 401 POST /plex/webhook from <address>` (or `/webhook`).
- **The warning.** It is raised when a reader session crossed
  `unconfirmed_read_margin_mb` and closed with no play confirmed within
  `playback_webhook_warn_days`, and no matched play webhook arrived in that
  time (a webhook never received counts). It appears as `no_play_webhook` in
  the `warnings` array of `/api/health`, as
  `flodni_warning{name="no_play_webhook"}` set to 1, and as one
  `WARNING: [Webhook]` log line when it turns on (one more line when it
  clears, none while it holds). It is re-evaluated hourly and when such a
  session closes. A session that crossed the margin is one the budget
  counted; see "Reader budget" for what counts.
- **A false warning.** A hasher, a copy or a media server's analysis pass
  also closes as an unconfirmed session, so the warning can fire in a
  library where nobody played anything and none was expected. When every
  such session since start (or since the last matched webhook) came from a
  process named like a known hasher or analysis tool (`md5sum`, `sha1sum`,
  `sha224sum`, `sha256sum`, `sha384sum`, `sha512sum`, `b2sum`, `cksum`,
  `xxhsum`, `ffprobe`, `mediainfo`), the log line says it is possibly a false
  warning. The warning still fires; nothing is excluded. The name is empty
  for a reader in another PID namespace (a media server in its own
  container), which then counts as unknown, so the note appears only for
  tools run where Flodni can see the process. The process name is
  not known for times restored from a restart, so a restart removes that
  note. A file smaller than the head and tail windows plus the margin never
  crosses the margin, so a library of only small files stays silent.
- **`playback_webhook_warn_days`** in `config.json` (default 7, read live; 0
  turns the warning off, the startup line and the series stay; a negative or
  absurd value is logged and replaced by 7).
- **State.** The three times (last matched webhook, last unmatched webhook,
  last unconfirmed session) are in the state database table `daemon_marks`
  (schema 15), written at most once an hour each. With the state database
  off they live since start and are gone at restart.

## Keep-alive

Built (Reliability step 8), on by default. The engine closes a torrent that
has had no reader for its idle timeout (15 s unless the engine's settings say
otherwise), which keeps idle titles cheap. A media server reads in bursts, and
a direct play or a throttled transcoder can wait longer than that between
reads; the torrent then closes and the next read reopens it with a new
announce and a new search for peers. The keep-alive stops the close while a
play is confirmed.

- **What it holds.** A torrent is not closed for idleness while a file of it
  has a webhook-confirmed playback that is not stopped. It needs the play
  webhook: with none configured, reachable and matching, nothing is confirmed
  and the keep-alive silently does nothing. See "Play webhook". A reader with
  no confirmed playback, a library scan, warm on add and catch-up keep the
  plain idle close. The hold is decided from the playback state each time the
  engine would close the torrent, so a torrent that was closed and reopened
  is held again and nothing is left to clear. It only suppresses a close: it
  adds no tracker, DHT, PEX or wake, so a private torrent gets nothing it
  would not have had.
- **How it ends.** A stop webhook releases the torrent at once; the engine
  then closes it after the stop's 30 s grace. Otherwise it ends
  `playback_keepalive_minutes` after the last activity, which is the later of
  the last read of the file and the last play or resume webhook. A pause
  webhook does not extend it, so a pause longer than the ceiling costs one
  reconnect on resume. A missed stop webhook is covered by the same ceiling.
  A read of a confirmed file whose stop never arrived (a scanner touching it)
  starts the ceiling again.
- **The cap.** At most `playback_keepalive_max_torrents` torrents are held at
  once. The most recently active win; a torrent over the cap falls back to the
  plain idle close and is counted once in `flodni_keepalive_evicted_total`.
  The ranking counts every confirmed playback, including torrents that still
  have a reader and so need no hold, so the held gauge can sit below the cap.
  Each held torrent keeps its engine cache, up to `CacheSize` (default
  128 MB), and its peer connections. Size the cap so that cap x `CacheSize`
  stays well under the container's memory limit: four held torrents at 512 MB
  is about 2 GB. The read-ahead cache is one global budget and does not grow
  with the number of plays. A held torrent keeps uploading what its cache
  holds, under the configured limits, for the play plus the ceiling.
- **The off switch.** `playback_keepalive_minutes` (default 15, read live) is
  the one switch: 0 turns the keep-alive off and every torrent idle-closes, a
  confirmed one included (priority no longer holds a torrent open). A negative
  value or one over 1440 is logged and replaced by 15. A ceiling above 15
  minutes is cut short when no handle is open, because the playback state is
  dropped 15 minutes after the last activity.
  `playback_keepalive_max_torrents` (default 4, read live) is the cap, never a
  switch: its minimum is 1, and 0 or a negative value is logged and replaced
  by 4.
- **Metrics.** `flodni_keepalive_held_torrents` (gauge),
  `flodni_keepalive_releases_total{reason="stop|ceiling"}` and
  `flodni_keepalive_evicted_total`, all present from 0; and
  `flodni_torrent_idle_closes_total{confirmed}` and
  `flodni_torrent_reopens_total{confirmed}` to see whether confirmed plays
  still close. The `[Webhook]` startup line states the effective settings.
- **Limits.** The 15 s idle timeout still applies to a torrent with no
  confirmed playback. The read-ahead pump's own 2 hour idle limit for a
  confirmed session is unchanged, and an open stream still restarts its engine
  reader every idle timeout; neither is part of the keep-alive. The state is
  rebuilt at most once a second, so a read shows in the hold within a second;
  a stop or a confirming webhook takes effect at once. Tested by unit tests
  and by the end-to-end `keep-alive` variant (a confirmed torrent is held
  through two idle windows and released by a stop webhook); the ceiling is
  unit-tested only.

## Readability record

`GET /api/torrents/health` on the control port lists what Flodni has
recorded about each torrent's reads. With a token set it needs the bearer
header (no `?token=`). It answers 503 when the state database is off:
nothing is recorded then. The fields below are the supported contract for
outside tools; log wording and stub names are not (the `[Session]` log
line below is the one log format that is).

| Field | Meaning |
|---|---|
| `hash` | Full info hash, lower-case hex. Join it with `GET /api/library/list`. |
| `consecutive_failures` | Sessions in a row that ended with a failed read and nothing served by the swarm. A session counts once, and only if it lasted 25 s. |
| `first_failure_at` | Start of the current run of failures; `null` when the count is 0. |
| `last_failure_at` | Last recorded failure; kept after a reset. |
| `last_failure_reason` | `no_peers`, `timeout` (peers connected, the read gave up), `not_active` (the engine did not have the torrent running), `unknown` (the engine did not answer in time), `not_stored` (the engine refused to wake the torrent because it keeps no record of it; not a failed read, see below); `null` for older records. |
| `last_failure_source` | `mount`: a reader through the FUSE mount. |
| `last_swarm_read_at` | Close time of the last session in which the swarm served a read, or the delivery time of a warm-on-add fill that completed a file's windows from the swarm; `null` if none was recorded. |

`?failing=1` keeps torrents with at least one consecutive failure,
`?min_failures=N` those with at least N.

- Only bytes from the swarm reset the count. A cache hit never does.
- `not_stored` is not a swarm failure. A read through the mount on a torrent
  the engine refused to wake (see "A stub the engine does not store") sets
  `last_failure_reason` to `not_stored`, `last_failure_source` to `mount` and
  `last_failure_at`, once per handle. It does not change `consecutive_failures`
  or `first_failure_at`, so such a torrent is not listed by `?failing=1` or
  `?min_failures=N`, and it is never a dead-swarm verdict. A session on it
  reports `failed_reads` 0. A later swarm read or failure replaces the reason.
- No entry means no evidence either way, not that the torrent reads.
- A `null` `last_swarm_read_at` is not a failure: reads answered from the
  caches do not set it.
- A session whose bytes came from the read-ahead pump rather than direct
  fetches may not set `last_swarm_read_at`; treat a single failure as weak
  evidence.
- A wake that had to fetch the metainfo from the swarm resets the count
  without setting `last_swarm_read_at`.
- A metainfo that never arrives is not counted: the open fails before a
  session exists. A cancelled read, a read Flodni refuses for the reader
  budget, and an open that fails before any read are not recorded either.
- Times are UTC and are taken when the session closes, up to two minutes
  after its last read.
- An entry goes when the last stub of its torrent is removed through the
  library API. A session still open on that torrent writes the entry back
  when it closes, and a stub deleted another way leaves it in place. Such
  entries stay until a startup prunes them. The prune runs only after a
  complete scan: both `movies` and `tv` exist and every stub was read. It
  is skipped otherwise, and the log says so.
- Join from `GET /api/library/list`: an entry whose hash that list does
  not carry belongs to a torrent that left the library. Ignore it.
- New reason and source values may appear; ignore the ones you do not
  know. `GET /api/library/list` always reports `file_index` (the
  engine's index, starting at 1; 0 means the stub carries none). Per-file
  records are the experimental `files` array below.

### Per-file records (experimental)

Each torrent object carries a `files` array, always present (`[]` when empty)
and always the last key. It is **experimental**: it is outside the contract
sentence above and the stable-names promise, and its names may change until
its first consumer runs against it. It is served only with the state database
on. Three things write it. In index mode (`warmup_dir` set), a read through
the mount that leaves a file's head and tail windows both complete records the
file. In any cache mode, a quota eviction records `cache: evicted`, also for a
file with no earlier record. With `warm_on_add` set, the warmer records every
attempt, its catch-up included (see "Warm on add"). A file nobody opened and
the warmer never tried has no record. A file whose windows were already
complete before this version may have none until a later read writes to them.
A torrent with records and no failure row is listed with
`consecutive_failures` 0 and nulls, unless `?failing` or `?min_failures` is
set; those keep their per-torrent meaning.

| Field | Meaning |
|---|---|
| `file_index` | The engine's file index, as in `GET /api/library/list`. |
| `readability` | `warmed`: the swarm delivered a piece under the windows (the engine stamps each piece when it passes its checksum); `present`: warm on add completed the windows and the engine holds no delivery time (rare); `failed`: a warm-on-add fill timed out; `unknown`: no verdict. `present` never replaces `warmed`; `failed` does, and the delivery time stays. The mount path writes only `warmed` and leaves `unknown` when the engine holds no stamp. |
| `cache` | State of the windows on disk, separate from `readability`: `cached`; `evicted` (quota eviction); `dropped_quota` and `dropped_no_evidence` (warm on add found no room and did not fetch, the first when it was allowed to evict never-played files, the second when it was not); `lost` (the windows vanished with no quota eviction mark) and `write_failed` (a write failed), both retried; `unknown` before any record. It is the last recorded event, not a live probe: a cache directory wiped by hand still reads `cached`. |
| `failed_attempts` | Warm-on-add fills in a row that got no bytes from the swarm in time; a `warmed` record resets it. Cache-side failures do not count. |
| `first_failed_at`, `last_attempt_at` | Start of the current run of failed fills, and the last warm-on-add attempt that recorded; `null` when unset. The mount path sets neither. |
| `last_swarm_delivery_at` | When the engine last completed a piece under the windows, not when the windows were written: a fill that read from RAM hours later still carries the delivery time. |

- Eviction changes `cache` only; it never changes `readability`.
- Times are RFC 3339 UTC or `null`.
- A scan by a media server that pulls the windows from the swarm counts as a
  swarm delivery, because the stamp is the fact, not who asked.
- The mount path does not touch `last_swarm_read_at` or the failure count of
  the torrent; the session verdict still owns those. Warm on add stamps
  `last_swarm_read_at` (with the delivery time, and it ends the failure
  count) only for a `warmed` outcome; `present`, `failed` and the cache-side
  outcomes never touch the per-torrent record.
- The series `flodni_warm_fills_total{trigger="mount",outcome}` counts the
  records the mount path wrote (`warmed`, or `cached` when no stamp exists);
  `trigger="add"` is warm on add.

## Session log

A session covers the reads of one torrent-backed file, from an open until
the file has been idle for a minute. `GET /api/sessions` lists the open
sessions and the last 200 closed ones, in memory. Each closed session is
also written to the log as one line, so a log store keeps the history. The
image tees the log to `tiramisu.log` under `$FLODNI_LOG_DIR` (default
`/state/logs`); that file survives a container recreate only when its
directory is on a mounted volume, and it is not rotated. Without the log FIFO the line
goes to stdout only. A session still open at shutdown is closed then, with
the same rules: one younger than 25 s is never condemned as a dead swarm.

```text
[GoProxy] 2026/11/12 09:02:10 [Session] {"path":"/mnt/flodni-real/tv/Example/Example.S01E01_0123abcd.mkv","hash":"0123456789abcdef0123456789abcdef01234567","size":31461601,"opened_at":"2026-11-12T09:00:05Z","closed_at":"2026-11-12T09:02:10Z","bytes_served":31461601,"first_byte_ms":12,"reads":241,"failed_reads":0,"outcome":"served","file_index":1,"bytes_fetch_block":135393,"bytes_racache_hit":31326208,"bytes_warmup_head":0,"bytes_warmup_tail":0,"swarm_served":true,"duration_ms":125000,"failure_reason":null,"reopens":0,"stalls":0,"max_stall_ms":0,"confirmed":false}
```

Everything after `[Session] ` is one JSON object on one line, the same
object `/api/sessions` returns; there an open session has outcome `open`,
no `closed_at` and live counters, so the byte fields can differ from
`bytes_served` by a read in flight. Parse the JSON; do not grep the path,
which `json.Marshal` escapes (`&`, `<`, `>`). Key order is not part of the
contract. The prefix and the fields below are the
supported contract: fields may be added; none is renamed, retyped or
removed. Ignore fields and outcome values you do not know. The `[TTFF]`
and `[DeadSwarm]` lines are unchanged, and their wording is not a contract.

| Field | Type | Meaning |
|---|---|---|
| `path` | string | The stub's path in the library. |
| `hash` | string | Full info hash, lower-case hex. |
| `file_index` | number | The index the stub carries, as `file_index` in `GET /api/library/list`; 0 if it carries none. |
| `size` | number | File size in bytes. |
| `opened_at`, `closed_at` | string | UTC, RFC 3339, whole seconds. |
| `duration_ms` | number | Open to close, including the idle wait before the close. |
| `first_byte_ms` | number or null | Open to the first byte served; `null` if none was. |
| `bytes_served` | number | Bytes handed to the kernel; the sum of the four fields below. |
| `bytes_fetch_block` | number | Bytes of reads that waited for the engine. |
| `bytes_racache_hit` | number | Bytes of reads answered from the read-ahead cache in memory. |
| `bytes_warmup_head`, `bytes_warmup_tail` | number | Bytes of reads answered from the head or tail window on disk. |
| `swarm_served` | boolean | A read was answered through the engine while a peer was connected: the rule that sets `last_swarm_read_at`. |
| `reads`, `failed_reads` | number | Reads served, and reads that gave up. |
| `failure_reason` | string or null | Why the last failed read gave up (the values of `last_failure_reason`); `null` if no read failed. Set whenever a read failed; only `dead_swarm` says the session counted as a failure. |
| `reopens` | number | Times a read on an open handle woke a torrent the engine had closed as idle. The wake at open does not count. |
| `stalls`, `max_stall_ms` | number | Served reads that took over one second, and the slowest one in milliseconds. Failed reads are not counted. |
| `confirmed` | boolean | A play webhook confirmed the file while the session was open, or it was confirmed when the session started. Sticky: a later stop webhook does not clear it. |
| `outcome` | string | `served`: 1 MiB or more, or over 64 KiB with a read past 8 MiB or in the tail window. `probe`: fewer bytes, a scan. `unserved`: no bytes. `dead_swarm`: the session counted as a failure in the readability record, whatever it served; it never appears when the state database is off. The thresholds may change. |

- The four byte fields name the path that answered each read, not where
  the bytes came from. Bytes fetched from the swarm in a session reappear
  under the cache fields when later reads find them there, and the engine
  can answer a fetch from pieces it already holds. Only `swarm_served`
  says the swarm delivered.
- `served` means the reader got bytes, not that the swarm sent them.
- A session is per file, not per open or per playback. Opens that overlap
  share one. After a session closes, reads on a handle that is still open
  are not counted until the file is opened again. A playback that pauses
  for more than a minute (30 s once the session is older than 30 minutes)
  is cut short in this record, and one whose session ended when another
  handle closed before the first byte shows as `unserved`.
- `reopens`, `stalls` and `max_stall_ms` are lower bounds: a play with long
  gaps spans several sessions, and reads on a handle after its session
  closed are not counted. The process-wide counters
  `flodni_torrent_reopens_total` and `flodni_read_stalls_total` have no
  such gap.
- The `confirmed` label of the three process-wide counters is per torrent: a
  scanner's stall on a sibling episode of a confirmed season pack counts
  `confirmed="true"`, while that session's own `confirmed` is false. The
  label comes from a set rebuilt once a second, so a webhook can show up in
  it a second late.
- Counting edges: a read that waits over a second for another reader's
  fetch counts its own stall; the wake's own duration is not part of the
  stall measure; reopens can exceed idle closes, because two reads in
  different cache chunks can both wake a closed torrent.
- A session still open when Flodni stops gets no line.
- `bytes_served` counts what Flodni handed to the kernel. The kernel's
  page cache answers repeated reads without Flodni seeing them.
- The line carries the library path. Remove it before sharing a log.

## Reader budget (counting always; refusal opt-in, off by default)

Counting is implemented and always on. Refusal is implemented, opt-in and off
by default (`refuse_unconfirmed_reads`). It is tested against a fake engine
and end to end: the `budget-refusal` variant of `make e2e` runs it against the
real mount and engine (below); the parked pump is covered by the fake engine
only. A file that no media server has confirmed as playing can still be read
end to end by a scanner, a hasher or a copy. Flodni counts what such a reader
takes past the head and tail windows and logs the session once. With the
switch off it never refuses or slows a read.

- **What counts.** A served read counts when the engine or the read-ahead
  cache answered it, the bytes lie outside the head window and the tail
  window, and the file has no playback confirmed by a webhook. Reads the
  warmup files answer never count. The unit is the session (overlapping
  opens of one file, ended after 60 s idle).
- **The counter** `flodni_unconfirmed_read_bytes_total` is the bytes the
  reader took past the windows, whichever path served them. It is not bytes
  downloaded: with the pump on (the default), the engine fill happened
  earlier through the pump or a prefetch, and attributing that fill to the
  reader is not built; it is revisited after Reliability step 6.
- **`unconfirmed_read_margin_mb`** in `config.json` (default 64, 0 logs at the
  first byte past the windows; a negative or absurd value is replaced by 64).
  When a session's counted bytes reach the margin, one `[ReaderBudget]` line
  is written with `"event":"over_margin"`: `hash`, `file_index`, `size`,
  `swarm_bytes`, `cache_bytes`, `margin`, `open_age_ms`, `comm`, `pid`, `uid`
  and `enforced` (false unless `refuse_unconfirmed_reads` is on and a
  webhook has matched; see below). It carries no path or file name. The
  line prefix and field names are meant to be stable. `swarm_bytes` and
  `cache_bytes`
  split the counted bytes by the read path that answered, for debugging:
  `swarm_bytes` waited for the engine (which can answer from pieces it
  already holds), `cache_bytes` came from the read-ahead cache. Neither is
  bytes downloaded; with the pump on, almost everything shows as
  `cache_bytes`.
- **Late confirmation.** If a webhook then confirms that file, a second line,
  `"event":"confirmed_late"`, carries `hash`, `file_index`, `size`,
  `swarm_bytes`, `cache_bytes` and `late_ms`: a playback that a budget would
  have cut. `flodni_unconfirmed_read_sessions_total` counts each crossed
  session once, when it closes. The end-to-end test covers this path: the
  `late-confirm` variant of `make e2e` posts a play webhook in the middle of
  a read that has crossed the margin and checks the sha256 of the read,
  `flodni_playback_webhooks_received_total{matched="true"}`, the
  `confirmed_late` session count and the line.
- **`comm`, `pid`, `uid`** come from the FUSE request of the latest open of
  the file. `comm` is read from `/proc/<pid>/comm` when the line is written,
  and is empty when the pid is 0 or not visible (another PID namespace, or
  the process already exited). The opener is not always the process that
  reads.
- **`flodni_playback_webhooks_received_total{matched}`** counts play and
  resume webhooks for a movie or an episode; `matched="true"` when the
  webhook resolved to one file. The refusal gate below arms only once
  `matched="true"` has been seen since start.
- **`refuse_unconfirmed_reads`** in `config.json` (default `false`, read at
  each read). Implemented, opt-in. When on, and once a play webhook has
  matched a file since start, a session that took the margin past the windows
  since the file was last confirmed (or since the start) gets no further
  fetches from the swarm for a file that is not confirmed as playing:
  - **What is refused.** A cache miss for bytes outside the two windows. The
    read waits up to 15 s on its handle for a webhook (a late webhook lifts
    the hold and the read goes ahead); after that, and for every later read
    on that handle, it fails at once with `EIO`. The 15 s is a constant, not
    a key. A short read is never returned (the kernel would cache the zeros),
    and `EAGAIN` makes scanners loop.
  - **What is always served.** A request the read-ahead cache covers in
    full, the SSD head and tail windows, the tail-window fetch, reads wholly
    inside a window, the wake at open, and warm on add and catch-up. A
    request that is only partly cached, or that crosses a window edge, is
    refused whole. The kernel retries page by page, which serves the cached
    pages and fails at the first uncached one. Measured end to end: a reader
    that started 512 KiB before the 20 MiB cut, where the read-ahead cache
    held bytes up to 32 MiB, got exactly the cached 12.5 MiB (the cache hits
    grew by the same 13107200 bytes), half a 1 MiB request past the last
    whole one. When nothing was cached at that offset, the reader got none.
  - **The pump** (`pump_on_open`, or the one started for inferred playback)
    is parked while its next chunk would be refused: its engine reader stays
    open and unread, and it resumes on confirmation. It is never cancelled.
  - **A refusal is not a failed read.** It never reaches the failure count
    or the readability record, so it cannot produce a dead-swarm verdict.
    A short read that ends in a refused sub-read is not counted as a failed
    repair (it still counts as a short read). A caller that leaves while a
    sub-read is held gets `EINTR`, not a failed read; this is the one
    behaviour change with the switch off, for the older `EINTR` exits of a
    sub-read.
  - **After a confirmation or a stop** the tally starts afresh: a play that
    ends does not leave the file refused on the next read.
  - **Log and metrics.** One `"event":"refused"` line per session (`hash`,
    `file_index`, `size`, the counted bytes, `margin`, `offset` of the first
    refused read, `held_ms`, `comm`, `pid`, `uid`, `enforced`), no path.
    `over_margin` reports `"enforced":true` when the key is on and a webhook
    has matched since start. `confirmed_late` gains `refused_reads`.
  - **Overshoot.** The margin is "stops starting fetches", not a byte cap:
    read-ahead already in flight completes, and the parked pump's engine
    reader may fill its own read-ahead. With the pump on, "cached" includes
    what the pump had fetched ahead when the margin was crossed. A miss
    inside the head window fetches a full read-ahead that can run past the
    window edge, and the warm-landing prefetch fetches the whole chunk around
    a window read. Measured end to end (pump off, windows 4 MiB, margin
    16 MiB): a scan got 20 to 32 MiB before the refusal, the 20 MiB cut plus
    up to 12 MiB of read-ahead already in flight, and the engine pulled no
    more from the seeder afterwards (622 bytes in 16 s). With the pump on it
    is not measured.
  - **Limits, check before turning it on.** A webhook later than 15 s, lost,
    or matched to the wrong file breaks that play shortly after the margin
    (a resume on a recently played file with a lost webhook is refused too:
    inferred playback is not exempt). Look at `late_ms` on `confirmed_late`
    lines and at `flodni_playback_webhooks_received_total{matched="false"}`
    first. A media server's full-file analysis (thumbnails, intro detection)
    of an unplayed file will fail past the margin. Reads on a handle after
    its session closed are not budgeted. Held reads occupy FUSE background
    slots for up to 15 s: with `fuse_max_inflight_mb` set, each held read
    keeps about 4 MB checked out (at the 16 MB floor, four held reads stop
    the whole mount for up to 15 s). A hold also keeps the kernel's pages of
    that file locked, so an open of a file with no other live handle waits for
    it (see "Page cache on a second open"). A hold adds up
    to 15 s to one read on top of the slot wait and fetch bounds, and can
    outlast the 4 s refill budget.
  - **More limits.** The tally is per file across opens: a player that opens
    a file a scanner just pushed past the margin has no margin left, only the
    15 s hold, and every new open of a refused file waits 15 s again. A stop
    webhook un-confirms the file for every reader, so a second client, or a
    stop that arrives after the next play, is cut after margin plus hold.
    Windows are exempt by byte, not by page: the tail window starts on a 4 KiB
    boundary, so the page that straddles the file's size minus the tail size is
    inside it, but kernel read-ahead across the head edge can stall a head read
    15 s once per open. A parked pump resumes when its session closes (no session, no
    refusal); for inferred playback it can then fetch up to the read-ahead
    budget once.
- **End-to-end test.** The `budget-refusal` variant (margin 16 MiB, windows
  4 MiB, `pump_on_open` off, auth off) reads 40 MiB from the middle of the
  fixture with no matched webhook (not armed: it passes), arms the gate with
  a play and a stop, and then: a `dd` of the file fails with `EIO` after at
  least the windows plus the margin and far below the file size, with one
  `refused` line (`"enforced":true`, `held_ms` near 15000, no file name) and
  no failed read in `/api/sessions`, in
  `flodni_fuse_short_read_results_total{result="failed"}` or the readability
  record; the seeder is asked for nothing more; page-aligned reads inside
  both windows still succeed; while a reader is held, `stat`, the reads of a
  descriptor opened before the hold, and fresh opens reading a head and a
  tail window (buffered and `O_DIRECT`) each answer in under 1 s (since the
  page-cache fix above); a reader killed mid-hold is not a failed read;
  a play that arrives 5 s into a held full read lifts the hold (the sha256
  matches, `holds_total{result="lifted"}` and
  `confirmed_after_refusal_total` move); after a play and a stop a new
  reader gets the margin again before it is refused; and when the session
  closes there is no `[DeadSwarm]` line. It reads the whole file only once,
  held, because re-fetching pieces the engine's 128 MiB cache already
  evicted stalls the engine for tens of seconds (the stall the other
  variants report) and would show as a timeout, not a refusal. Not covered
  end to end: the pump park with `pump_on_open` on.
- **Refusal series (all start at 0 and move only with the switch on):**
  `flodni_unconfirmed_read_refused_reads_total`,
  `flodni_unconfirmed_read_holds_total{result="lifted|refused"}` and
  `flodni_unconfirmed_read_confirmed_after_refusal_total`.

Limits: reads on a handle after its session closed and sessions open at
shutdown are not counted; reads the kernel page cache answers never reach
Flodni; a fuzzy webhook match or a state restored at start can mark a file
confirmed that is being scanned. A confirmation also outlives the playback:
it stays until a stop webhook, or until 15 minutes after the last handle of
the file closes, and a scan in that window is not counted.

## Warmup cache eviction

With the warmup cache on, a new head file that would pass the quota evicts
whole media files, head and tail together. Files never played go first, then
played files; each class goes oldest written first. This is built and runs
today. The cache also keeps an in-memory list of which files are warm; it is
internal and not served, and only the aggregates below are exported
(`flodni_warmup_files`). What is not built: evicting the least recently read
file (an idea in `docs/ideas.md`, not planned). Warm on add and its catch-up,
which fill windows on their own, are described in their own section below; both
are off by default.

- **Played** means a playback confirmed by Plex's play webhook, never a
  scanner's read. A webhook marks at most one file: a movie by file name,
  IMDB id or a unique title (and year); an episode by file name, or by a
  unique IMDB id that agrees with the file's season and episode, or by series
  title, season and episode together (`Metadata.parentIndex` and
  `Metadata.index`). When several candidates fit equally, or an episode
  webhook carries no season and episode and no file name, no file is marked:
  a missed play costs one file its protection, a wrong mark would protect a
  season. Marking does not change which file the webhook starts playback
  for.
- **Protection expires.** A play protects its file for `played_protection_days`
  (default 90) counted from the last play; after that the file ranks as never
  played. `0` means a play protects forever. A negative value is logged at
  load and replaced by 90.
- **Limits.** Without a webhook nothing is ever played, so the order is
  oldest first. With the state database off (`enable_state_db`) the marks
  live in memory and are gone at restart. A play whose webhook arrives
  before the file is opened is not marked. When only played files remain,
  any read through the mount can evict the oldest played file, because a
  play's first reads cannot be told from a scan before the webhook arrives.
- **State.** The marks are in the state database table `file_state` (schema
  13): the last play and whether the quota evicted the file. Schema 14 adds
  the experimental per-file record (see "Per-file records") to the same
  rows; schema 15 adds `daemon_marks`, three daemon times (see "Play
  webhook"); schema 16 adds `queued_at` (see "Scope" under catch-up).
  Eviction removes bytes only; the readability record stays. A torrent
  removed from the library loses its marks, and a startup prune after a
  complete library scan drops the marks of torrents that left it.
- **Log line.** Each evicted file logs one line, `[WarmupEvict] ` followed by
  a JSON object: `hash` (full), `file` (index), `head_bytes`, `tail_bytes`
  (0 when that window was not on disk), `age_s` (since the newer window was
  written), `played` (inside its protection) and `trigger` (`head_create`,
  `warm_add` for an eviction made to fit a warm-on-add fill, or `request` for
  one made by `POST /api/warmup/evict`). It carries no path. The prefix and
  field names are meant to be stable.
- **Free space on request.** `POST /api/warmup/evict` with the body
  `{"target_gb": 10}` (or `{"target_bytes": 10737418240}`; give exactly one,
  not negative; `0` empties what may be evicted) evicts down to that size now,
  in the same order as the quota: files never played first, then played files,
  each oldest first. It answers `{"evicted_files": N, "evicted_bytes": N,
  "total_bytes_after": N, "target_bytes": N}`; a target at or above the
  current total evicts nothing. `400` is a bad target, `405` is any method
  but POST, `503` is the cache off. **It is destructive:** it deletes warmup
  cache files (a file that was played goes too once the unplayed ones are
  not enough), which then come back only through the swarm or a refill. It
  needs the `api_token` bearer header when a token is set (no `?token=`),
  deletes only inside the cache directory, and touches no torrent, stub or
  record except the eviction marks a quota eviction also writes. Each file
  logs the `[WarmupEvict]` line with `trigger` `request`, and the eviction
  counters move as for a quota eviction. The configured quota is not
  changed: the next new head still evicts down to it.
- **Cold reopen.** `flodni_warmup_cold_reopen_total` counts a confirmed play
  on a session that opened with no ready head, of a file with an eviction
  mark; it counts once per eviction and clears the mark. `previously_played`
  says whether the file had been played before, expired or not.
  `flodni_playback_starts_total` counts the same confirmed starts (not
  playing to playing), the denominator for the rate. A start whose webhook
  resolves to no single file is still counted, but its cold reopen is not,
  so the rate is a lower bound. Neither counts a play whose webhook matches
  no open file.

### Cache disk

With `warmup_dir` on its own disk, make the disk part of the start. This is
the deployment side; Flodni cannot tell an unmounted disk from an empty
directory.

- Make the start depend on the mount: `RequiresMountsFor=` in the systemd
  unit, or a Compose bind mount without `create_host_path` (so Docker fails
  instead of creating the path on the host).
- Keep the empty mountpoint unwritable (`chattr +i` on it while nothing is
  mounted), so a create under it fails.
- Use a journalling filesystem (ext4 or XFS). Heads are not synced, and FAT,
  exFAT and NTFS can expose bytes that were extended but never written after
  an unclean unplug.
- Restart Flodni after the disk returns. A directory that vanished at runtime
  is not re-created, and the totals are not rebuilt without a restart.
- Never delete `.dense-migrated` by hand. A missing marker next to a populated
  cache now checks every head for holes at the next start and drops only
  those (one WARNING line with the counts, and each dropped head named), so a
  deleted marker costs a scan, not the cache; a cache that has the marker is
  not checked. Copy a cache without sparse options (no `rsync -S`, no
  `cp --sparse=always`), and keep the marker with the copy: a head with a
  stored run of zeros reads as a hole. ZFS with compression does the same
  (it stores all-zero records as holes), so with a missing marker it can drop
  heads that hold long zero runs.

What Flodni does in each case:

- **Absent at start.** It creates the directory and runs on whatever
  filesystem holds the path, with a WARNING `warmup_dir is not a separate
  mount` when the directory already existed on its parent's filesystem. That
  is only a hint, since a plain directory looks the same, and it does not
  refuse to start. If the directory cannot be created, it logs a WARNING and
  runs with no cache.
- **Vanished at runtime.** Nothing re-creates it. Head and tail writes and
  fills fail and log, reads miss and go to the engine, and the quota walk
  keeps its old total instead of storing 0. A hung filesystem can still block
  reads; there is no timeout.
- **Read-only.** Heads already there are served (one WARNING, once); nothing
  new is written, and a file the quota cannot unlink is neither evicted nor
  marked evicted.
- **Full.** Failed creates and writes are logged, nothing false is recorded,
  and a fill reports that the file was evicted.

## Removing a title

`POST /api/library/remove` takes a `path` or a `hash`. A hash removes every
stub whose own link carries that full 40-hex hash, whatever its file name: the
eight digits older tools put in the name (first eight for episodes, last eight
for movies) are not used, so a stub of another torrent that shares them is
left alone. The call reads each stub under both folders. A stub with no
readable hash is skipped and logged once per call. The torrent is dropped once
no stub links to it.

## Warm on add

**Experimental, off by default.** With `warm_on_add` set (and `warmup_dir`
set, see below), Flodni completes the head and tail windows of each file the
library API files (`POST /api/library/add`), in the background, so the first
play starts from the SSD instead of waiting for the swarm. It is implemented
and tested against fakes and, for one title, end to end: `make e2e` adds a
fixture with the switch on, waits for the fill, stops the seeder and reads both
windows back from the SSD. Pause, the wake of a closed torrent and removal
while queued are covered by unit tests only (the `catch-up` variant does wake a
closed torrent and reports the engine's state for it before the pass).

- **Cost.** It downloads the windows (by default 64 MiB of head and 16 MiB of
  tail per file, `warmup_head_size_mb` and `warmup_tail_size_mb`; the tail
  window is page-aligned: its start is rounded down to a 4 KiB boundary, so it
  is up to 4095 bytes longer, and a read of exactly the last N MiB of a file
  stays inside it) from the
  swarm for every file of every title added, played or not. A private tracker
  may count that download against the account; that is why it is off by
  default. A pack added episode by episode warms each episode. Catch-up (below)
  does the same for the titles already in the library, until the cache is full
  (about 190 files at the default 15 GB at a time; room freed by evictions is
  filled again), so it can cost a private tracker one download per not-warm
  title.
- **Switch.** `warm_on_add` is read at each add, so a change saved in the
  control panel applies to the next add. Switching it off drops the queue and
  stops a running fill at its next check. It works only with `warmup_dir` set
  (index mode, where nothing reaches the SSD before its pieces pass their
  checksum). With `warm_on_add` set and `warmup_dir` empty, Flodni logs one
  warning and warms nothing.
- **What it does.** An add that wrote stubs hands the new files to an
  in-memory queue (4096 files; over that a file is dropped and counted as
  `dropped_queue_full`). Two workers take one title at a time and fill each
  file's windows in 4 MiB chunks, writing only verified bytes. A restart
  forgets the queue; catch-up (below) feeds the files that were not warmed yet
  in a later pass.
- **Pause.** While a read-ahead pump runs, or a playback the media server
  confirmed (or that the read pattern shows) still has an open file, the
  warmer stops. A running fill stops at its next check, up to one fetch (8 s)
  later; the file goes back to the front of the queue and nothing is
  recorded. It resumes after playback was clear on two checks 5 s apart.
- **Removal.** Removing the torrent cancels its queued files and stops a
  running fill; a later add of the same hash starts again. A fill that
  outlived the removal has its cache files removed once more.
- **Room.** It checks for room before it fetches. It may evict only files
  never played, oldest first, and only when the state database is on and at
  least one play has been recorded. Without that evidence it evicts nothing:
  a file with no room is dropped (`cache: dropped_quota` or
  `dropped_no_evidence`) and counted as `no_room`.
- **Closed torrents.** A torrent the engine closed is woken from its hash
  alone, and only if the engine still stores it: the stored record brings
  back the metainfo, the private flag and the trackers it was added with, so
  a private torrent gets no DHT, PEX or public tracker from the wake. A hash
  the engine does not store, or stores without its metainfo, is refused: the
  job is skipped, counted as `wake_error`, and nothing is recorded. The check
  and the load of the record are one step against a removal, so a torrent
  removed meanwhile does not come back (see "A stub the engine does not
  store").
- **Outcomes.** One per file attempt, the `outcome` label of
  `flodni_warm_fills_total{trigger="add"}` and `{trigger="catchup"}`:

| Outcome | `readability` | `cache` | Per-torrent record |
|---|---|---|---|
| `warmed`: windows complete and a swarm delivery time is known | `warmed` | `cached` | `last_swarm_read_at` set to the delivery time, failure count reset |
| `present`: complete, no delivery time | `present` | `cached` | untouched |
| `already_warm`: the windows were complete | unchanged (`warmed` if this file's own earlier attempt delivered) | `cached` | stamped only in that case |
| `no_room` | unchanged | `dropped_quota` or `dropped_no_evidence`; catch-up records nothing | untouched |
| `failed`: a chunk did not verify in time | `failed`, `failed_attempts` +1 | unchanged | untouched |
| `evicted`: quota eviction took the windows during the fill | unchanged | `evicted`, not retried | untouched |
| `lost`: the windows vanished with no eviction mark | unchanged (`warmed` if a delivery was seen) | `lost` | stamped if `warmed` |
| `write_failed`: another error, such as a failed write | as `lost` | `write_failed` | as `lost` |
| `index_mismatch`: the engine's file at the stub's index has another length than the stub | unchanged | unchanged, not retried; only the attempt time is recorded (catch-up looks again after 20 hours) | untouched |
| `wake_error`, `dropped_queue_full` | nothing recorded | | |
| `skipped`: a file left behind after a `wake_error`, after a `failed` with no peers or no running torrent, or (catch-up) after a `no_room` of the same title | nothing recorded | | |

- **Delivery time.** The time kept is the latest swarm delivery seen across
  the attempts of a file, so a fill paused at 20:00 and finished at 23:00 from
  RAM carries 20:00. If a media server scan filled the windows from the swarm
  first, that scan recorded its own delivery, and an `already_warm` fill adds
  nothing.
- **Retries.** `lost` and `write_failed` are retried from memory after 10
  minutes, 1 hour and 6 hours, then left alone
  (`flodni_warm_cache_failures_total` makes a failing disk visible); catch-up
  tries such a file again after 20 hours. After a
  timeout with no peers, or a torrent the engine did not have running, the rest
  of that title's files are skipped without a record; warm on add does not
  retry them, and catch-up (below) picks them up in a later pass.
- **Log line.** Each attempt logs one line, `[Warm] ` followed by a JSON
  object: `hash` (full), `file` (index), `trigger` (`add` or `catchup`),
  `outcome` (also `stopped` and `cancelled` for fills that recorded nothing),
  `cache`, `reason`, `head_bytes`, `tail_bytes`, `delivered_pieces`, `delivered_at`,
  `evicted` (files evicted for room) and `duration_ms`. It carries no path.
- **Catch-up.** Experimental, tested against fakes and, for one title, end to
  end (`make e2e`, variant `catch-up`: off, then on after a restart, then a
  second restart that feeds nothing, then the windows read back with the seeder
  dead). The quota stop, the exclusion of evicted files, the overwrite of a
  stale eviction mark and the 20-hour rule are covered by unit tests only. It
  warms the titles that are already in the library: the
  ones filed before `warm_on_add` was on, and the ones whose warm failed or was
  dropped. It runs only with `warm_on_add` on, `warmup_dir` set and the state
  database attached (without the database an eviction mark would not survive a
  restart). With `warm_on_add` off it does nothing; with it on and `warmup_dir`
  or the database missing, Flodni logs one warning and does nothing. The first pass
  starts one interval after the daemon starts (3 minutes at the default
  pace), then a pass starts 6 hours after the previous one ended, from a fresh
  walk of the stubs; two passes never run at once.
  - **Pace.** `warm_catchup_per_hour` (default 20) is the files started per
    hour, read on each feed, so a change in the control panel applies to a
    running pass. `0` turns catch-up and its day-scale retries off; a negative
    value is logged and replaced by 20. A pace raised from 0, or `warm_on_add`
    switched on, takes effect at the next pass: up to 6 hours later, or at a
    restart. A pass feeds one title at a time, newest stub first, only while
    the warmer is idle, so it holds at most one of the two workers: an add is
    never queued behind a backlog, and a playback pauses it like any fill.
  - **Scope.** `warm_catchup_scope` is `"all"` (the default; the rule below
    unchanged) or `"retries"`. Use `"retries"` with `warm_on_add` on and no
    backlog to walk: catch-up then feeds only files warm on add attempted
    and did not finish, so a failure there is tried again and the titles
    filed before warm on add was on are left alone. The files are: a failed
    read (the same 20-hour wait and 4 failed attempts as below);
    `dropped_quota`, `dropped_no_evidence`, `lost` and `write_failed`; and a
    file warm on add queued over an hour ago with no outcome recorded, which
    covers a restart in the middle of a warm, a wake error and a queue that
    was full (`file_state.queued_at`, schema 16: written when warm on add
    queues the file, cleared when an attempt records any outcome, and read
    after a restart). A file never attempted by warm on add (no record, or
    only `cached`), an evicted file and a file warm on disk are never fed,
    and the gauge counts the never-attempted ones as `out_of_scope`. The
    rest of the rules below (20 hours, the cap, never evicting, the quota
    stop) apply in both scopes. An unknown value is logged and replaced by
    `"all"`. Evidence does not change with the scope: a retried read the
    swarm served, failed or not, counts as it does today, and
    `dropped_quota`, `write_failed`, `lost` and a refused wake never do and
    never touch the readability or the title's streak. The pass logs its
    scope (`"scope"` in both `[WarmCatchup]` lines) and
    `flodni_warm_catchup_scope{scope}` says which one the last pass ran.
    Recommended pace for `"retries"`: `warm_catchup_per_hour` 6. The
    retries are few, a handful of files a day, and each one wakes a torrent
    and reads a swarm that already failed or a cache that was full, so
    there is nothing to hurry; at 6 an hour the first pass starts 10 minutes
    after the daemon starts. The default 20 is sized for walking a backlog.
  - **Which files.** A file is skipped when both windows are complete on disk
    (the disk decides, not the record; a record without `cache: cached` is
    written, with no wake). It is never warmed when its `cache` is `evicted`,
    or it carries an eviction mark or time, including a file evicted before
    schema 14: eviction is left to its next play. A file that failed 4 times
    is given up on; only a read through the mount that the swarm serves, or an
    add again, makes it eligible. A file attempted under 20 hours ago waits,
    and so does its whole title when any file of it failed under 20 hours ago
    (a no-peers failure skips the rest of a pack without a record). A title the
    engine refused to wake (`not_stored`, for example a stub the engine never
    stored) waits for a play. Everything else is a candidate: no record,
    `dropped_quota`, `dropped_no_evidence`, `lost`, `write_failed`, and a
    record that says `cached` while the disk does not.
  - **Never evicts.** It checks for room before it wakes anything. At a full
    cache the pass ends: the file is counted `no_room`, nothing is recorded, and
    `flodni_warm_catchup_files{state="left_no_room"}` holds what the pass left.
    A pass also ends after three cache-side failures (`lost`, `write_failed`)
    of its own files.
  - **Retries.** There is no new state: the 20 hours and the 4 failed
    attempts are computed from `last_attempt_at` and `failed_attempts`, so the
    6-hour pass picks a failed file up once its 20 hours are over. The
    in-memory retries of a cache-side loss (10 minutes, 1 hour, 6 hours) stay;
    after them only the 20-hour wait applies, with no cap.
  - **Closed torrents.** It wakes only through the same check as warm on add
    (the engine must still store the torrent), never from the stub's magnet.
  - **Log.** Its attempts log `[Warm]` lines with `"trigger":"catchup"`, and a
    pass logs one `[WarmCatchup]` JSON line at its start and one at its end,
    with counts only (no path).
- **Metrics.** `flodni_warm_fills_total{trigger,outcome}` (`add` and
  `catchup`, every outcome from 0),
  `flodni_warm_fill_bytes_total{trigger,window}`,
  `flodni_warm_cache_failures_total{reason}`, `flodni_warm_queue_files`,
  `flodni_warm_paused` and `flodni_warm_catchup_files{state}`, the backlog as
  of the last pass: `pending` (also what a pass left because it was stopped, the
  pace was set to 0 or its disk failed), `retry_wait`, `gave_up`, `evicted`,
  `not_stored`, `out_of_scope` (scope `retries` only) and `left_no_room`.
  Already warm files and files a pass found no longer candidates are not
  counted. `flodni_warm_catchup_scope{scope}` says which scope the last pass
  ran.
- **Limits.** The queue and the retries are in memory. The eviction time is
  kept in whole seconds. Without the state database the add warmer still fills,
  evicts nothing, and its records stay in memory and are not served; catch-up
  does not run. A wake that fails for a passing reason (the engine busy) also
  skips the title; catch-up feeds it again in a later pass, once its 20 hours
  are over if an attempt was recorded. A title removed and added again while
  `warm_on_add` was off keeps its removal mark until a restart: catch-up lists
  it as `pending` every pass and feeds nothing.

### A stub the engine does not store

A closed torrent is woken from the engine's stored record only, and a hash
with no record, or a record without the metainfo, is refused. This is the
strict wake (`docs/roadmap.md`, step 1): a wake from a bare magnet would add
the torrent with the DHT and the public trackers and no private flag known
until the metainfo arrived (hard rule 3). Such a stub, from a library add made
before the engine saved it, a lost `config.db` or a raw engine `rem` with the
stub left, stops opening until it is added again. At startup the daemon logs
one `WARNING: N of M library torrents have no stored engine record with
metainfo` line, naming the first five hashes, and exports
`flodni_library_torrents_unstored` (counted once, after the library walk), so
read it before deploying the strict wake.

What a refusal looks like:

- An open of the stub answers `EIO` (`ENOENT` if the stub is already gone
  from disk) and logs `[VFS] wake refused at open ...`.
- A read on a handle that is already open, whose torrent the engine closed and
  then does not store, answers `EIO` with no fetch and no engine call after the
  first; the handle stays refused until it is reopened.
- Neither is a dead swarm. No failed read is marked and no dead-swarm verdict
  follows. The readability record gets its own reason, `not_stored` (see
  "Readability record"), and `flodni_wake_refused_total` counts it.

The library adds the engine now saves at once (`save_to_db`), so only existing
stubs are affected. To repair one, add the hash to the engine with
`save_to_db` set (`POST /torrents` with
`{"action":"add","link":"magnet:?xt=urn:btih:<hash>","save_to_db":true}` on
the engine port; an explicit add, so the link's trackers are used and a
private torrent needs its `.torrent` instead), or run a library remove and
then a library add. A library add alone answers "already present" and changes
nothing.

Every path that starts a torrent for a hash, labelled:

| Path | Kind | What it does |
|---|---|---|
| A read or an open through the mount, warm on add, catch-up | wake | `native.Wake` loads the stored record (`torr.LoadStored`); the magnet is used for its hash only, and a link that is not a magnet is refused without being fetched. Refuses an unstored hash. |
| `/play/{hash}/{id}`, `/stream` with a stored hash, and the engine's own lookup of a stored torrent | wake | `torr.LoadStored`. `/play` answers 404 for an unstored hash. |
| `POST /torrents` (`add`), upload, `/stream?link=` on a new link | explicit add | The caller's link or file starts the torrent, a stored record or not; the stored metainfo and trackers are merged in when there is one. |

A load, a save and a removal of one torrent are serialised in the engine, so a
torrent that was removed stays removed: a wake that was still adding it
finishes first and the removal then takes it, and a save by an idle close that
lost the race writes no record. The one case left is an explicit add of the
same hash after the removal, which is a new add. This is unit-tested under the
race detector, and end to end: the `removal` variant of `make e2e` removes
three titles back to back (two through the library API, one with `rm`) while a
fourth is read and a descriptor is open on one of them.

### A stub whose file index is stale

A stub names its file by the index the engine gave it when the stub was
written. Flodni writes the index and the size from the same engine record, so
its own stubs agree with the engine. A stub from elsewhere, or an engine that
numbered a pack's files another way, can leave the index naming a different
file. The ids are the engine's own (the list saved with the torrent's record
and `Status()` serve the same ids): not the order of the metainfo and not a
sort of the paths. An open compares the stub with that list by one rule: the
stub's index wins when the engine's file there has the stub's length; else the
file of that length (by name if several); else the open fails with `EIO`.

- **The counter.** `flodni_stub_index_mismatch_total` counts the paths whose
  stub index differs from the file an open resolved, or names no file of the
  stub's length, once per path per process. Each first open logs one
  `WARNING: stub index:` line naming the path. It is 0 from the start. A
  non-zero value means a stale stub exists; zero over a few weeks of use means
  none of the files opened has one.
- **To repair a stub:** run a library remove and then a library add of the
  title. A library add alone answers "already present" and changes nothing.
- **What is guarded.** The open reads the engine's list from the engine when
  the torrent is loaded, else from the record saved with it, without waking the
  torrent. A warm head under the stub's index is used only when the file there
  has the stub's length; otherwise the open is cold (the torrent is woken
  first) and resolves by the rule above. When no file has the stub's length the
  open fails with `EIO` and the counter moves: nothing is served for a stub
  that cannot be matched. A record saved before the metainfo has no list; then
  an open takes a warm head only if it is no longer than the stub's file. Warm
  on add and catch-up refuse a file whose engine length differs from the
  stub's size (outcome `index_mismatch`). Not guarded: a torrent with no
  stored list whose stale stub shares its index with another file's warm head
  of a plausible length (`docs/ideas.md`).

## Metrics

`GET /metrics/prometheus` on the control port serves the Prometheus text
format 0.0.4, next to the JSON at `/metrics` that the control panel reads.
It follows the token rule of the other control routes: open when no token is
set, the bearer header only when one is (no `?token=`). It answers `GET`
only, with 405 and `Allow: GET` otherwise. A scrape reads in-memory counters
and never calls the torrent engine.

```yaml
scrape_configs:
  - job_name: flodni
    metrics_path: /metrics/prometheus
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/flodni-token
    static_configs:
      - targets: ["flodni:9080"]
```

Drop `authorization` when no token is set. The port has no TLS, so keep the
scraper on the same host or Docker network.

| Series | Type | Labels | Meaning |
|---|---|---|---|
| `flodni_build_info` | gauge | `version` | Always 1. |
| `process_start_time_seconds` | gauge | none | Start of the process. |
| `flodni_session_first_byte_seconds` | histogram | none | Open to the first byte served, per real playback. |
| `flodni_session_first_deep_read_seconds` | histogram | none | Open to the first read at 8 MiB or later or in the tail, per real playback. |
| `flodni_seek_read_duration_seconds` | histogram | none | Duration of the read that followed a seek. |
| `flodni_read_duration_seconds` | histogram | `source` | Duration of each served read. Its `_count` is the number of reads each path answered. |
| `flodni_read_bytes_total` | counter | `source` | Bytes handed to the kernel. |
| `flodni_sessions_closed_total` | counter | `outcome` | Closed sessions: `served`, `probe`, `unserved`, `dead_swarm`. |
| `flodni_dead_swarm_verdicts_total` | counter | `reason`, `source` | Dead-swarm verdicts; `reason` as `last_failure_reason`, `source` is `mount`. |
| `flodni_warmup_cache_bytes` | gauge | none | Approximate size of the warmup cache; only with the cache enabled. |
| `flodni_warmup_cache_quota_bytes` | gauge | none | Its size limit; only with the cache enabled. |
| `flodni_warmup_evictions_total` | counter | `window` | Files removed by quota eviction, `head` or `tail`. |
| `flodni_warmup_evicted_bytes_total` | counter | `window` | Their apparent size in bytes. |
| `flodni_warm_fills_total` | counter | `trigger`, `outcome` | Warm fills. `trigger="mount"`: per-file records the mount path wrote, `outcome` `warmed` (the engine holds a swarm delivery stamp) or `cached` (it does not); needs the state database and index mode. `trigger="add"` and `trigger="catchup"`: warm on add and catch-up attempts, `outcome` as in "Warm on add", every value present from 0. |
| `flodni_warm_fill_bytes_total` | counter | `trigger`, `window` | Bytes warm on add and catch-up wrote, `window` `head` or `tail`, also by fills that did not finish. |
| `flodni_warm_cache_failures_total` | counter | `reason` | Warm on add and catch-up attempts that lost a window or could not write it: `lost` or `write_failed`. |
| `flodni_warm_queue_files` | gauge | none | Files waiting for warm on add or catch-up. |
| `flodni_warm_paused` | gauge | none | 1 while warm on add and catch-up are paused for a playback. |
| `flodni_warm_catchup_files` | gauge | `state` | Library files catch-up has not warmed, as of the last pass: `pending`, `retry_wait`, `gave_up`, `evicted`, `not_stored`, `out_of_scope`, `left_no_room`; every state present from 0. See "Warm on add". |
| `flodni_warm_catchup_scope` | gauge | `scope` | The `warm_catchup_scope` the last catch-up pass ran with: 1 for `all` or `retries`, 0 for the other; both 0 before the first pass. |
| `flodni_warmup_files` | gauge | `window` | Media files with a warm window on disk now: `head` counts heads at or above the 1 MiB ready floor, `tail` counts tails fully covered. Only with the cache enabled. |
| `flodni_playback_starts_total` | counter | none | Playbacks the media server webhook confirmed (not playing to playing). |
| `flodni_torrent_idle_closes_total` | counter | `confirmed` | Torrents the engine closed because no reader had them for the idle timeout. `confirmed` is `true` when a file of the torrent had a webhook-confirmed playback; the label is refreshed once a second. Both values present from 0. With the keep-alive on, a torrent it holds is not counted; see Keep-alive. |
| `flodni_torrent_reopens_total` | counter | `confirmed` | Closed torrents a read on an open handle woke again; the wake at open is not counted. `confirmed` as above. |
| `flodni_wake_refused_total` | counter | none | Wakes the engine refused because it keeps no record of the torrent: one per handle at a read, one per open. Not a failed read and not a dead swarm. 0 from start. See "A stub the engine does not store". |
| `flodni_read_stalls_total` | counter | `confirmed` | Served reads that took over one second; failed reads are not counted. `confirmed` as above. Independent of sessions. |
| `flodni_keepalive_releases_total` | counter | `reason` | Holds that ended: `stop` (a stop webhook, or the playback state dropped) or `ceiling` (`playback_keepalive_minutes` after the last activity). The torrent then closes at its next idle check. Both values present from 0. See Keep-alive. |
| `flodni_keepalive_evicted_total` | counter | none | Confirmed torrents that lost their hold to `playback_keepalive_max_torrents`, once per episode. Present from 0. |
| `flodni_keepalive_held_torrents` | gauge | none | Idle torrents the keep-alive is keeping open now (a torrent with a reader is not counted). |
| `flodni_playback_webhooks_received_total` | counter | `matched` | Play and resume webhooks for a movie or an episode; `matched` is `true` when the webhook resolved to one file, else `false`. |
| `flodni_playback_webhook_match_rule_total` | counter | `rule` | The same webhooks by the rule that resolved them: `basename`, `path_map`, `imdb`, `fuzzy` or `none`; every value present from 0. See Play webhook. |
| `flodni_playback_webhook_last_timestamp_seconds` | gauge | `matched` | Unix time of the last play or resume webhook for a movie or an episode, `matched` as above; 0 = none seen. Persisted hourly with the state database. See Play webhook. |
| `flodni_unconfirmed_read_last_session_timestamp_seconds` | gauge | none | Unix time of the last session that crossed `unconfirmed_read_margin_mb` and closed unconfirmed; 0 = none seen. |
| `flodni_warning` | gauge | `name` | 1 while an advisory warning holds, else 0. `no_play_webhook`: see Play webhook. Never affects `/api/health` status. |
| `flodni_unconfirmed_read_bytes_total` | counter | none | Bytes past the head and tail windows that readers took from files with no confirmed playback, whichever path served them. Not bytes downloaded. See Reader budget. |
| `flodni_unconfirmed_read_sessions_total` | counter | `outcome` | Sessions that crossed `unconfirmed_read_margin_mb`, each counted once when it closes: `confirmed_late` if a webhook confirmed the file afterwards, else `unconfirmed`. |
| `flodni_unconfirmed_read_refused_reads_total` | counter | none | Reads the reader budget refused with EIO. Moves only with `refuse_unconfirmed_reads` on. See Reader budget. |
| `flodni_unconfirmed_read_holds_total` | counter | `result` | Holds of a refused handle, when they end: `lifted` by a confirmation (or the switch going off), `refused` when the time ran out. Moves only with `refuse_unconfirmed_reads` on. |
| `flodni_unconfirmed_read_confirmed_after_refusal_total` | counter | none | Sessions with a refused read that a webhook then confirmed, once each (plays the budget would have broken). Moves only with `refuse_unconfirmed_reads` on. |
| `flodni_warmup_cold_reopen_total` | counter | `previously_played` | Confirmed plays that opened cold on a file quota eviction had removed, `true` or `false` by whether it had been played before. |
| `flodni_warmup_unverified_drops_total` | counter | none | Warmup blocks, head or tail, never written because their pieces did not verify in time. |
| `flodni_warmup_write_queue_drops_total` | counter | none | Head chunks dropped because the warmup write queue was full. |
| `flodni_readability_torrents` | gauge | `state` | Library torrents by readability: `failed` has a failure streak, `ok` has none and a read served by the swarm, `never_read` is the rest (no record, or a streak reset by a metainfo without a read). Not exported when the state database is off. |
| `flodni_library_torrents_unstored` | gauge | none | Library torrents with no stored engine record holding metainfo, counted once at startup after the library walk and not refreshed. Not exported until counted. See "A stub the engine does not store". |
| `flodni_stub_index_mismatch_total` | counter | none | Library paths whose stub file index differs from the file an open resolved by size and name, once per path per process. Non-zero means a stale stub. See "A stub whose file index is stale". |
| `flodni_readahead_cache_bytes` | gauge | none | Bytes in the read-ahead cache. |
| `flodni_readahead_cache_active_bytes` | gauge | none | The part that is not stale. |
| `flodni_readahead_cache_entries` | gauge | none | Entries in the cache. |
| `flodni_readahead_cache_budget_bytes` | gauge | none | The cache's size budget. |
| `flodni_fuse_short_reads_total` | counter | none | Reads served with fewer bytes than requested away from EOF. |
| `flodni_fuse_short_read_results_total` | counter | `result` | Outcome of the refill: `repaired` or `failed`. |
| `flodni_fetch_dedup_total` | counter | none | Concurrent fetches of one chunk that shared a single fetch. |

`source` is `warmup_head`, `warmup_tail`, `racache_hit` or `fetch_block`.
Every label value is exported at 0 from the start. Durations are in seconds
and the buckets are fixed (1 ms to 10 s). Series names and labels are meant
to be stable; buckets and help text are not.

- Counters reset when Flodni restarts; use `rate()` or `increase()`.
- `flodni_session_first_byte_seconds` and
  `flodni_session_first_deep_read_seconds` cover only sessions that reach
  the `served` thresholds above, not scanner probes, and a session counts
  when it closes. `flodni_seek_read_duration_seconds` is recorded at the
  read itself in any open session, scanner probes included.
- `source` names the path that answered a read, not where the bytes came
  from: a `racache_hit` may hold bytes the swarm delivered a moment ago.
- The dead-swarm series stay 0 when the state database is off; nothing is
  recorded then.
- Durations are truncated to whole milliseconds: reads under 1 ms land in
  `le="0.001"` and add nothing to `_sum`.
- `flodni_read_bytes_total` counts every served read, including reads after
  a session closed and reads with no session, so it differs from the
  `[Session]` lines.
- `flodni_warmup_cache_bytes` is a running total, resynced from the disk on
  each eviction walk. It can drift in between; the known causes are in
  `docs/ideas.md`.
- `flodni_warmup_files` reads the cache's in-memory index. A head removed
  behind the cache's back shows until the next lookup of it, a tail until
  the next restart. Warm bytes are `flodni_warmup_cache_bytes`. The series is a count of files, not of
  complete media files: a head counts once it holds 1 MiB, whatever the
  head window size.
- `flodni_readability_torrents` is recomputed every 60 s, so a scrape stays
  in memory. The library size is the number of distinct torrents
  looked up through the mount, so a new torrent counts once it appears
  there. Records of torrents that left the library are not subtracted (a
  startup prune removes them), and `never_read` stops at 0 rather than
  going negative.
- Engine and peer counters, read-ahead evictions and Go runtime metrics are
  not exported yet (`docs/ideas.md`).

## Viewing the model

`make check` validates the architecture model and `make view` serves it at
<http://localhost:8080/workspace/1>. Both need Docker. `make docs` checks
that the documentation agrees with the tree.

## Contributing

Pull requests are opened by the maintainer only. Issues are welcome; see
[CONTRIBUTING.md](CONTRIBUTING.md).

## Licence

GPL-3.0, see [LICENSE.md](LICENSE.md). Tiramisu is copyright Matteo
Rancilio; third-party components are listed in [NOTICE](NOTICE).
