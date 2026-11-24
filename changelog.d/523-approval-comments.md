---
issue: 523
type: feature
section: Changed
status: done
breaking: false
title: One approvals comment per job, and Web Push before the forge
components: [forge, api]
pr: 541
---
## Summary

The forge sidecar no longer posts one "waiting for approval" comment per request. Each job has one approvals comment
on its issue or PR: a table of its requests (id, tool, state), the text of each waiting request, and the commands in
a `<details>` block. The sidecar edits it on each new request and change of state, so a job with many requests makes
one comment. When Web Push reaches a person with write access, the request is not commented on the forge at all; it
goes there only if nobody decided before the agent stopped waiting. Docs: `docs/user-guide.md`, "Answer an approval
request from an agent".

## Description

New internal endpoint `GET /internal/approvals/{agent}/reach?repo=&forge=&source=` on the API (registered when
`APPROVALS=true` and Web Push is on). The caller must be a running pod of the agent and the repository one of its
repositories; the answer is one bit. A sidecar of this release with an older API, or with Web Push off, gets no
`true` answer and comments as before (one comment per job). Cost requests are never pushed, so they always go to the
comment. The decision rules in `images/api/approvals.go` do not change.
