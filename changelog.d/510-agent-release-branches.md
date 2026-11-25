---
issue: 510
type: task
section: Changed
status: done
title: Release and forward-merge PRs from sandboxed agent branches
components: [ci]
---
## Summary

A sandboxed agent can open a release PR from `agent/<name>/release-X.Y.Z` and a forward-merge PR from
`agent/<name>/merge-<lower>-into-<upper>`. The changelog gate exempts them like `chore/release-*` and `merge/*`.
`scripts/release-lines.sh kind BRANCH` prints the kind of a head branch (docs/release.md §6).
