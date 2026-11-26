# Admin guide section on Spot VMs, preemptions and exit code 137 (#595)

Released in 1.3.0-rc.3 (2026-11-26). Type: docs. Section: Changed. Components: docs. PR: #596.

## Summary

`docs/admin.md` has a section "Spot VMs (GKE)". It tells the effect of a preemption on the system pool and on the agent pool, the other causes of exit code 137, how to find the cause, and what agentdev does to limit the effect.

## Description

The section replaces "Spot system pool (GKE)" and keeps its tasks. The text was checked against the code of #588, #590 and #593.
