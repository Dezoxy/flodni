# 7. Stay on Flodni's engine; do not move to upstream's Tentacle

Date: 2026-11-23

## Status

Accepted on 2026-11-23 by the owner. Nothing in the code changes. The finding
comes from a code read of upstream Tiramisu v1.12.03 (the last GoStorm
release) and v1.13.1 (Tentacle) against Flodni's tree; nothing was run.

## Context

Upstream v1.13.0 replaced GoStorm with Tentacle, a new engine built around
FUSE, and removed the `:8090` API. Its release notes claim, from a simulator,
a cached-read p95 of 128 µs instead of 302 µs, 8.2 GiB/s cached throughput
instead of 3.8, no multi-file reads stuck over 15 s (2 to 6 a run before),
cold reads and sustained download on par, and a smaller binary.

Flodni forked before v1.11.9 and has since reworked the read path itself:
pump lead per playing pump, read-ahead cache eviction by class, an idle-based
stream timeout, stall `cause` and `origin` metrics, a starved re-announce for
private torrents, scoped authentication, durable records and CalVer releases.

On one production host, 2026-11-18 to 2026-11-22 under v2026.11.3, the stall
origins were `pump_behind` about half, `swarm` 29 % and `evicted` 21 %. A
second reader on a playing file (a media server's subtitle extraction) took
the pump. Those stalls last over a second; a sub-millisecond cached read is
not where Flodni loses viewers.

What the code shows:

- **What is new.** The bridge (`internal/bridge/`) and the engine
  (`internal/engine/`) are GoStorm reshaped, with the same reader, wake and
  fetch-ahead functions. The piece store (`internal/piecestore/`) is
  rewritten. The streamer (`internal/streamer/`) is new: no engine `Reader`,
  one pass per torrent sets priorities, deadlines, pins and eviction. The
  FUSE pump and primary promotion stay as they were.
- **Private torrents.** In the default tracker mode the public tracker list
  is added to every torrent with no private check
  (`internal/engine/trackers.go`). DHT and PEX are client-wide switches. None
  of the six tests that pin hard rule 3 has an equivalent, and the vendored
  engine lacks Flodni's private guard and forced announce. Upstream's
  vendored engine gained stream cursors and piece exclusion, which the
  streamer needs.
- **State and API.** v1.13.1 refuses to start while an unimported BBolt
  `config.db` is present (`internal/engine/settings/legacy.go`); the importer
  exists only in v1.12.03, and whether it reads Flodni's diverged records is
  unknown. `:8090` is gone.
- **Benchmarks.** The benchmark harness has no caller in the tree and the
  simulator is not shipped, so the numbers cannot be reproduced. Their
  baseline is v1.12.03, not Flodni.

## Decision drivers

- Hard rule 3: a private torrent stays private.
- Hard rule 4: the vendored engine takes fixes only.
- One production host: a playback regression or a migration that loses
  records costs more than a faster cached read.
- The changes Flodni makes should target the stall origins it measures.

## Considered options

1. Adopt Tentacle: rebase Flodni onto v1.13.1.
2. Stay on Flodni's engine and trial specific Tentacle techniques.
3. Stay, and take nothing.

## Decision

Option 2. Flodni keeps its engine. Three techniques are candidates for a
trial, each judged on Flodni's existing stall and eviction metrics, and only
after the roadmap's eviction breakdown ("Find what still evicts ahead of the
player") shows where the `evicted` stalls come from:

- window and read-ahead computed in one pass from the same shares;
- the playback window as a hard exclusion from eviction;
- pins served oldest wait first.

Upstream's pause of other torrents' prefetch when a playback risks a stall
is already on the roadmap. Stream cursors and piece exclusion would add
features to the vendored engine, which needs the owner's decision under hard
rule 4.

Option 1 breaks hard rule 3 on the first private torrent, and the guarantees
would have to be rebuilt in a new engine and a diverged vendored tree. By its
code it would not help `pump_behind` (the pump and the promotion stay, and
nothing paces a second reader on the same file) or `swarm` (same engine).
It could help `evicted`. The port is a re-port of every Flodni change, weeks
of work, plus a state migration with no tested rollback and the loss of
`:8090`. Option 3 ignores eviction ideas that target a measured stall origin.

## Consequences

Positive:

- No migration, no API break, and the privacy tests keep guarding the real
  code.
- Each borrowed technique is a small change measured on its own.

Negative / accepted trade-offs:

- Upstream fixes to the engine and the streamer no longer apply cleanly;
  each upstream release needs a review like the one behind this ADR.
- Any cached-read gain Tentacle has is forgone.

## Risks

- A borrowed eviction technique can let the cache overshoot its budget when
  nothing is evictable, and heap is already a pressure on the host. Each
  trial watches the cache size.
- The finding rests on a code read. A later upstream release can change it.

Reopen this decision if the eviction breakdown shows read-ahead evictions the
current cache cannot fix and upstream gains per-torrent private guards.

## Related

- Requirements: none tracked.
- Architecture views: none change.
- Other ADRs: [1. Hard-fork Tiramisu as Flodni](0001-hard-fork-tiramisu.md)
  made Flodni its own line; this keeps its engine on that line.
