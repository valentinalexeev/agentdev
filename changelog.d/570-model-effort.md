---
issue: 570
type: fix
section: Added
status: in-progress
breaking: false
title: Per-agent Claude model and effort, and a model and effort choice for chat sessions
components: [operator, worker, api, credproxy, ansible]
pr: 576
---
## Summary

`Agent` has `spec.model` (an alias or a full name of a `claude-proxy` price family) and `spec.effort` (`low` to `max`).
Agent runs and chat sessions start Claude Code with `--model` and `--effort`; unset keeps today's behaviour. Set them
with `agents[].model` and `agents[].effort` (GKE) or `kind_agents_model` and `kind_agents_effort` (kind). A chat
session can change them with `POST /api/sessions/{id}/settings`. Docs: `docs/admin.md`, "Set the model and effort of
an agent".

## Description

The operator refuses an unknown model or effort (phase `invalid`), so a typo never counts at the highest multiplier;
the worker drops a value outside the rules. An agent without the fields gets no new environment, so its pod is not
restarted on upgrade. Each change of `spec.model` or `spec.effort` makes one audit entry, action `model`. The chat
server sends `set_model` and `apply_flag_settings` (`effortLevel`) to Claude and records a `settings` event when
Claude answers.

Not yet done: the model and effort picker in the console (thread header and **Start session**), reading the chat
allow-list `CHAT_MODELS` and `CHAT_EFFORTS` from the API environment, and a check of the control requests against the
pinned Claude Code version on a live cluster.
