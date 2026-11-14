# 3. Make the file lifecycle explicit in the library API

Date: 2026-11-14

## Status

Proposed

Designed, not built. Nothing in this record is implemented; the code described
as "today" is what the tree does now, the rest is a proposal for the owner to
accept, change or reject.

## Context

A file in Flodni changes state through side effects of calls that are named for
something else. An add may also replace an older release and warm the new one.
A play webhook confirms a session, marks a file played and protects it from
eviction in one pass. A caller that wants one of those effects without the
others, or wants to know what will happen before it asks, has no way to say so.
Roadmap step 19 asks for add, replace with a policy, confirm and remove as
explicit operations.

### Today's lifecycle (observed in the tree)

Every place a file's state changes, with the reason it counts as implicit:

| Event | Where | What changes as a side effect |
|---|---|---|
| Add (`POST /api/library/add`) | `Manager.Add`, `internal/library/manager.go` | Asks the engine to take the torrent and waits for metadata; writes the stub (`addMovie`, `addEpisodes`); for TV writes the registry row (`UpsertEpisode`, source `api`, the caller's `quality_score`); clears an open gap (`ClearEpisodeGap`); schedules a media-server scan (`scheduleRefresh`, `refresh.go`). A repeat of the same release answers `already_present` (200) and does nothing else. |
| Warm on add | `onLibraryAdded`, `internal/daemon/warm_add.go`, called from `Manager.Add` through `Config.OnAdded` | Enqueues the new files in the warmer when `warm_on_add` and `warmup_dir` are set; writes the per-file warm record (`filewarm.go`). The caller cannot ask for or refuse it per call. |
| Replace (TV only) | `addEpisodes`, `manager.go` | When the registry holds a different path for the same `EpisodeKey`, the new release is read once (`ReadFileHead`, 60 s, `replacementReadTimeout`); a release that delivers nothing is refused with 422 and the old files stay. Otherwise the new stubs are written, and only after the whole pack is written the old stubs are deleted and their torrents dropped when no other stub uses them (`dropTorrentIfUnused`). The decision is unconditional: `quality_score` is stored and never compared. A movie never replaces anything; a second release is a second stub. |
| Remove (`POST /api/library/remove`) | `Manager.Remove`, `manager.go` | Deletes the stub; optionally blacklists the hash (`blacklist`); forgets the registry row (`forgetEpisode`); drops the torrent when unused; schedules a scan. |
| Unlink through the mount | `VirtualDirNode.Unlink`, `internal/daemon/fuse_nodes.go`, wired to `Manager.Remove` by `unlinkRemove` in `main.go` | Same as Remove by path, without the blacklist flag. |
| Torrent dropped | `warmup.OnHashRemoved`, set to `onHashRemoved` in `warm_add.go` | Cancels the warmer for the hash and deletes its `file_state` rows (`ForgetHash`): the played mark and warm record of a removed or replaced file are gone, not archived. |
| Play confirmed | `handlePlexWebhook`, `internal/daemon/webhook.go` | `media.play` or `media.resume` matched to a session: marks it healthy (the confirmation the reader budget waits for), notes a late confirmation, raises the torrent's priority, and, on a start, `choosePlayedFile` picks one file and `countConfirmedPlay` (`filestate.go`) clears its eviction mark and records the play (`MarkPlayed`), which protects the file for `defaultPlayedProtectionDays` (90). Confirmation and played mark are one code path. |
| Play stopped | `handlePlexWebhook`, `media.stop` branch | Marks the session stopped, cancels the pump, drops priority. |
| Eviction | `fileStateIndex.OnEvicted`, `filestate.go`; triggered by the cache quota or `POST /api/warmup/evict` (`warmup_evict.go`) | Marks the file evicted and its cache state `evicted`; the played time and readability stay. A later confirmed cold play clears the mark. |
| Catch-up | `catchup`, `internal/daemon/warm_catchup.go` | A periodic pass over `GET /api/library/list` that feeds files not yet warmed (or failed, or queued and never finished) back to the warmer. |
| Reads through the mount | `filewarm.go` (`windowsCached`) and the FUSE read path | Write the warm record when a file's windows complete. |

Facts the design depends on, all from the tree: the file's stable identity in
the state store is `(hash, file_idx)` (`file_state` primary key,
`internal/metadb/db.go`); a TV episode's identity in the registry is
`EpisodeKey(title, season, episode)`; the control port is gated by `api_token`
as a bearer token (`internal/apiauth`), and every route registered on the mux
inherits it; the add path's worst case is bounded by the 600 s control-server
write timeout (`main.go`).

### What is wrong with it

- **No refusal.** A caller cannot say "add this, but only if nothing is there".
  Today an episode is replaced whenever the new release is readable.
- **No preview.** The only way to learn what an add would replace is to do it.
- **No manual confirm.** The only way to mark a file played, and so protect it,
  is a media-server webhook. A file played elsewhere, or a webhook that did not
  match, leaves no record.
- **No state read.** The per-file facts are spread over the stub, the registry
  and `file_state`; `GET /api/torrents/health` shows only the warm record.
- **Replacement and removal lose the history** of the file they end (played
  time, warm record), by design of `ForgetHash`.

## Decision drivers

- One maintainer, one deployment: fewer routes, fewer concepts.
- The caller keeps the replacement decision (the homelab decides which release
  is better; Flodni only enforces the safety gate and the caller's rule).
- Existing callers must keep working unchanged.
- A private torrent stays private (hard rule 3): a new route that can add a
  torrent, or look at one without keeping it, must not widen peer discovery.
- Auth stays what it is: the `api_token` bearer, no new mechanism.

## Considered options

### A. Extend the existing `/api/library` routes

Add parameters to `add` (`replace`, `dry_run`) and two routes in the same
namespace: `GET /api/library/file?hash=&file_index=` for state and
`POST /api/library/confirm`. `remove`, `list` and `inspect` stay as they are.

- Pro: smallest change; one namespace, one handler, one test file; existing
  callers see only additive JSON fields; no migration.
- Con: `add` keeps doing several things; "replace" is a parameter, not a verb.

### B. A new `/api/files` resource

`PUT /api/files/{hash}/{index}` to add, `DELETE` to remove, `GET` for state,
`POST .../confirm`, `POST .../replace`.

- Pro: the cleanest model, and the key is the one the state store already uses.
- Con: a second API beside the first for years (the old routes must keep
  working), a TV episode is not addressed by hash and index until it exists,
  and pack adds create many resources from one call, which fits a resource
  poorly. More code, more docs, two things to keep in sync.

### C. A small RPC set in a new namespace

`POST /api/lifecycle/{add,replace,confirm,remove,state}` with explicit bodies.

- Pro: each verb does one thing; the names match the roadmap.
- Con: same duplication as B with less structure; `replace` and `add` share
  almost all of their code, so the split is naming only.

## Decision

Proposed: option A. Keep the existing routes as the API; make the implicit
effects explicit by parameters and two additions. Reasons: it is the only
option that ships in small steps with no second API to carry, and the
homelab's callers already use these routes. B is the better model if Flodni
ever gets a second kind of client; this record does not need it. The add and
replace verbs are one operation with a policy because they share the same
steps (engine add, metadata, readability gate, write); a separate `replace`
route would be a second name for `add` with `replace: "always"`.

Everything below is designed, not built.

### Operations

All routes are on the control port, behind the existing bearer token, JSON in
and out, bodies capped at 1 MiB like today's.

**Add, with a replace policy and dry run** (`POST /api/library/add`, new
optional fields):

| Field | Values | Meaning |
|---|---|---|
| `replace` | `never`, `if-better`, `always` | What to do when the registry already holds a different release for the same `EpisodeKey`. `never`: refuse. `if-better`: replace only when the request's `quality_score` is greater than the stored one (a score of 0 is never better, which makes the unscored caller safe). `always`: replace. Omitted: `always`, which is today's behaviour. |
| `dry_run` | bool | Decide from the registry only and report; no engine call, nothing written, readability `unchecked`. |

The readability check stays under every policy: it is a safety gate, not a
policy. A release that delivers no data is refused with 422 whatever `replace`
says. `never` and `if-better` are decided from the registry before the engine
is contacted, so a refused add costs no metadata wait and touches no swarm.
For a pack the episodes are only known after metadata; there the policy is
applied per episode (see the open questions for what a mixed result means).

Response, additive fields on the existing body: `replaced` (the files this add
replaced or, in a dry run, would replace: `path`, `hash`, `file_index`),
`decision` (`added`, `already_present`, `replaced`, `kept`), `dry_run`.
Existing fields keep their meaning; today's `already_present` stays.

**Replace** is `add` with `replace: "always"` (or `if-better`). There is no
separate route in this proposal.

**Confirm** (`POST /api/library/confirm`, body `{hash, file_index}`): the manual
equivalent of the played-mark half of a play webhook: clears the file's
eviction mark and records the play, so the 90-day protection applies. If a
reader session is open on that file it also confirms the session as the
webhook does; with none open it only records the play. It does not touch
webhook counters (a separate `source` label on the play counter). Idempotent in
state: a repeat renews the played time, as a second play does.

**Remove** (`POST /api/library/remove`): unchanged. The unlink through the
mount stays a thin call to it.

**State** (`GET /api/library/file?hash=&file_index=`): a projection of records
that exist today (stub on disk, registry row, `file_state`), not a new store.

| State | Derived from |
|---|---|
| `added` | a stub links to `(hash, index)` and no warm record exists |
| `warming` | the warm record shows an attempt queued or running (`queued_at`) |
| `warm` | readability `warmed` or `present` and cache `cached` |
| `evicted` | cache state `evicted` (readability is reported separately, as in the health API) |
| `played` | a played time within the protection window |
| `confirmed` | a session open on the file that a webhook or confirm has confirmed |

The body reports these as independent facts (`stub`, `warm`, `played_at`,
`evicted`, `confirmed`) plus one summary `state`, because a file is warm and
played at once. `replaced` and `removed` are not states a live file can be in:
their rows are deleted today (`ForgetHash`), so the answer is 404. Keeping a
tombstone would need a new table and is an open question, not part of this
proposal.

### Idempotency and errors

| Case | Result |
|---|---|
| Add of a release already filed | 200 `already_present`, as today |
| Add refused by `replace` | 200 with `"decision": "kept"`; `files` is empty; nothing written, no engine contact |
| Replacement delivers no data | 422, as today; existing files kept |
| Confirm of an unknown `(hash, index)` | 404 |
| Confirm repeated | 200, state unchanged except the played time |
| Remove of nothing | 404, as today |
| Unknown `replace` value, missing keys | 400 |
| Missing or wrong token | 401 |
| Engine or registry failure | 502, 503, 504 as today |

### Auth

No new mechanism. Routes inherit `apiauth.Middleware`; no query-token
exemption. When `api_token` is empty the control port is open today, and the
existing routes stay as they are (`remove` keeps working without a token).
The new `confirm` fails closed: with `api_token` unset it answers 403 and
records nothing, because a manual play mark changes eviction protection.

### Compatibility

A request with none of the new fields behaves exactly as today, and a test
pins it: an `add` without `replace`, `quality_score` or `dry_run` returns the
same response shape as v0.1.0. New response fields are additive. `list`,
`inspect`, `remove` and the unlink path do not change. Callers that read
`already_present` keep working.

**The `files` contract.** Callers link every entry of an add's `files` that
carries a `fuse_path`, so `files` lists only files this request wrote:

- A refused episode of a partly allowed pack goes in a separate `skipped`
  list (with its reason), never in `files`. A pack with every episode refused
  answers with `files` empty.
- A `decision: "kept"` answer never lists the existing file in `files`;
  `files` is empty.
- A `dry_run` answer carries no `fuse_path` in any entry, since nothing was
  written.

An empty `files` therefore always means "nothing filed by this request".

### Not in scope

- A policy engine. Flodni compares a caller-supplied number at most; which
  release is better stays with the caller (the homelab keeps that decision).
- Release scoring, scheduling or automatic upgrades.
- Replacing movies by default (open question).
- Tombstones or a lifecycle history.
- Batch operations, events or webhooks out of Flodni.
- Any change to how a play is matched to a file.

## Consequences

Positive:

- A caller can refuse, preview and confirm without scraping logs.
- `never` and `if-better` refusals contact no swarm, so they cannot leak a
  private torrent's tracker contact.
- No second API to maintain; existing callers are untouched.

Negative / accepted trade-offs:

- `add` keeps its several effects; only the replacement and the preview become
  explicit. Warm on add stays a daemon switch, not a per-call option.
- `dry_run` is a registry decision only: it never asks the engine for the
  torrent, so it cannot say whether the release reads (readability
  `unchecked`), and a plan can still fail at the real add's readability gate.
- The state projection reads three stores that are updated at different times;
  a reader can see `added` a moment before `warm`.

## Risks

- **Private torrents.** `add` starts a torrent and keeps today's magnet and
  tracker handling. `dry_run` starts nothing: no engine call, so no announce
  at all, private or public. A refused add (`never`, `if-better`, `kept`)
  makes no engine call either. Tests below pin both with a recording fake.
- **Concurrent adds.** Two adds for the same episode with different policies
  race on the registry key today and are serialised by the per-episode lock
  (`hashLocks`, `showLocks`). The policy check must run under that lock.
- **Confirm abuse.** A manual confirm can protect any file from eviction, so
  it fails closed: with `api_token` unset it answers 403 and records nothing.
  With a token, anyone holding it can still protect any file.

## Migration: small pull requests

Each step is one PR off `main`, behaviour-preserving until its own feature,
with its tests. All are designed, not built.

1. **Pin today's lifecycle.** Characterization tests only, no code change:
   add calls `OnAdded` once and writes the registry row; replace keeps the old
   files when the read fails; remove and unlink end the same way; a dropped
   torrent clears `file_state`. Extends `replace_test.go` and `onadded_test.go`.
2. **Replace policy.** `replace` field, decided under the episode lock before
   the engine call for single episodes, per episode after metadata for packs;
   `decision` (`kept` answers 200 with an empty `files`), `replaced` and
   `skipped` in the response; `files` lists only files this request wrote.
   Tests: the `files` contract (no refused, kept or dry-run file ever carries
   a `fuse_path`); a request without the new fields returns the v0.1.0 shape;
   a table of
   policy against (no prior, prior with a lower score, equal, higher, score 0)
   asserting stubs, registry and engine calls; the readability gate still
   refuses under `always` and `if-better`; rollback unchanged; a race test
   with two concurrent adds of different policies (`-race`); omitted `replace`
   equals today's golden result. Privacy: a refused add makes no `AddTorrent`
   call (recording fake), extending `privacy_test.go`.
3. **Dry run.** `dry_run` is a registry decision only: it makes no engine
   call (no `AddTorrent`, no metadata wait, no head read), writes no stub or
   registry row, calls no `OnAdded` and no refresh, returns no `fuse_path`,
   and reports readability `unchecked`. Tests: a dry run makes no `AddTorrent`
   call (recording fake, extending `privacy_test.go`), for a private
   `.torrent` and a magnet alike; a paired test where the dry-run decision
   (`added`, `replaced`, `kept`, `skipped` per episode) equals the registry
   decision the real add then makes; nothing is written.
4. **State read.** `GET /api/library/file`, with a `Config` callback from the
   daemon for the `file_state` side (as `Gaps` and `OnAdded` are wired).
   Tests: one case per fact and the summary; 404 for an unknown or dropped
   file; 405 on POST; 401 with a token set and none sent.
5. **Confirm.** Split the session-independent part of `countConfirmedPlay`
   from the webhook; add `POST /api/library/confirm`. Tests: marks played and
   protects, clears the eviction mark, repeat is idempotent, unknown file 404,
   open session confirmed and none open tolerated, webhook tests unchanged and
   green, the play counter carries its source label.
6. **Docs and gate.** README API sections, this ADR to Accepted, the roadmap
   entry (done by the owner's process, not here), and the e2e variant that
   refuses an add with `never` and previews one with `dry_run`. Run on the
   maintainer's Docker host, not in CI.

## Answers to the open questions

Proposed by the maintainer and agreed by the main API consumer on
2026-11-14; the owner may still override any of them.

1. An omitted `replace` means `always` (today's TV behaviour). Movies do not
   replace: a second release stays a second stub.
2. `if-better` compares only the caller's `quality_score` (0 is never
   better); quality is the caller's policy.
3. A partly allowed pack writes the allowed episodes and reports the refused
   ones in `skipped`, never in `files` (see Compatibility).
4. `dry_run` is offline: a registry decision only, readability reported as
   `unchecked`; nothing is announced, and no `fuse_path` is returned.
5. A replacement starts unplayed, as today.
6. A manual `confirm` only records the play; it does not lift a reader-budget
   hold.
7. A replaced or removed file answers 404; no history table.
8. A refused replace answers 200 with `decision: "kept"` and an empty
   `files`.
9. Only the new `confirm` needs a token, and it fails closed (403) when
   `api_token` is unset; `remove` is unchanged.

## Related

- Requirements: none recorded.
- Architecture views: Daemon (library manager, warm on add, file state).
- Other ADRs: [2. Record readability in Flodni, choose releases outside it](0002-readability-evidence-in-engine.md),
  which sets the line this record keeps: Flodni records evidence, the caller
  chooses releases.
- Roadmap: step 19, [roadmap](../../roadmap.md) ("A proper file-lifecycle
  API", designed, not built).
