# 0007: SSH certificate authority, server enrolment and Ansible executor stages

Status: accepted (owner decisions on #204, 2026-11-05: "As proposed"). Implements ADR 0003 D4 and D5.

## Context

Deterministic executors (#69) run fixed tools with write credentials after an approval. Ansible needs SSH to servers.
ADR 0003 decided that only executor Jobs get server access, through short-lived SSH certificates, and that no AI
process ever holds it. The owner chose on #204:

1. step-ca as the CA for servers outside GCE.
2. No server credentials on pull request heads. Check mode against a staging inventory may come later.
3. Static SSH keys in `exec-<name>-apply` are allowed as a deprecated stopgap until the CA is used.

This record fixes the technical choices that the issue left open.

## Decision

### The CA

- **step-ca in `EXEC_NS`**, one replica, Service `step-ca:9000` (TLS from its own X.509 root). NetworkPolicy: only the
  operator may connect. The CA has a user key and a host key for SSH (`step ca init --ssh`).
- **Key ceremony outside the cluster:** `scripts/ssh-ca-init.sh` makes the CA keys with the `step` CLI (local, or the
  pinned `smallstep/step-ca` image) in a temporary directory, writes two Secrets and deletes the directory:
  `step-ca` (CA configuration, keys, password; mounted only in the step-ca pod) and `step-ca-provisioner` (the
  operator's provisioner key and the CA root certificate; read by the operator by name). It never replaces existing
  Secrets. Cloud KMS for the signing keys on GKE is a follow-up; until then the keys are in a Secret on GKE too.
- **One provisioner, `agentdev-operator` (JWK).** The operator signs its own one-time tokens with that key, so it is
  the only requester. Claims of the provisioner: SSH only, user certificates at most 15 minutes, host certificates at
  most 90 days.
- **Certificate content is set by the CA's template, not by the requester:** no extensions (no PTY, no port, agent or
  X11 forwarding), and `source-address` from the token when the instance sets `SSH_SOURCE_CIDRS`. Key ID
  `executor/<run>/<commit>`, `probe/<target>` or `host/<target>`, so sshd logs map to an `ExecutorRun` or a target.

### Enrolment: `SSHTarget`

An admin writes an `SSHTarget` in `EXEC_NS` (only cluster admins can): host, port, executor, the two local users and
the SHA-256 fingerprint of the server's host key, read on the server by a person.

1. The operator reads the host key over SSH and compares it with the fingerprint. Only on a match does it ask the CA
   for a host certificate and store it in the status (`HostSigned`). The fingerprint written by an admin is the
   decision that ADR 0003 D4 step 4 calls "an admin compares fingerprints"; no separate `Approval` object is made,
   because only the same admins could decide it. Instance identity checks (GCE, AWS) are a follow-up.
2. `scripts/ssh-enrol.sh <target>` prints the server snippet from the status: user CA keys (`TrustedUserCAKeys`, several
   keys allowed for rotation), `AuthorizedPrincipalsFile`, `RevokedKeys`, the host certificate, two users, sudoers for
   the apply user only, and the probe command. Also as an Ansible bootstrap playbook and as cloud-init. A person applies
   it with their own access; agentdev never holds bootstrap credentials.
3. The operator probes with a certificate of principal `agentdev-probe` whose `force-command` is the probe script, and
   checks the server's host certificate against the host CA (no trust on first use). Only a successful probe sets
   `Ready`. Probes repeat every 10 minutes; a failure sets `NotReady`.
4. Removal: delete the `SSHTarget`, then the principals files on the server. Certificates live minutes, so nothing
   needs revoking; the KRL (`RevokedKeys`) is the emergency stop for a compromised CA key.

**Two local users:** `agentdev` (principal `agentdev-<executor>-apply`, sudo) and `agentdev-check`
(principals `agentdev-<executor>-check` and `agentdev-probe`, no sudo). sudo cannot tell certificate principals
apart, so the stages differ by user, not only by principal.

### Ansible executor stages (D5)

| Stage | Command | Server credentials |
|---|---|---|
| `pr` (unreviewed head) | `spec.prPlan` (syntax check, lint) | none: no SSH Secret, no proxy rule for port 22 |
| `merge` (reviewed) | `spec.plan` (`--check --diff` to `$(PLAN_FILE)`) | `check` certificate, user `agentdev-check` |
| `apply` (after `/approve`) | `spec.apply` | `apply` certificate, user `agentdev` |

- The operator makes a new key pair for every merge and apply run, signs it (15 minutes) and puts key, certificate,
  `known_hosts` (`@cert-authority`) and an SSH config in a Secret owned by the Job, mounted only in the tool container.
- The Job reaches port 22 only through the executor egress proxy: the proxy credential of the executor opens
  `CONNECT` to its enrolled targets on their SSH port, nothing else. The operator's binary is copied into the Job
  as the `ProxyCommand`.
- Ansible has no plan file to verify. The check output is the plan file: the approval binds to it as for Terraform,
  but the apply is not proven to do what the check showed. The docs say so.

## Consequences

- Admins run one more manual ceremony (`ssh-ca-init.sh`) and enrol each server by hand. That is on purpose.
- A compromise of the operator can mint certificates for every enrolled server for 15 minutes at a time; the
  operator already holds every executor's write credentials (`docs/security.md`).
- Snapshots of server state (#205 D6) build on this flow and follow as a separate issue.

## Alternatives rejected

- **OpenBao SSH engine:** more moving parts, needs unsealing; the owner chose step-ca.
- **Job asks the CA itself:** would give PR code a path to the CA; the operator stays the only requester.
- **Check mode on PR heads:** `check_mode: false` and controller-side plugins run for real (owner decision 2).
- **One local user with two principals:** sudo would apply to the check stage as well.
