---
issue: 593
type: fix
section: Fixed
status: done
breaking: false
title: A job stopped by a Spot preemption, drain, scale-down or upgrade pushes its work and is retried, bounded by spec.retries.infrastructure
components: [operator, worker, forge]
---
## Summary

A job whose pod is stopped by the infrastructure (pod deleted, node preemption, node drain, autoscaler scale-down,
upgrade) is retried on the next pod, resuming from its work in progress, at most `spec.retries.infrastructure` times
(new Agent field, default 2: at most 3 attempts; 0 never retries). A security pause, a deadline and the job's own
errors are never retried. The comment says, for example, "Interrupted by node preemption, retried (attempt 2 of 3)";
the attempt goes to the job's run log. On SIGTERM the loop records the reason first, then commits and pushes each
job's work in progress to `agent/<agent>/wip/<job>` within a 20 s budget (Spot gives 30 s), never to the job's branch.
The operator tells a preemption from a drain or a scale-down by the pod's `DisruptionTarget` condition and the node's
taints, and records it as a `PodStopped` Event on the Agent.

## Upgrade

1. Run the deploy playbook as usual. It applies the updated `agents` CRD, which `spec.retries` needs, and the new
   ClusterRole `agentdev-operator-nodes` (`get` on `nodes` only) in `manifests/operator.yaml.tmpl`. Without it the
   operator cannot read the node and records drains and preemptions only from the pod's conditions.
2. Nothing else: an Agent without `spec.retries` keeps its pod template (no restart on upgrade) and gets 2 retries.
   To turn retries off for an agent, set `spec.retries.infrastructure: 0`.

## Description

Builds on #588 (#590), which records why a job stopped. This change adds the parts the issue asks for after it:

- **Push work early.** Before a Claude run of a job that already has local commits the forge does not have (a resumed
  job), and on SIGTERM after the reason is written, the loop pushes the job's tree to `agent/<agent>/wip/<job>`
  through the forge sidecar, with a short timeout (`WIP_PUSH_TIMEOUT`, default 8 s; the shutdown pushes stop at
  `TERM_PUSH_SECONDS`, default 20 s, after the signal). A fresh job has nothing to push, so a run without a stop costs
  the same as before. The wip branch is separate, so half-done work never becomes a PR, a PR update or a summary. The
  forge sidecar keeps serving after its own SIGTERM until the loop writes `loop-exit` to the wake file, at most
  `FORGE_TERM_HOLD_SECONDS` (default 25 s).
- **Retry.** The loop counts the attempts of a job in `jobs/<job>.attempts` and decides on the next pod: an
  infrastructure reason within the bound runs the job again from its kept tree, or from the pushed wip branch when the
  state volume has no tree; any other reason, or a used-up bound, ends the job with a fixed note that names the wip
  branch, and the issue is not picked up again. The operator holds the same rule (`images/operator/retry.go`); a
  test keeps the two lists of reasons equal.
- **Detect the cause.** The operator's collector looks at each agent pod that is being deleted: the security pause
  mark, the drain annotation of its own upgrade, the pod's `DisruptionTarget` condition (`TerminationByKubelet`: a node
  shutdown, as on a Spot preemption; `EvictionByEvictionAPI`: a drain or a scale-down, told apart by the node's
  `ToBeDeletedByClusterAutoscaler` or unschedulable taint), the GKE `cloud.google.com/impending-node-termination` taint,
  and a Spot node that is no longer ready. It writes `stop.<agent>` into the `agentdev-usage` ConfigMap, which the agent
  pod already mounts, and the next pod uses it to name a plain `pod-deleted` more precisely.

Limits: the cause is best effort. When the operator does not see the pod in time (it scans every 15 s) the reason
stays `pod-deleted`, which is retried all the same. The issue's addendum (stderr tail, signal name and memory figures
of an abnormal exit; attributing self-inflicted kills) is not part of this change.
