---
issue: 505
type: feature
section: Added
status: done
breaking: false
title: Permission mode and auto-mode rules of an agent in the audit log and the console
components: [api, ui]
---
## Summary

The API writes one audit entry (action `permission`) for each change of `spec.permission` of an agent, with the old
and the new mode and rules. `GET /api/agents/{name}` returns `permission` to `agentdev-admins` members, and the agent
drawer of the console shows the mode and the rules, read-only, on the **Permission** tab. Docs: `docs/admin.md`,
"Set the permission mode of an agent".

## Description

The API watches the Agent objects and compares `spec.permission` with the last `permission` entry of the agent, so
a restart writes no duplicate and a change made while the API was stopped is recorded once when it starts. An agent
that always had the default (`dontAsk`, no rules) makes no entry. The API does not know who changed the object: the
entry has no person; the Git history or the Kubernetes audit log names them. Editing stays in the Agent object
(Ansible or Git).

The store reads the last entry and inserts in one transaction with an advisory lock per agent, so two API
processes write one entry. Without an earlier entry and with a value other than the default, the entry has
`before: null` and the reason `first seen (no earlier entry)`. The container log line has the mode, rule counts and
a sha256 of the rules only, never the rule text.
