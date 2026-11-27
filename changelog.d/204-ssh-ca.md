---
issue: 204
type: feature
section: Added
status: done
title: SSH certificate authority and server enrolment for executors
components: [operator, manifests, ansible, scripts]
pr: 264
---
## Summary

step-ca in its own namespace `SSH_CA_NS` (`agentdev-ssh-ca`, never `EXEC_NS`) signs short-lived SSH certificates only for the operator (its JWK key is the only provisioner,
user certificates at most 15 minutes, no extensions). New CRD `SSHTarget`: the operator signs a server's host key
only when it matches `spec.hostKeyFingerprint` and sets `Ready` after a probe (no trust on first use).
`scripts/ssh-ca-init.sh` is the key ceremony, `scripts/ssh-enrol.sh` prints the server snippet. Docs: ADR 0007,
`docs/admin.md` "SSH access for executors"; risks R133, R134.

## Description

Key IDs are `executor/<run>/<commit>`, `probe/<target>` or `host/<target>`. The ceremony writes Secrets `step-ca`
and `step-ca-provisioner` and never replaces them (`--out`/`--from` for rotation). The enrolment snippet (shell,
`--format ansible`, `--format cloud-init`) creates users `agentdev` (apply, sudo) and `agentdev-check` (check and
probe, no sudo), principals files, `TrustedUserCAKeys`, an empty KRL and sshd settings. Ansible: `step_ca_image`
(GKE) and `operator_step_ca_image` (kind tag `operator`); namespace `ssh_ca_ns` / `operator_ssh_ca_ns`. The operator may get only `step-ca-provisioner` there, never the CA keys. kind e2e: `scripts/e2e-ssh.sh` with
`tests/e2e/ssh-server`.

## Upgrade

Apply the `operator` tag (kind) or the playbook (GKE) again: it adds the `SSHTarget` CRD and the step-ca
Deployment, which stays idle until an admin runs `scripts/ssh-ca-init.sh`.
