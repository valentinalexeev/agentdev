---
issue: 570
type: feature
section: Added
status: done
breaking: false
title: Model and effort choice for chat sessions; spec.model limited to the claude-proxy price families
components: [operator, worker, api, ui, credproxy, ansible]
pr: 576
---
## Summary

A chat session starts with the agent's `spec.model` and `spec.effort`. A person with write access can choose another
model or effort on **Start session** or in the session header, from the agent's own values and the instance allow-list
`api.chat.models`, `api.chat.efforts` (`CHAT_MODELS`, `CHAT_EFFORTS`). The menus show the `claude-proxy` multiplier
as a cost hint. `spec.model` keeps the 1.3.0 rule: `opus`, `sonnet`, `haiku`, `fable` or `claude-<family>-...` (optional
`[1m]`). The agent drawer shows the values; each change is an audit entry. Docs: `docs/admin.md`, "Set the model and
effort of an agent".

## Upgrade

1. No action for `spec.model`: the stricter pattern (max 80) already shipped in 1.3.0 (#609, #610). `deploy.yml`,
   `render.yml` and `kind.yml` (tag `agents`) check `claude_model`, `agents[].model` and `kind_agents_model` with the
   same rule (`tasks/model_check.yml`) before they apply anything.
2. Optional: set `api.chat.models` and `api.chat.efforts` in `ansible/instance.yml` to allow other values in chat.
   An invalid entry stops the Ansible run and the API (fail closed).

## Description

Built on #564 (`AGENT_MODEL`, `AGENT_EFFORT`). The chat session server reads the same variables; the API sets them on
the session's chat container only from the allow-list, and the admission policy for session Jobs still admits the
pod (test `TestChatPodModelEnv`). The server sends `set_model` and `apply_flag_settings` (`effortLevel`) to Claude and
records a `settings` event when Claude answers. The proxy's `/status` adds `models` (multiplier by prefix) for the
cost hint; budgets and counting are unchanged. Not checked yet: the control requests against the pinned Claude Code
version on a live cluster.
