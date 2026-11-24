---
issue: 499
type: feature
section: Added
status: in-progress
breaking: false
title: Per-agent toolchain setup (spec.toolchain)
components: [operator, ansible]
---
## Summary

Optional `spec.toolchain` on the Agent (`agents[].toolchain` in `instance.yml`): setup steps that install tools for one
agent into `/state/toolchain`. The steps run in a new init container `toolchain` of the worker image, with the egress
proxy and no credential, and run again only when the steps change, a step failed or `refresh` passed. `path` goes
first in the agent's `PATH`; `failurePolicy` (`Fail`, `Continue`) decides if a failed step stops the pod. No toolchain:
no change. See docs/admin.md, "Install tools for one agent".

## Description

Not in this change (follow-ups of #499): the status of each step in `Agent.status` and the console, a periodic
refresh while the pod runs, a privileged step for `apt-get`, a security event for a failed step, and the toolchain in
chat sessions.
