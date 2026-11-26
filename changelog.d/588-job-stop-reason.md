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

A run that ends with class `error` names a `reason` in the job comment and run log: `oom` or `unknown`. A job stopped
by SIGTERM names `security-pause` or `pod-deleted` when it continues on the next pod (preemption, eviction and deadline
are not detected yet and show as `pod-deleted`). New Agent field `spec.diagnostics.resources` (default off) logs memory
and disk use of each run. A security pause names its jobs and events (counts, first action; never run event text) in
the `SecurityPaused` condition and Event. The operator's Role gains `events` `create`/`patch`
(`manifests/operator.yaml.tmpl` already grants it).

## Upgrade

1. Run the deploy playbook as usual: it applies the updated `agents` CRD, which `spec.diagnostics` needs. Without the
   field nothing changes and agent pods are not restarted.

## Description

The operator marks the pod with the annotation `agentdev/stop-reason=security-pause` before it pauses the agent. On
SIGTERM, before it kills the jobs, the loop reads the mark from the downward API file and writes it into the state of
each running job (`jobs/<job>.stopped`, kept across the restart with the job state of #175); without the mark the
reason is `pod-deleted`. The job that continues on the next pod posts the reason once to its run log, names it in its
comment, and removes the file when it succeeds. Without SIGTERM, a rise of `memory.events` `oom_kill` during the run
gives `oom` (where the sandbox exposes the counter; else `unknown`). The values `node-preempted`, `evicted` and
`deadline` are in the fixed list but nothing sets them yet: those stops show as `pod-deleted`. Detecting them needs the
operator to read the pod's last state and the node conditions before the pod is gone.
