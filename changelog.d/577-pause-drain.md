---
issue: 577
type: fix
section: Fixed
status: done
title: A security pause for many low or medium events lets the running jobs finish
components: [operator, forge, worker]
pr: 579
---
## Summary

A `TooManyEvents` security pause now drains the agent first: the loop starts no new job, the running jobs finish
(at most `DRAIN_MAX_MINUTES`), then the pod stops. A high-severity event still stops the pod at once. Repeats of
one `run-events-invalid` refusal count once per job toward `SECURITY_PAUSE_AFTER`. The event names the refused repo,
the number of events and a hash of the first event. The `Resumed` condition and the operator log name who resumed
the agent. Docs: `docs/admin.md`, "Resume an agent that the operator paused"; `docs/security-model.md`.

## Description

On v1.3.0-rc.1 six low `run-events-invalid` events in one job paused the agent, and the pause deleted the pod at
once: the running jobs ended with exit code 137. The batches came from the worker test `resume.test.sh`, which ran
inside an agent pod and sent its run log to the real forge sidecar (`FORGE_API`); the test now points `FORGE_API`
at a closed port.

The security collector marks the Agent with the annotation `agentdev/pause-drain` (Unix time) when it pauses for
`TooManyEvents`. While the mark is there, the reconciler keeps the replica count, marks the pod for drain (the same
annotation as a rollout drain) and checks again every 15 seconds. It stops the pod when the jobs have ended, the pod
is not ready, or `DRAIN_MAX_MINUTES` passed. A resume clears both marks. The operator also no longer logs a
toolchain security event again on each reconcile.
