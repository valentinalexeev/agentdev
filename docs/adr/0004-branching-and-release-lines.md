# 0004: Branching and release lines

Status: accepted (owner, "go as proposed", 2026-11-04)

## Context

agentdev follows strict SemVer (`CLAUDE.md` §7) and plans work in milestones (`1.0.0`, `1.0.1`, `1.1.0` ... `2.0.0`).
Until now there was one branch, `main`, and no tag. Work from several milestones merged into `main` in whatever
order it was ready: the 1.1.0 items #169, #170, #171, #172, #183 and #184 merged on 2026-11-02, between 1.0.0 work.
That is fine before the first release, but after `v1.0.0` it would mean a 1.0 fix can only ship together with
unrelated 1.1 features.

The owner set three rules:

1. Several lines can be active at the same time (for example 1.0, 1.1 and 2.0).
2. Patches do not get a branch. Patch numbers only go up. Tagging is explicit, and one tag can bundle several fixes.
3. A higher line always contains everything in the lower lines. A fix on 1.0 moves 1.1 and 2.0 to a new baseline.

## Decision

### Lines and branches

| Branch | Holds | Lifetime |
|---|---|---|
| `release/X.Y` | one minor line: its stabilisation, then its patches | from the freeze of X.Y until the line leaves support |
| `main` | the next unreleased minor of the current major | permanent |
| `next/X.0` | breaking work for the next major while minors of the current major still ship | until the last minor of the current major is cut; then merged into `main` and deleted |

The active lines form one chain ordered by version, not by branch name:
`release/1.0` → `release/1.1` → `main` → `next/2.0`.
`scripts/release-lines.sh list` prints it from the remote branches.

- A line is **cut** from the line above it when its version freezes: `release/1.1` is branched from `main` when the
  1.1 scope is complete. From then on `main` is 1.2.
- A `next/X.0` branch exists only while breaking work has to merge before the current major is closed. Breaking
  changes go nowhere else once `1.0.0` is released.

### Tags (rule 2)

- `vX.Y.Z` tags are set only on a `release/X.Y` commit. Nothing is tagged on `main` or `next/*`.
- `vX.Y.0` is the end of the stabilisation of `release/X.Y`. `vX.Y.1`, `vX.Y.2` ... follow on the same branch.
- Patches never get a branch. A patch tag is an explicit owner decision and covers every fix merged since the
  previous tag of that line.
- Cutting a release is a PR `chore/release-X.Y.Z` into `release/X.Y` (commit `chore(release): vX.Y.Z`). The
  short-lived `release/X.Y.Z` prep branches are no longer used.

### Forward merges (rule 3)

- A fix is merged by PR into the **lowest supported line that has the defect**.
- Then it is merged forward, **one hop at a time**, each hop through a PR from a `merge/<lower>-into-<upper>` branch:
  `release/1.0` → `release/1.1` → `main` → `next/2.0`. `scripts/release-lines.sh forward <lower>` makes the hop
  branch, does the merge and pushes it; conflicts are resolved in that branch.
- Merge commits only. No rebase of a line branch (it would move tagged commits and make approvals stale) and no
  cherry-pick upward (it breaks the ancestry check and duplicates commits).
- A fix that does not apply to a higher line is still merged forward, resolved to the higher line's content, so the
  ancestry holds and the record shows that the hop was looked at.
- **Invariant:** for every adjacent pair, `git merge-base --is-ancestor <lower> <higher>`. `scripts/release-lines.sh
  check` tests it. The `Release lines` workflow reports it on every push to a line (warning, so `main` does not go
  red only because a hop is pending), and fails a release PR (`chore/release-*`) whose lower lines are not contained.
- **Mechanical forward merges** (no conflict, green CI) may be merged by the bot without a fresh owner approval.
  This is the only exception to the approval rule in `CLAUDE.md` §4. A hop with a conflict resolution needs the
  normal owner approval.

### Milestones

- Milestone `X.Y.0` maps to `main` until the cut, then to `release/X.Y`. A PR's base is the branch of its issue's
  milestone.
- Patches use one rolling milestone per maintained line, named `X.Y.x`. The owner renames it to the real version
  (`1.0.2`) when tagging; a new `X.Y.x` milestone is opened for the next patch.
- Milestones further ahead than the next cut are planning buckets, not branches. A change merged into `main` ships in
  the next minor cut from `main`. At each cut, the line's content is the truth: every issue merged into the line gets
  that milestone. A feature that must wait stays an open PR or ships default-off.
- After `1.0.0`, SemVer limits what a milestone holds: breaking changes only in a major (`next/X.0`), features and
  deprecations only in a minor, backwards-compatible fixes only in a patch.

### Support window

The latest minor and the one before it are supported (two `release/1.x` lines). Older lines get security fixes only,
on request. A line leaves support when the owner deletes its branch; its tags keep the history, and a security fix on
request re-creates the branch from the last tag.

## Transition (one time)

1. Move the 1.1.0 items already on `main` (#169, #170, #171, #172, #183, #184) to milestone 1.0.0. They are additive and
   mostly default-off; 1.0.0 is not released, so the kind-only BREAKING note of #172 becomes part of 1.0.0.
2. Cut `release/1.0` from `main`. The open 1.0.0 items (#214, #125, #123, #55) finish on `release/1.0` and merge
   forward. `main` becomes 1.1.
3. #210 (`feat(terraform)!`, milestone 1.3.0) is breaking: after 1.0.0 it is either made opt-in without `!` (stays
   1.3.0) or moved to 2.0.0 on `next/2.0`.
4. Milestone 1.0.1: move #27, #213 and #106 (features, deprecation) to 1.1.0 or later, then rename it `1.0.x`.

## Consequences

- One extra PR per hop for every fix on a lower line. With two supported minors plus `main` that is at most three hops.
- `CHANGELOG.md` entries for a fix are written in the lowest line's `## [Unreleased]`; a forward merge keeps the
  released `## [X.Y.Z]` sections of lower lines in version order in the higher line.
- CI runs on pushes to `main`, `release/**` and `next/**`; images are still published only from `main` and from tags.

## Alternatives rejected

- **One branch per planned minor** (1.1, 1.2, 1.3, 1.4 live at once): every 1.1 merge would cascade through four
  branches. Used only when two minors are really stabilised at the same time.
- **Rebasing higher lines on lower ones:** rewrites shared history, moves tags, invalidates approvals.
- **Cherry-picking fixes upward:** loses the ancestry invariant; the same fix exists as several commits.
- **A cascade-merge GitHub Action** (ready-made): PRs opened with `GITHUB_TOKEN` do not trigger CI, so the hop could
  never show green CI, and an extra app token would be a new secret. The bot opens the hop PRs with its own identity
  through `scripts/release-lines.sh`; the workflow only checks.
- **Cutting `release/1.0` from a commit before 2026-11-02** and cherry-picking the later 1.0 fixes: about eight
  cherry-picks for no gain.
