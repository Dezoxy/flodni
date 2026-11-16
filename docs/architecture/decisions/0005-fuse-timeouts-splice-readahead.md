# 5. Make FUSE timeouts and splice options, leave kernel readahead to the host

Date: 2026-11-16

## Status

Accepted

Builds on the mount described in [1. Hard-fork Tiramisu as Flodni](0001-hard-fork-tiramisu.md).
Part of the roadmap's "Engine hardening from upstream", step 7.

## Context

Upstream Tiramisu, in v1.11.10 and v1.11.11, changed four things about how its
mount talks to the kernel. These are upstream's choices and its stated reasons;
none of them is measured on Flodni.

- **Longer attribute and entry timeouts** (5 s in upstream, against Flodni's
  1 s), to cut FUSE round trips from library scans. Upstream paired it with
  kernel dentry invalidation (`NotifyEntryPath`) for stubs added or removed
  outside the mount, because a longer entry timeout keeps a removed name visible.
- **A 1 MB kernel readahead**, written to the mount's backing device entry in
  sysfs. That write needs privileges a container usually does not have.
- **Splice off.** Upstream's commit says 1 MB replies overflowed the pipe and
  logged `trySplice` on every read.
- **A configurable `fuse_max_background`.** Flodni already ties
  `MaxBackground` to `master_concurrency_limit`; nothing to add.

What Flodni has today:

- `attr_timeout_seconds` 1, `entry_timeout_seconds` 1 and
  `negative_timeout_seconds` 0 are config keys (the start log prints them). Any
  float is accepted, including a negative or absurd one.
- Splice is whatever go-fuse does by default (on). The mount's `MaxWrite` and
  `MaxReadAhead` are 4 MB.
- Nothing invalidates the kernel's dentries. A stub removed or replaced through
  the library API changes the physical source folder, not the mount, so the
  kernel keeps the old name until the entry timeout runs out. At 1 s that is
  invisible; at 60 s it would not be.
- There is no reliable way to know what a change does without a number from a
  real library and swarm. The owner and the deployment decided to measure first.

## Decision drivers

- Flodni never changes the host: no sysfs write, no privileged call at start.
- Defaults stay as they are until a deployment has numbers.
- A setting that can show a deleted file must say so where it is set.
- One maintainer: no code for a case nobody has asked for.

## Considered options

1. Adopt upstream's values (5 s timeouts, splice off, 1 MB readahead) as
   defaults.
2. Make the timeouts and splice options, defaults unchanged; leave readahead
   to the host; do not build dentry invalidation yet.
3. Make them options and also build dentry invalidation now.
4. Change nothing.

## Decision

Option 2.

**Timeouts.** `attr_timeout_seconds`, `entry_timeout_seconds` and
`negative_timeout_seconds` stay the keys (not renamed; a rename would break
existing files for nothing). Each must be from 0 to 60 seconds; a value outside
that range, or not a number, **refuses the start** and a panel save that would
write it. Startup-only: the mount is made once, so a save changes nothing until
the next start. Defaults 1, 1 and 0, pinned by a test.

**Splice.** New `fuse_disable_splice`, default `false`, which sets go-fuse's
`DisableSplice`. Startup-only. Off is the behaviour today.

**Kernel readahead is host policy.** Flodni never writes sysfs, in any mode. The
mount's BDI is observed and logged, and the value exposed as a gauge (a separate
change); a deployment that wants a different readahead sets it itself, with
the privileges it already has. This record does not decide a value.

**Dentry invalidation is required before a deployment relies on a longer entry
timeout, and is not built here.** The reason is the removal path above: with a
timeout above 1 s, a removed or replaced stub stays visible for that long,
and a media server can list a file whose read then fails. The cost of building
it is small (upstream's `NotifyEntryPath` walks the inodes the kernel has
looked up and calls `NotifyEntry`), but it adds a call from the library
removal and add paths into the mount, and it should be written against a
measured gain, not before. So:

- Raising `attr_timeout_seconds` or `entry_timeout_seconds` above 1 is
  **allowed and warns at start**, in one `[Config] WARNING` line that names the
  stale-name window and this record. It does not refuse, because the deployment
  will A/B these values and a refusal would block that.
- Recommending a value above 1 s to anyone needs invalidation first. Building
  it is the follow-up if the numbers favour longer timeouts.

The negative timeout is bounded the same way but does not warn: a longer one
hides a stub that was just added, which is the opposite risk, and it stays 0
unless someone has a reason.

## Consequences

Positive:

- The deployment can change the timeouts and splice with a config edit and a
  restart, and compare numbers, with no new image.
- Defaults and the mount are byte-for-byte what they were; a test pins both.
- Flodni gains no privilege and writes nothing outside its own paths.
- A typo (`-1`, `600`) is refused at start instead of mounting with a
  surprising cache.

Negative / accepted trade-offs:

- A deployment can raise the timeouts and get stale names until invalidation
  exists. The warning is the only guard.
- Splice off is a guess from upstream's report, not a Flodni measurement; it
  may cost CPU (a copy through user space) on a fast mount.
- A hand edit of `config.json` that used to mount now refuses to start if it
  held a negative or over-60 timeout. Nobody is known to have one.

## Risks

- A longer entry timeout shows a deleted stub; the read of it fails with an
  error the media server may report as a broken file. Mitigated by the warning
  and by the default.
- The 60 s ceiling is a judgement, not a limit of the kernel; raise it when a
  measurement asks for it.

## Related

- Requirements: none tracked.
- Architecture views: none change (no new container or component).
- Other ADRs: [1. Hard-fork Tiramisu as Flodni](0001-hard-fork-tiramisu.md).
