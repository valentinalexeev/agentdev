# Per-agent Claude Code permission mode (auto) with administrator rules (#500)

Released in 1.3.0-rc.3 (2026-11-26). Type: feature. Section: Added. Components: operator, worker, ansible. PR: #502.

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

The owner checked mode `auto` on a live cluster with 1.3.0-rc.2 (#504). Not yet done: the mode in the API audit log
and the console, and a security event for a call the classifier allowed outside `CLAUDE_TOOLS`.
