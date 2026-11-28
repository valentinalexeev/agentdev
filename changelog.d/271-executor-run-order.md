---
issue: 271
type: feature
section: Changed
status: done
title: Runs of one Executor run one at a time
components: [operator]
pr: 264
---
## Summary

The merge plans, applies and snapshots of one Executor run one at a time; a waiting run starts at the next poll after
the active run ends. Order: an approved apply first, then merge plans by merge time (annotation
`agentdev.dev/merged-at`), then the snapshot and the drift run. PR head plans and probes do not wait.
