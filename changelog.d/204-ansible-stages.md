---
issues: [204, 205]
type: feature
section: Added
status: done
title: Ansible executor stages over SSH certificates, and server snapshots
components: [operator, manifests, ansible]
pr: 264
---
## Summary

`Executor.spec.ssh` gives the merge plan a 15-minute certificate for `agentdev-check` (no sudo) and the approved
apply one for `agentdev`. The pull request head runs `spec.prPlan` without any SSH credential. SSH goes through the
executor egress proxy, only to the executor's `Ready` SSHTargets. `spec.snapshot` publishes redacted, untrusted
server output in ConfigMap `agentdev-snapshots`. Ansible vars `ssh`, `pr_plan`, `snapshot`, `snapshot_hours`.
Risk R135.

## Description

Key, certificate, `known_hosts` (`@cert-authority`, `HostKeyAlias`), ssh config and inventory are in Secret
`<run>-ssh`, owned by the run and mounted only in the tool container. A second proxy credential `<executor>-ssh`,
given only to merge, apply and snapshot Jobs, opens `CONNECT` to port 22 of the SSHTargets (`acl-ssh.conf`,
NetworkPolicy `executor-egress-proxy-ssh`). Manuals: admin.md "Set up an Ansible executor", "Publish server
snapshots". kind e2e: `scripts/e2e-ssh.sh ansible`.
