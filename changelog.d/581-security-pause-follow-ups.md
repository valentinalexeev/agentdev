---
issue: 581
type: fix
section: Fixed
status: done
title: Security pause follow-ups, interrupted jobs marked and no toolchain reinstall on every pod
components: [operator, worker, docs]
pr: 586
---
## Summary

A job that the drain time of a security pause cuts off is marked "interrupted by security pause": the saved commit
names the reason, and after resume the job comments on its issue or PR (operator annotation `agentdev/drain-reason`).
An unchanged toolchain is no longer reinstalled on each new pod, because the kubelet's fsGroup mode change no longer
changes the manifest hash. `docs/admin.md` lists the default egress hosts and explains why a high-severity stop keeps
the normal grace period.
