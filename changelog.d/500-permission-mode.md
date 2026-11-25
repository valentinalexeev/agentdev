---
issue: 500
type: feature
section: Added
status: in-progress
breaking: false
title: Per-agent Claude Code permission mode (auto) with administrator rules
components: [operator, worker, ansible]
---
## Summary

`Agent` has `spec.permission`: `mode: dontAsk` (default, as before) or `mode: auto`, with optional classifier rules in
`autoMode` (`allow`, `softDeny`, `environment`). In mode `auto` Claude Code's classifier decides a tool call outside the
allow-list; with approvals on, a call it blocks still goes to a person. Set it with `agents[].permission` (GKE) or
`kind_agents_permission` (kind). Docs: `docs/admin.md`, "Set the permission mode of an agent".

## Description

The operator passes the mode as `CLAUDE_PERMISSION_MODE` and the rules as `CLAUDE_AUTO_MODE` (JSON) to the agent
container only; the worker gives the rules to Claude Code with `--settings`. The rules come from the Agent object
only, never from the repository or issue text. The operator refuses another mode, rules without mode `auto` and rules
with control characters (phase `invalid`); the worker falls back to `dontAsk` on rules it cannot read. An agent without
the field gets no new environment, so its pod is not restarted on upgrade. `bypassPermissions` is not supported, and
the hard deny rules of Claude Code cannot be changed.

Not yet done: the mode in the API audit log and the console, a security event for a call the classifier allowed
outside `CLAUDE_TOOLS`, and a check on a live cluster that the classifier calls pass `claude-proxy` and that
`--permission-mode auto` combines with `--permission-prompt-tool`.
