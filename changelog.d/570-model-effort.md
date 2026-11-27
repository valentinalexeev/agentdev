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
as a cost hint. `spec.model` now accepts only `opus`, `sonnet`, `haiku`, `fable` or `claude-<family>-...` (optional
`[1m]`). The agent drawer shows the values; each change is an audit entry. Docs: `docs/admin.md`, "Set the model and
effort of an agent".

## Upgrade

1. The `Agent` CRD changes: `spec.model` has a stricter pattern and a maximum length of 80 (was 100). Apply the new
   CRD with the operator (`deploy.yml` does it). Run
   `kubectl get agents -A -o custom-columns=NAME:.metadata.name,MODEL:.spec.model` first: an agent with a model
   outside the families (for example another vendor's name) gets the phase `invalid` until you fix `claude_model`,
   `agents[].model` or `kind_agents_model`, and the API server refuses new writes of that value. `deploy.yml`,
   `render.yml` and `kind.yml` (tag `agents`) now stop before they apply anything when one of these values is wrong.
2. Optional: set `api.chat.models` and `api.chat.efforts` in `ansible/instance.yml` to allow other values in chat.
   An invalid entry stops the Ansible run and the API (fail closed).

## Description

Built on #564 (`AGENT_MODEL`, `AGENT_EFFORT`). The chat session server reads the same variables; the API sets them on
the session's chat container only from the allow-list, and the admission policy for session Jobs still admits the
pod (test `TestChatPodModelEnv`). The server sends `set_model` and `apply_flag_settings` (`effortLevel`) to Claude and
records a `settings` event when Claude answers. The proxy's `/status` adds `models` (multiplier by prefix) for the
cost hint; budgets and counting are unchanged. Not checked yet: the control requests against the pinned Claude Code
version on a live cluster.
