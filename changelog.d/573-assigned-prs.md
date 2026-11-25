---
issue: 573
type: fix
section: Fixed
status: done
title: An agent works on any pull request assigned to it, whatever its author or branch
components: [worker]
pr: 575
---
## Summary

The agent now also works on a pull request that it did not open: the label `agent:<name>` (GitHub) or an assignment
(Gitea), set by a person with write access. It never pushes to another author's branch: it pushes to
`agent/<name>/pr-<n>` and opens a stacked PR into the head, or posts a patch when the head is the default branch.
A fork, or a label by a person without write access, gets one comment that says why. A wake that starts no job
writes `wake: no job started` in the log. Docs: `docs/concepts.md`, "Who may give work".

## Description

Before, the loop read feedback only for PRs that the agent opened from `agent/<name>/*` (`MY_PRS`). A PR by a person,
for example from `main` into an agent branch, was routed and woke the agent, but the agent did nothing and logged
nothing. The new filter `ASSIGNED_PRS` selects the other PRs; `handle_assigned` checks the labeler (#237 P3), the
head repository (no forks) and the branch names, and then starts the usual `pr-<n>` job with the same cursor,
backoff and `maxConcurrent`. The first run after the assignment needs no comment: the assignment is the request.
