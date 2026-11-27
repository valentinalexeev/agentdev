---
issue: 236
type: feature
section: Added
status: done
breaking: false
title: Forge sidecar labels, draft PRs, own comment edits, PR files and an ETag cache
components: [forge, worker]
pr: 247
---
## Summary

Forge sidecar interface additions, both forges. Labels `needs-decision`, `in-progress` and `agent:<name>` (only those;
`agent:<name>` only on the agent's own PR, the #227 rule) on issues given to the agent or its own PRs; draft PRs and a
sidecar `POST /repos/{o}/{r}/pulls/{n}/ready_for_review` for the agent's own PR; edits of the agent's own comments;
`GET /pulls/{n}/files`; an ETag cache for GET answers. The loop opens a draft PR for `STATUS: partial` (GitHub `draft`,
Gitea `WIP:` title) and adds `needs-decision` when it asks a question. New security events `label-not-allowed`,
`comment-not-own`. The GitHub App needs no new permission. Re-run: `./run.sh 06` or the agent role of the playbook.

## Description

A console chat pod cannot remove labels, edit comments or mark a PR ready (refused with `chat-label-not-asked`);
`agent:<name>` is only accepted alone, through the #227 rule.
