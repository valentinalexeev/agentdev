---
issue: 496
type: feature
section: Added
status: done
breaking: false
title: Tool allow-list per agent in the Agent CRD
components: [operator, worker, ansible]
pr: 497
---
## Summary

`Agent` has an optional `spec.tools` with `allow` (added to the worker's default tool list) and `replace` (replaces
it). The operator passes it to the agent container as `CLAUDE_TOOLS_ALLOW` and `CLAUDE_TOOLS`, and refuses an incorrect
entry (phase `invalid`, reason `InvalidTools`). Set it with `kind_agents_tools` on kind or `agents[].tools` on GKE.
An agent without the field keeps the default list. Docs: `docs/admin.md`, `docs/concepts.md`.

## Description

The default list still has no `npm ci` or `npm install`. An install tool runs package install scripts, so a wider list
widens what a prompt injection or a bad dependency can run in the agent pod (threat model V6). The API cannot change
`spec.tools`: only a person who can write Agent objects can widen the list.
