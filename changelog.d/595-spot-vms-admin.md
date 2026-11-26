---
issue: 595
type: docs
section: Changed
status: done
title: Admin guide section on Spot VMs, preemptions and exit code 137
components: [docs]
---
## Summary

`docs/admin.md` has a section "Spot VMs (GKE)". It tells the effect of a preemption on the system pool and on the agent pool, the other causes of exit code 137, how to find the cause, how to get an e-mail for each preemption, and what agentdev does to limit the effect.

## Description

The section replaces "Spot system pool (GKE)" and keeps its tasks. Text about unmerged code (#593) has a marker (an HTML comment) until the code is in the release.
