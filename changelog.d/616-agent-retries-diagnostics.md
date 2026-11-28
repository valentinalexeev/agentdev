---
issue: 616
type: feature
section: Added
status: done
breaking: false
title: Set spec.retries and spec.diagnostics of an Agent from instance.yml and kind.vars.yml
components: [ansible]
---
## Summary

The deploy playbook sets the Agent's `spec.retries` (#593) from `agents[].retries` and `spec.diagnostics` (#588) from
`agents[].diagnostics`, for example `{infrastructure: 4}` and `{resources: true, intervalSeconds: 60}`. The optional
instance-wide defaults `agent_retries` and `agent_diagnostics` apply to all agents; the keys of an agent have priority.
On `kind`, set `kind_agents_retries` and `kind_agents_diagnostics` per agent. Unset: no field, so the pod template
stays as before and an upgrade does not restart the agent. Docs: `docs/admin.md`, "How agentdev limits the effect".

## Description

Before, `templates/agents.j2` copied only some keys of `agents[]`, so a deployment had to patch the Agent with
`kubectl`, which drifts from Git. The values go to the CRD as they are; the CRD checks the ranges (`infrastructure`
0 to 10, `intervalSeconds` 5 to 3600). Test: `infra/ansible/tests/agents_render.yml`.
