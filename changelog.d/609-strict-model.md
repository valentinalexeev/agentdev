---
issue: 609
type: task
section: Changed
status: done
breaking: false
title: spec.model limited to the claude-proxy price families (back-port of the #570 rule)
components: [operator, worker, ansible]
---
## Summary

`spec.model` (and `claude_model`, `agents[].model` in Ansible) accepts only `opus`, `sonnet`, `haiku`, `fable` or
`claude-<family>-...`, optionally with the suffix `[1m]`, at most 80 characters, so a typo is refused instead of being
counted at the highest `claude-proxy` multiplier. The CRD, the operator, the worker and the playbook checks apply the
same rule as 1.4.

## Upgrade

1. An rc.2 or rc.3 install whose `spec.model`, `claude_model` or `agents[].model` holds a free-form name (for example
   `sonet` or `gpt-5`) must change it to a family name above before upgrading, or the agent goes to phase Invalid.
