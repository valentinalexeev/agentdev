# A job stopped by a Spot preemption, drain, scale-down or upgrade pushes its work and is retried, bounded by spec.retries.infrastructure (#593)

Released in 1.3.0-rc.3 (2026-11-26). Type: fix. Section: Fixed. Components: operator, worker, forge. PR: #594.

## Summary

A job whose pod is stopped by the infrastructure (pod deleted, Spot preemption, drain, scale-down, upgrade) is retried
on the next pod from its pushed work, at most `spec.retries.infrastructure` times (new Agent field, default 2; 0: never).
A security pause, a deadline and the job's own errors never retry. The comment says, for example, "Interrupted by node
preemption, retried (attempt 2 of 3)". On SIGTERM the loop records the reason, then pushes work in progress to
`agent/<agent>/wip/<job>` within 20 s. The operator detects the cause from pod conditions and node taints.

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

- **Keep the evidence of an abnormal exit** (addendum). Claude's stderr goes to a per-job file capped at 64 KiB
  (`jobs/claude-<job>.stderr`, deleted after the run). On any non-zero exit the loop logs one line with the exit
  status and signal name (137 SIGKILL, 143 SIGTERM), the class and reason, who sent the signal, the run and job wall
  times, the container's `memory.peak` and `memory.events` `oom_kill` (cgroup v2, v1 fallback, `n/a` when not
  readable, as under gVisor) and the last 4 KiB of stderr, redacted with the run log's filter (credential shapes and
  this environment's token values). The same goes to `jobs/<job>.exit` (JSON, state volume, survives log rotation)
  and as a result run event to the console's job record.
- **Attribute the worker's own kills** (addendum). Before the SIGTERM handler sends any signal it writes
  `jobs/<job>.killed` for each running job (`mark_killed`); the exit record and the next pod's stop event then say
  `killed by worker: <cause>`, and a signal without that mark says `not by the worker`.

Limits: the cause is best effort. When the operator does not see the pod in time (it scans every 15 s) the reason
stays `pod-deleted`, which is retried all the same. No run guard such as a job timeout or a budget kill exists in the
worker yet, so the SIGTERM handler is the only place that marks a kill; a future guard must call `mark_killed` first.
The memory figures are those of the whole container, not of the Claude process.
