---
issue: 564
type: feature
section: Added
status: done
breaking: false
title: Per-agent Claude model and reasoning effort
components: [operator, worker, ansible]
---
## Summary

`Agent` has `spec.model` (an alias such as `opus`, or a full model name) and `spec.effort` (`low`, `medium`, `high`,
`xhigh`, `max`). The worker passes them to Claude Code as `--model` and `--effort`. Set them with `claude_model`,
`claude_effort`, `agents[].model`, `agents[].effort` (GKE) or `kind_agents_model`, `kind_agents_effort` (kind).
Unset: the Claude Code CLI's own default, as before. Docs: `docs/admin.md`, "Set the concurrency".

## Description

The operator passes the fields as `AGENT_MODEL` and `AGENT_EFFORT` to the agent container only, and only when they are
set, so the pod template of an agent without them does not change (no restart on upgrade). The names are not
`CLAUDE_MODEL`/`CLAUDE_EFFORT` because Claude Code itself exports `CLAUDE_EFFORT` to its tools and hooks. The CRD, the
operator (fail closed: phase `invalid`) and the worker (no flag and a log line) check the values: a model name starts
with a letter or digit, so it can never become another flag of `claude`. The effort flag and its levels were checked
against the pinned Claude Code CLI 2.1.312 (`--effort <level>`, levels `low, medium, high, xhigh, max`). The Claude
proxy already records the model of each response with its job, and the metric `agentdev_cost_units_total` has
the model as a label: that is where the new model shows.

Chat sessions in the console (#424) keep the CLI default. Several agent types per project (part 2 of #564) is not in
this change.
