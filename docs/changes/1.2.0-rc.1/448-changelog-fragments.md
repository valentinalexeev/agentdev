# Changelog fragments in changelog.d (#448)

Released in 1.2.0-rc.1 (2026-11-23). Type: task. Section: Changed. Components: scripts, ci.

## Summary

A PR now adds one file `changelog.d/<issue>-<slug>.md` instead of a line under `## [Unreleased]` in `CHANGELOG.md`
(format: `changelog.d/README.md`). `scripts/changelog.sh check|preview|release` validates and assembles them;
`scripts/release-lines.sh release X.Y.Z` cuts the release PR branch with them. The `Changelog` workflow fails a PR
that changes code without a fragment (label `no-changelog` to skip) or edits `## [Unreleased]` directly.

## Description

Parallel PRs all appended to the same `## [Unreleased]` lines, so almost every merge produced a conflict in
`CHANGELOG.md`. One file per change removes the shared lines. The tool is Python 3 standard library only (no PyYAML,
no yq on the runner): the frontmatter is a small YAML subset. At release the existing `## [Unreleased]` lines are
kept and fragment entries are added into the same `###` sections, so the content written before #448 is not
migrated (owner decision). `## Description` parts move to `docs/changes/<version>/` to keep the longer history.
