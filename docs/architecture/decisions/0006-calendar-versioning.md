# 6. Number releases by calendar (vYYYY.M.N) from the next release

Date: 2026-11-18

## Status

Accepted on 2026-11-18. It takes effect with the next release, which is
`v2026.11.0`. The last release numbered by semantic versioning is `v0.3.12`.

The owner decided the format and its rules. Nothing in the code changes: the
release workflow already accepts any `v*` tag.

## Context

Flodni ships small releases often, several in a week at times. Each one is a
tag on `main` after CI is green; the tag builds the image.

The `0.x` version says nothing useful. Under semver a `0.x` line promises
nothing, so a minor bump can break anything, and the project has used it that
way: `v0.3.0` held the scoped-token upgrade, and `v0.3.1` to `v0.3.12` were
fixes and features of every size. A reader cannot tell from the number whether
an upgrade is safe, how old a build is, or whether two builds are close. Moving
to `1.0.0` would promise a stability the README says Flodni does not offer
("Expect breaking changes and no support").

What the version is used for today, all from the tree:

- The git tag (`v*`) triggers the publish job in `ci.yml`.
- The image tag is the git tag (`type=ref,event=tag`), plus the commit SHA.
- `APP_VERSION` is the tag, built into the binary, and reported in the startup
  report and as the `version` label of the build info metric.
- Nothing parses the version, compares it or branches on it.

## Decision drivers

- A version that tells the reader when a build was made.
- No change to CI, the image or the binary.
- A valid semver string, so the tools that read tags (Renovate, registries,
  `sort -V`) keep working and sort the new tags above the old.
- One maintainer: no rule that needs a judgement call on every release.

## Considered options

1. Stay on semver and keep counting `0.3.N`.
2. `vYY.M.N` (for example `v26.11.0`).
3. `vYYYY.MM.DD.N` (for example `v2026.11.18.0`).
4. `vYYYY.M.N` (for example `v2026.11.0`).

## Decision

Option 4. From the next release, a tag is `vYYYY.M.N`:

- `YYYY` is the year of the release and `M` is its month, with no leading
  zero. Semver forbids leading zeros in a number, so it is `v2026.1.0`, never
  `v2026.01.0`.
- `N` counts the releases in that month, from 0. It resets to 0 when the month
  changes. The sequence is `v2026.11.0`, `v2026.11.1`, `v2026.12.0`,
  `v2027.1.0`.
- A fix is the next `N`, like any other release. There is no separate patch
  line.
- `v2026.11.0` is the first CalVer tag. It sorts above `v0.3.12` in semver
  order, so every tool that picks the highest version moves to it with no
  special case.
- **Breaking changes are no longer signalled by the number.** Each release's
  notes carry an explicit `Breaking:` line, and the README upgrade section is
  written when an upgrade needs steps. A release with nothing breaking says
  `Breaking: none`.
- Tags stay immutable, annotated, and covered by the provenance attestation CI
  signs, exactly as today. A wrong tag is never moved or deleted; the next
  release supersedes it.

Option 1 keeps a number that means nothing, and it invites a `1.0.0` that
Flodni would not honour. Option 2 is a valid semver string too, but `26.11.0`
reads as a semver major and collides with any future semver line if the project
ever wants one back; four digits of year cannot be mistaken for a count.
Option 3 is not semver at all: it has four numeric parts, Renovate's semver
versioning and most registries reject or misorder it, and a date in the number
pairs badly with several releases a day.

## Consequences

Positive:

- The age of a build is in its tag and in the startup report.
- No workflow, Dockerfile, Makefile or Renovate change; the first CalVer tag
  goes through the existing publish job.
- The tag is valid semver and sorts above every `0.x` tag.

Negative / accepted trade-offs:

- **The number no longer says whether an upgrade breaks.** A reader has to open
  the release notes. A tool that treats a patch or minor bump as safe, such as
  a Renovate rule that automerges them, would treat a breaking Flodni release
  as safe too: `v2026.11.0` to `v2026.12.0` is a semver minor and
  `v2026.12.0` to `v2027.1.0` a semver major, whatever changed. A consumer
  should not automerge Flodni updates. Nothing in this repository does.
- The jump from `v0.3.12` to `v2026.11.0` is a semver major bump, once. The
  Go module is named `flodni` and is not importable, so Go's `/vN` major
  version suffix rule does not apply, and nothing installs Flodni with
  `go install` at a version. The image is the only distribution.
- Historical text keeps the old numbers: "v0.3.0", "the v0.1.0 body" and the
  `0.x` tags name real past releases, and they are not rewritten.

## Risks

- A release whose notes omit the `Breaking:` line leaves the reader with no
  signal at all. Mitigated by making the line mandatory in the release notes;
  there is no check for it in CI.
- Two releases in one month with a wrong `N` (a skipped or reused number).
  Tags are immutable, so a reused number cannot happen; a skipped one is
  harmless.

## Related

- Requirements: none tracked.
- Architecture views: none change (no new container or component).
- Other ADRs: [1. Hard-fork Tiramisu as Flodni](0001-hard-fork-tiramisu.md)
  made the project its own line of releases.
