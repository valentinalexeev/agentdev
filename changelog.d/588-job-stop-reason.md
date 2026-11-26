---
issue: 588
type: feature
section: Added
status: done
breaking: false
title: A job that stops without a summary names the reason; optional resource samples; the security pause names its jobs and events
components: [operator, worker]
pr: 590
---
## Summary

A run that ends with class `error` (often exit code 137) now names a `reason` in the job comment and the run log of
the job record: `security-pause`, `pod-deleted`, `oom` (the cgroup `oom_kill` counter rose) or `unknown`. New Agent
field `spec.diagnostics.resources` (default off; `intervalSeconds`, default 60) logs the memory and root file system use
of each run. A security pause names the jobs with events and, per source and rule, the count and first recorded action (never run event text) in the `SecurityPaused`
condition and a Kubernetes Event `SecurityPaused` on the Agent. The operator's generated Role
(`images/operator/config/rbac/role.yaml`) gains `events` `create`/`patch`; `manifests/operator.yaml.tmpl` already grants it.

## Upgrade

1. Run the deploy playbook as usual: it applies the updated `agents` CRD, which `spec.diagnostics` needs. Without the
   field nothing changes and agent pods are not restarted.

## Description

The operator marks the pod with the annotation `agentdev/stop-reason=security-pause` when it pauses the agent. The
loop reads it from the downward API file on SIGTERM and writes it to `jobs/stopping`; a SIGTERM without the mark gives
`pod-deleted`. Without SIGTERM, a rise of `memory.events` `oom_kill` during the run gives `oom`. The values
`node-preempted`, `evicted` and `deadline` are in the fixed list but no component sets them yet: that needs the operator
to read the pod's last state and the node conditions before the pod is gone.
