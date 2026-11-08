# ADR 0003: Secrets provisioning, dependency caches and server access

- **Status:** accepted (owner answered "as proposed" on #202, #203, #204 and #205, 2026-11-05; see "Owner decisions").
  Brought to `main` with #202 part 1; it was drafted on `claude/secrets-manager-per-user-project-89hbv7`. ADR 0007
  (#204) implements D4 and D5.
- **Issues:** #202 (secrets store), #203 (registry caches), #204 (SSH CA and Ansible executor), #205 (probe
  catalogue and observability tools), #206 (harness guide for agents); related #64 (forge sidecar), #65 (egress), #66 (credential proxy), #67 (approvals),
  #69 (executors), #135 (executor egress), #14 (MCP), #26 (remote control), #157 and #168 (idle cost)
- **Date:** 2026-11-03

## Context

agentdev rests on one rule: **no credential the model can read** (`docs/security-model.md`, "Principle"). Each
credential so far got its own delivery path that keeps it out of the agent container:

| Credential | Stored in | Delivered by | Readable by the agent |
|---|---|---|---|
| Forge token, GitHub App key | Secret `agent-<name>` | forge sidecar, own UID (#64) | no |
| Claude login | `claude-<name>`, `claude-shared` | `claude-proxy`; the agent gets `ANTHROPIC_BASE_URL` and a proxy key (#66) | no (proxy key only, R60) |
| Cloud write access | `exec-<name>-apply`, Workload Identity | executor Job, after an approval (#69) | no |
| Egress | `executor-egress-proxy` | random proxy credential per executor (#135) | scoped |
| Instance secrets | `.secrets/` (kind), GCP Secret Manager (GKE) | Ansible copies them into Secrets | not mounted |

Projects now need more credentials: private package registries, third-party APIs used in tests, servers managed by
Ansible, and read access to server state for the agent that designs changes. Today each new key means a new
Secret name, a CEL rule and an Ansible task, and there is no scope model (instance, project, user, agent). Asking
for "a per-user and per-project secrets manager" raised the question of how values reach execution environments
without breaking the rule above.

Constraints: ready-made before custom (CLAUDE.md §6); kind and GKE at parity (§4); idle cost stays low (#168);
nothing the model prints reaches a control decision and every outside value is pattern-checked (reviewer
checklist items 4 and 5). Egress today allows `CONNECT` to port 443 only (`manifests/egress.yaml.tmpl`,
`manifests/executor-egress.yaml.tmpl`), so SSH does not leave any pod.

## Decision

### D1. A scoped secrets store that decides use, not a value dispenser

Secrets get a **scope** and a **delivery mode**. The store answers "who may use this, and where", it never hands a
value to the agent container by default.

- **Storage:** External Secrets Operator, backed by GCP Secret Manager on GKE and by a Kubernetes Secret store on
  kind. Synced Secret names are fixed by convention and enforced by CEL, as for `agent-<name>` today.
- **Scopes:** `instance` (admin), `project` (project maintainers write; the project's agents and executors use),
  `agent` (one agent), `user` (only sessions the user starts, for example #26; never an autonomous agent, which
  acts as its own forge account and would become a confused deputy across users).
- **Write-only:** the API and UI set, rotate and delete values; they never return them. Every use is a
  SecurityEvent.

### D2. Delivery modes, in order of preference

| Mode | Use | Agent sees the value |
|---|---|---|
| `identity` | GCP, AWS, GitHub through Workload Identity or OIDC federation; short-lived, nothing stored | no |
| `broker` | HTTP APIs and registries: a proxy outside the pod adds the credential per host (generalised `credproxy`, base-URL style; TLS interception only if a tool cannot change its base URL) | no |
| `executor` | write credentials, signing keys, SSH: only executor Jobs, after approval (#69) | no |
| `ssh` | short-lived SSH certificates from the instance CA (D4); executor Jobs only | no |
| `env` | a value a build needs in plain text (a test database password); explicit opt-in, recorded risk, value redacted from approval text | **yes** |

Git over SSH stays off: the forge sidecar pushes over HTTPS with its own policy (`agent/<name>/*`, no force), and
an SSH key would bypass it. The agent loop already scrubs `sshCommand` and `insteadOf` from git config.

### D3. Private dependencies through caching proxies, not keys

The caching registry proxy planned in phase 2 (`docs/security-model.md`, "Not yet: a caching registry proxy")
becomes the way to reach private packages. The upstream credential lives in the cache; the agent only reads
packages it is allowed to read.

- **Instance cache** for public upstreams (npm, PyPI, Go proxy, crates.io), no credentials.
- **Project-scoped repositories** for private upstreams, each with its own pull credential, mapped from the agent's
  egress proxy credential. A single shared cache would leak project A's private packages to project B.
- **Implementation:** Artifact Registry remote repositories on GKE (near zero idle cost, IAM per repository,
  upstream credentials from Secret Manager); Verdaccio (npm), devpi (PyPI) and Athens (Go, also private Go modules
  over git) on kind and on GKE when Artifact Registry is off (#157). One `registry_proxy` setting selects them.
- Read-only. Publishing is an executor job. Arbitrary private git dependencies (submodules, `git+https`) are
  fetched by the forge sidecar from a per-project read allow-list, or vendored.

### D4. Server access through an SSH certificate authority, executors only

SSH to servers is infrastructure write access, so it follows phase 5: only executor Jobs hold it.

- **GCE targets:** OS Login with the apply Job's Workload Identity service account
  (`roles/compute.osAdminLogin` on the target instances) and IAP TCP forwarding over port 443. No stored key.
- **Other servers:** step-ca in `EXEC_NS`. Signing key in Cloud KMS on GKE, in a Secret on kind. Separate user CA
  and host CA keys; executors trust hosts through `@cert-authority`, with no trust on first use. Only the operator
  requests certificates; the key ID is `executor/run/commit`, so sshd logs map to an `ExecutorRun`.
- **Principals:** `agentdev-<executor>-apply`, `agentdev-<executor>-check`, `agentdev-probe`. Certificate lifetime
  in minutes, `source-address` set to the executor egress address, no `permit-pty`, port, agent or X11 forwarding
  except where the stage needs them.
- **Egress:** a per-executor `CONNECT :22` rule for enrolled hosts in the executor proxy, bound to the executor's
  proxy credential as in #135.
- **Enrolment** (manual by design; agentdev never holds bootstrap credentials):
  1. An admin registers an `SSHTarget` (host, port, executor, local user, principals, host key fingerprint).
  2. `scripts/ssh-enrol.sh <target>` prints the server snippet (CA public key, `TrustedUserCAKeys`,
     `AuthorizedPrincipalsFile /etc/ssh/auth_principals/%u`, sudoers for the apply user only, the probe script),
     also as a bootstrap playbook and as cloud-init.
  3. A person applies it with their own access.
  4. The host key is signed: automatically after an instance identity check (GCE, AWS), otherwise after an admin
     compares fingerprints and decides an Approval.
  5. The operator runs an enrolment probe; the target becomes `Ready` only if it succeeds with a valid host
     certificate.
  6. Removal: delete the `SSHTarget`, then the principals file. Short lifetimes make revocation unnecessary.
- **Rotation:** `TrustedUserCAKeys` takes several keys: enrol the next key, switch signing, remove the old key.
  `RevokedKeys` (KRL) is the emergency stop if a CA key is compromised.

### D5. Ansible executor stages

`ansible-playbook --check` is not a safe plan on unreviewed code: a task with `check_mode: false` runs for real,
and `delegate_to: localhost`, lookups and plugins run on the controller, which holds the SSH credential.

| Stage | Runs | Server credentials |
|---|---|---|
| PR head (unreviewed) | `--syntax-check`, ansible-lint, Molecule against throwaway containers where the sandbox allows | **none** |
| merge commit (reviewed) | `--check --diff`, shown as the plan | `check` principal |
| apply, after `/approve` | `ansible-playbook` | `apply` principal, one use |

Ansible has no plan file to hash. The approval binds to the merge commit, playbook path, inventory, image digest
and the Executor spec hash; the check output is information, not a verified plan. This is weaker than Terraform's
guarantee and the docs say so.

### D6. Agents get no SSH; read-only checks through fixed channels

A "read-only" shell is not read-only: it exposes `/etc`, process environments and service keys, it is an egress
bypass, and it invites local privilege escalation, while the agent can be steered by prompt injection. Instead:

| Need | Mechanism | Credential holder |
|---|---|---|
| Logs, metrics, health | MCP tools (ContextForge, #14) over Cloud Logging/Monitoring, Prometheus, Loki | read-only API token in the MCP gateway |
| Config and versions | snapshot executor: scheduled fixed fact gathering, output published for the agent | `agentdev-probe` |
| Drift | nightly `--check --diff` on `main`; drift opens an issue | `check` principal |
| A specific check | probe catalogue (below) | `agentdev-probe` |

**Probe catalogue.** Admins define checks in the Executor spec: name, command template, argument patterns (for
example `service-status`: `systemctl status {unit}`, `unit` matching `^[a-z0-9@._-]{1,64}$`). The agent calls the
MCP tool `server_check(target, check, args)` and supplies only a name and pattern-checked arguments. The API
creates an `ExecutorRun` with stage `probe`; the Job gets a 5-minute certificate with
`force-command=/usr/local/sbin/agentdev-probe`. The server enforces a second time: the probe script matches
`SSH_ORIGINAL_COMMAND` against its own local allow-list and runs as an unprivileged user with read-only sudo for
those commands only. Output is size-limited and redacted like approval text; each probe is a low SecurityEvent,
rate-limited per agent. Checks an admin marks sensitive need an approval.

Probe, snapshot and log output is **untrusted input** for the model and never reaches a control decision.

## Consequences

- New components: External Secrets Operator, a generic credential broker, registry caches, step-ca, CRDs
  `SSHTarget` (or Executor targets) and probe stages. Each lands in its own issue and PR, with Ansible and
  Terraform parity.
- Manuals: `admin.md` gets STE procedures "Set up the SSH certificate authority", "Enrol a server", "Remove a
  server", "Rotate the CA key", "Add a project secret", "Configure the registry cache"; `troubleshooting.md` gets
  sshd failures (principal, clock skew, host certificate); `docs/writing.md` gets the new terms (certificate
  authority, principal, enrol, host certificate).
- New risk register entries in `docs/security.md`: secrets in log and probe output reaching the model (accepted
  residual after redaction); CA signing key compromise (mitigated: KMS, short lifetimes, KRL); `env` mode secrets
  readable by the model (accepted per secret, opt-in).
- Agents must learn these channels exist, or they will look for keys and SSH: #206 generates a read-only harness
  guide per agent (capabilities, executors, probes, caches) from the operator's view.
- The R48 residual (GitHub App key in each sidecar) can be removed by moving installation-token minting into the
  broker.

## Alternatives rejected

- **Secrets as environment variables or files in the agent pod.** Reverses #64 and #66; R4 and R11 come back.
- **SSH agent forwarding into the agent pod.** Protects the key but not its use: the model can sign for any host
  and command the key allows while the socket is mounted. Destination constraints (`ssh-add -h`) depend on client
  support and are defence in depth only.
- **Read-only SSH for agents.** See D6.
- **Static SSH keys in executor Secrets.** Works today without code, kept only as a stopgap: long-lived, no
  per-run attribution.
- **Teleport.** Session recording and access requests, but heavy and overlaps the approvals of #67.
- **Nexus OSS as the only registry cache.** Covers every ecosystem, but a JVM at 2 to 4 GB RAM conflicts with #168.
- **HashiCorp Vault.** Licence (BUSL); OpenBao stays an option if ESO with Secret Manager is not enough.

## Owner decisions

Asked in the issues with `needs-decision`; all answered "as proposed" on 2026-11-05 (recorded in each issue):

1. Scopes for the first release: **`project` and `agent`**; `user` with #26 (#202).
2. `env` mode: **allowed as explicit opt-in** with a recorded risk and redaction (#202).
3. Broker style: **base-URL broker first**; TLS interception only for tools that cannot change their base URL (#202).
4. Registry cache on GKE: **Artifact Registry remote repositories**; in-cluster caches as the fallback (#203).
5. Non-GCE servers: **step-ca** (#204, ADR 0007).
6. Ansible PR stage: **no server credentials** on PR heads (#204).

## Implementation of D1 (#202)

- **`ProjectSecret`** (`agentdev.dev/v1alpha1`, agents namespace): `scope` (`project` with `spec.project`, or
  `agent` with `spec.agent`), `mode` (`broker` with `spec.broker.upstream`, header and scheme; or `env` with
  `spec.env.name` and `riskAccepted: true`). It holds no value. Scope, owner and mode are immutable (CEL).
  `identity`, `executor` and `ssh` need no stored value here (Workload Identity, `exec-*` Secrets, ADR 0007).
- **Names:** the value of ProjectSecret `<n>` is the store key `agentdev-ps-<n>`; External Secrets Operator syncs it
  into the Secret `ps-<n>` (key `value`) in the agents namespace. The operator writes one `ExternalSecret ps-<n>` per
  ProjectSecret and never reads the value.
- **Stores:** a namespaced `SecretStore agentdev-secrets`: GCP Secret Manager with Workload Identity on GKE (the
  reader may only access `agentdev-ps-*`), the Kubernetes provider over the namespace `agentdev-secret-values` on
  kind. No ClusterSecretStore.
- **CEL (ValidatingAdmissionPolicy):** an ExternalSecret in the agents namespace must be `ps-<n>`, use only
  `agentdev-secrets`, and map exactly `agentdev-ps-<n>` to key `value` (no `dataFrom`, no templates with data);
  `ps-*` Secrets are written only by External Secrets Operator; no agentdev service account writes a secret store.
