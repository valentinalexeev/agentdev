# Administration

This guide is for the administrator of an agentdev instance. The local paths refer to the `kind` installation
(`kind.vars.yml`, `.env`, `.secrets/`, the playbook `infra/ansible/kind.yml`). On GKE, the same settings are Ansible
variables in the deployment repository (`ansible/instance.yml`). On GKE, the secrets are in Secret Manager.

On `kind`, this guide gives a setting by its former `config.env` name, for example `AGENTS`. Set its Ansible variable
in `kind.vars.yml`. The file `infra/ansible/kind.example.vars.yml` shows the Ansible variable for each `config.env`
name. To deploy one part again on `kind`, run the playbook with the tag of that part, for example:
`ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags agents`. Before each playbook run, load the
secrets: `set -a; source .env; set +a`.

## Projects

A project is one forge repository with its agents. The bootstrap script connects a repository that exists.

### Add a project

1. Copy the example specification:
   `cp examples/project.yaml my-project.yaml`
2. In `my-project.yaml`, set `forge.repo`, `forge.owner` and `agents`.
3. Run the bootstrap: `scripts/bootstrap-project.sh my-project.yaml`
   Each agent writes **ready** on the issue "agentdev: project connected".

NOTE: You can run the bootstrap again with no risk.

### Reference: the bootstrap

The bootstrap does these actions:

- It makes the agent accounts and the forge tokens.
- It sets branch protection: only the owner can approve. It keeps a rule that exists. `REPLACE_BRANCH_PROTECTION=1`
  replaces the rule.
- It adds the labels.
- It proposes the process pack in a pull request.
- It deploys the agents and waits for the **ready** comment of each agent.

Limits of v1:

- Only Gitea.
- The issues are on the same forge.
- One repository for each agent deployment.

## Agents

### Add an agent

1. On `kind`: add the agent to the agent list (`AGENTS`) in `kind.vars.yml`.
   On GKE: add the agent to the `agents:` list in `instance.yml`.
2. On `kind`: run `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags forge,agents`.
   On GKE: open a pull request in the deployment repository.

### Remove an agent

CAUTION: Step 3 deletes the work data of the agent.

1. Remove the agent from the agent list (`AGENTS` or `agents:`).
2. Deploy again: `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags agents` on `kind`, or a pull
   request in the deployment repository.
3. Delete the StatefulSet, the volumes and the forge account of the agent.

### Keep the agents in Git (Flux)

With the setting `agents_source: git`, Flux applies the `Agent` objects from the deployment repository (#58). The
default is `api`: the playbook and the API write the `Agent` objects.

In `git` mode:

- An agent from Git is read-only in the API and the web UI. Only **Pause** and **Resume** are possible.
- An agent that a person adds in the web UI or the API stays owned by the API. Flux does not prune it.
- The playbook makes the Secrets of the agents. It does not apply the `Agent` objects.

Each field has one owner:

| Field | Owner |
|---|---|
| `spec.repo`, `spec.forge`, `spec.claudeSecret`, `spec.maxConcurrent`, `spec.maxTurns`, `spec.permission`, `spec.tools`, `spec.toolchain`, `spec.scaleToZero` | Git (Flux) |
| `spec.paused` | the API (field manager `agentdev-api`) and the security pause (field manager `agentdev-operator`) |
| replicas of the StatefulSet | the router. The StatefulSets are not in Git. |
| `status` | the operator |

CAUTION: Do not put `spec.paused` in the Git manifest. If you do, Flux owns the field and resumes an agent that the
security pause stopped.

1. Set `agents_source: git` in `instance.yml`. On `kind`, set `AGENTS_SOURCE` to `git` in `kind.vars.yml`.
2. Put one `Agent` manifest for each agent in the `agents/` directory of the deployment repository:
   ```yaml
   apiVersion: agentdev.dev/v1alpha1
   kind: Agent
   metadata: {name: alice, namespace: coding-agents}
   spec:
     repo: my-org/my-repo
     forge: github
     claudeSecret: claude-shared
     maxConcurrent: 2
     scaleToZero: true
     # no spec.paused: the API and the operator own it
   ```
3. Keep the agent in the `agents:` list of `instance.yml`. The playbook makes its Secrets.
4. Add a Flux `Kustomization` for `agents/` (see "Health checks for Flux and Argo CD").
5. Deploy again.

NOTE: On `kind`, the playbook tag `agents` applies the `Agent` objects in both modes. It does the work of Flux.

For Argo CD, add `ignoreDifferences` for `spec.paused`, or `selfHeal` resumes a paused agent:

```yaml
spec:
  ignoreDifferences:
    - group: agentdev.dev
      kind: Agent
      jsonPointers: [/spec/paused]
```

### Give an agent its own Claude credential

WARNING: Do not put the credential in git or in an issue.

1. Get a credential for the Claude subscription: `claude setup-token`.
2. On `kind`: write `CLAUDE_CODE_OAUTH_TOKEN=...` or `ANTHROPIC_API_KEY=...` in `.secrets/<agent>.claude`.
   On GKE: write the `KEY=VALUE` lines in Secret Manager `agent-<agent>-claude`.
3. Deploy again: on `kind`, run the playbook with the tag `agents`. On GKE, open a pull request in the deployment
   repository.

NOTE: Each agent uses one Claude credential. It uses its own credential if it has one. If not, it uses the shared
credential (#5). The shared credential is `.env` on `kind` and Secret Manager `claude-shared` on GKE.

NOTE: Only the Claude proxy reads the credential. The agent pod gets a proxy key, not the credential (#66).

### Replace a credential

Use this task to replace a Claude credential or a forge token.

1. Replace the file or the secret.
2. Deploy again: on `kind`, run the playbook with the tag `agents`. On GKE, open a pull request in the deployment
   repository.

NOTE: With `external_secrets.mode: external` on GKE (#381), step 2 is not needed. Add a new version of the
Secret Manager secret. The External Secrets Operator copies it within `external_secrets_refresh` (default 15
minutes), and Reloader restarts the pods that use it. An agent pod restarts at its next scale to zero or drain, not
in the middle of a job. The Claude proxy refuses to start when `claude-shared` has no `CLAUDE_CODE_OAUTH_TOKEN=` or
`ANTHROPIC_API_KEY=` line. It reports an invalid `agent-<agent>-claude` as a security event.

### Read the secrets from Secret Manager without the playbook (GKE, #381)

In `external` mode, no secret value passes through the operator machine or Ansible. Terraform generates the random
secrets once in Secret Manager. The External Secrets Operator copies each one into a Kubernetes Secret with the same
name and keys as before. Stakater Reloader v2 (a pinned beta, owner decision on #381) restarts the workloads that
carry `secret.reloader.stakater.com/reload`.

1. In `terraform/instance.tfvars`, set `external_secrets = true` and set `agent_names` to the names of `agents` in
   `ansible/instance.yml`. With the default `false`, Terraform makes none of these secrets or accounts.
2. For an upgrade from 1.1.x, the playbook already made the generated secrets. List them in
   `keep_existing_secret_values` and import them (`terraform/imports.tf.example` of the deployment template). If you
   skip this step, Terraform writes new values, the databases keep the old passwords, and the platform breaks.
3. Apply Terraform. `terraform output secrets_readers` shows the Google service accounts of the namespaces.
4. Make sure that the secrets you supply exist in Secret Manager: `claude-shared`, `router-register-token`,
   `github-app-*`, `agent-<agent>-forge-token`, and `agentdev-api-vapid-*` if Web Push is on. External mode does not
   create them.
5. In `ansible/instance.yml`, set `external_secrets: {mode: external}`. Set `own_claude: true` on each agent that has
   `agent-<agent>-claude`. Deploy.

WARNING: The Terraform state now holds the generated values. Only the deployment identities may read the state bucket.

NOTE: A new version of a database password does not change the password in Postgres yet (#382).

### Set the concurrency

The concurrency is the number of items that one agent works on at the same time.

1. On `kind`: set `MAX_CONCURRENT` (the default is 2).
   On GKE: set `max_concurrent`, or `agents[].max_concurrent` for one agent.
2. Deploy again.

The turn limit is the maximum number of turns of one Claude run (#336). When a run uses all its turns, the agent
writes "class `max-turns`" on the PR and on the issue. A large task can need more turns.

1. On `kind`: set `MAX_TURNS` (the default is 40, the maximum is 500).
   On GKE: set `max_turns`, or `agents[].max_turns` for one agent.
2. Deploy again.

NOTE: The memory limit of an agent is (1 + concurrency) GiB. At the default of 2, the limit is 3 GiB. The operator
sets the limit.

### Set the permission mode of an agent

The permission mode decides a tool call that is not on the allow-list of the agent (#500). Each agent has its own mode:

- `dontAsk` (default): Claude Code refuses the call. With approvals on, a project member must approve the call.
- `auto`: a classifier of Claude Code decides the call. The classifier uses its default rules and your rules. With
  approvals on, a project member must approve a call that the classifier blocks.

The deny-list of the worker (`git push`, `curl`, `/state` and others) stays in both modes. `bypassPermissions` is
not available.

WARNING: The classifier is the judgment of a model. It is not a security boundary. Use `auto` only for agents that
you trust with packages that the classifier installs (threat model V6).

1. On `kind`: set `kind_agents_permission` for the agent, for example
   `{alice: {mode: auto, autoMode: {allow: ["Install dependencies declared in the repository"]}}}`.
   On GKE: set `agents[].permission` for the agent, for example `{mode: auto}`.
   In Git mode: set `spec.permission` in the `Agent` manifest.
2. Optional: add rules in `autoMode`. Use `allow`, `softDeny` and `environment`, one rule on each line.
3. Deploy again.
4. Do the command `claude auto-mode defaults` in an agent pod to see the default rules.

NOTE: The rules come from the `Agent` object only. The repository and the issue text cannot change them. The
classifier sends more requests through `claude-proxy`. They count against the token budget of the agent.

To see the mode and the rules of an agent, open the agent in the console and select the **Permission** tab. The
**Overview** tab shows the mode too. Only `agentdev-admins` members see them, and the console does not change them.
The API also returns them in `GET /api/agents/{name}` (`permission`).

Each change of `spec.permission` makes one entry in the audit log (#505): action `permission`, with the old and the
new mode and rules in `before.permission` and `after.permission`. Open the **Audit** page and select the action
`permission`, or do `curl -H "Authorization: Bearer <token>" "https://<api host>/api/audit?agent=<name>"`. The API
does not know who changed the `Agent` object, so the entry has no person. Look in the Git history or the Kubernetes
audit log for the person. The API records a change that happened while it was stopped when it starts again. Two
API processes write one entry for one change.

If the audit log has no earlier `permission` entry of the agent, and the mode is not the default (`dontAsk` without
rules), the entry has `before: null` and the reason `first seen (no earlier entry)`. This occurs for a new agent,
after the retention removed the old entries, or after a restart of an API without a database. The console shows
`unknown → <new mode>`.

NOTE: The `AGENTDEV_AUDIT` line in the container log has only the mode, the number of rules of each list and a
sha256 of the rules, not the rule text. The full rules are only in the audit log of the API (`agentdev-admins`).

### Allow extra tools for an agent

The allow-list of the worker has the tools that all agents can use without a question (#496). You can add tools to
the list for one agent, for example `npm ci` for a repository that needs `node_modules`:

- The entries are added to the default list, or to `CLAUDE_TOOLS` of the instance. You cannot remove or replace
  entries of the default list.
- The deny-list of the worker (`git push`, `curl`, `/state` and others) stays.
- Use the Claude Code rule syntax, one rule in each entry, for example `Bash(npm ci:*)`. An entry has no commas and
  no control characters, and is not longer than 200 characters. An agent has at most 50 entries.

WARNING: Each entry widens what the agent can do without a question. `npm install` runs the install scripts of
packages: a compromised dependency then runs in the agent pod (threat model V6). Add an entry only for an agent and
a repository that need it.

1. On `kind`: set `kind_agents_tools` for the agent, for example `{alice: ["Bash(npm ci:*)", "Bash(npm install:*)"]}`.
   On GKE: set `agents[].tools` for the agent, for example `{allow: ["Bash(npm ci:*)"]}`.
   In Git mode: set `spec.tools.allow` in the `Agent` manifest.
2. Deploy again. The agent pod restarts with the new list.
3. Do the command `kubectl -n coding-agents get statefulset agent-<name> -o yaml | grep -A1 CLAUDE_TOOLS_EXTRA` to
   see the added entries.

NOTE: The entries come from the `Agent` object only. The repository and the issue text cannot change them. The
operator refuses an `Agent` with an invalid entry (phase `invalid`). An agent without entries has no
`CLAUDE_TOOLS_EXTRA`, so its pod does not restart on upgrade.

### Give an agent a toolchain

An agent can get a language toolchain that the worker image does not have, for example Go or Rust (#499). agentdev
installs it before the agent starts, and the agent can use it but not change it.

- The presets and their versions are in the operator (`images/operator/toolchain.go`). Each download has a pinned
  SHA-256. A new version is an agentdev release.
- The init container `toolchain` installs the toolchain into `/state/toolchain` on the agent volume. The agent
  container mounts that directory read-only, and gets it on its search path.
- A wake only checks the installed files. A change of the toolchain, of the worker image or of the refresh period
  installs again, after the running jobs end.
- The commands of the preset (for example `go mod`, `cargo build`) are added to the tool allow-list of the agent.

| Preset | Versions | Download hosts | Agent environment |
|--------|----------|----------------|-------------------|
| `go` | 1.27.4, 1.26.11 | `dl.google.com` | `GOTOOLCHAIN=local`, `GOPATH` and `GOMODCACHE` under `/state/cache/go` |
| `rust` | 1.100.0 (rustup, profile minimal) | `static.rust-lang.org` | `RUSTUP_HOME` read-only, `CARGO_HOME=/state/cache/cargo` |

1. On `kind`: set `kind_agents_toolchain` for the agent, for example `{alice: {presets: {go: "1.27.4"}}}`.
   On GKE: set `agents[].toolchain` for the agent, for example `{presets: {go: "1.27.4", rust: "1.100.0"}}`.
   In Git mode: set `spec.toolchain` in the `Agent` manifest.
2. Optional: set `refresh`, for example `168h` (at least `1h`), to install again regularly. Each agent has its own
   offset, so the agents do not all install at the same time.
3. Optional: add `path` entries below `$TOOLCHAIN`, for example `$TOOLCHAIN/go/bin`.
4. Make sure the agent volume is large enough (see below).
5. Deploy again.
6. Do the command `kubectl -n coding-agents get agent <name> -o jsonpath='{.status.toolchain}'` to see the result.
   `reason` is `Installed` or `Verified` when the toolchain is ready.

**Disk.** The toolchain and the caches are on the agent volume (`AGENT_STATE_STORAGE`, default 10 GiB):

| Toolchain | Toolchain size | Caches (modules, build) | Volume |
|-----------|----------------|-------------------------|--------|
| Go | about 250 MB | 1 to 3 GB | 10 GiB is enough |
| Rust | about 550 MB | 2 to 6 GB (`CARGO_HOME`, `target/`) | 20 GiB or more |

A new size applies to new agents. To grow the volume of an existing agent, resize its PVC `state-agent-<name>-0`
(the StorageClass must allow volume expansion).

**When the install fails.** The pod stays in `Init`, so the agent does not run without its toolchain.
`status.toolchain.reason` is `StepFailed` (a download, a checksum or the check of the toolchain failed) or `Invalid`
(for example a symbolic link in `/state/toolchain`). `message` has the details. Do the command
`kubectl -n coding-agents logs agent-<name>-0 -c toolchain` to see the full output.

WARNING: A `Reinstalled` reason means that the installed files changed since the last start. The operator records
a high-severity security event `toolchain-manifest`. Find out what changed it (see "Find the security events of an
agent").

NOTE: The download hosts are open only for the init container, with the credential `egress-setup-proxy` (generated
by the playbook, or by Terraform with `external_secrets`). The agent container cannot use them. The toolchain comes
from the `Agent` object only. The repository and the issue text cannot change it.

### Set the poll interval

Agents read the forge at this interval when webhooks do not operate.

1. On `kind`: set `POLL_SECONDS`. On GKE: set `poll_seconds`.
2. Deploy again.

### Set the agent volume

Each agent has one volume (PVC `state-agent-<name>-0`). The volume keeps the clones, the cursors and the Claude
state (`/home/node/.claude` is the `claude` directory of the volume).

1. On `kind`: set `AGENT_STORAGE_CLASS` and `AGENT_STATE_STORAGE` in `kind.vars.yml`, then run the playbook with the tag `operator`.
   On GKE: set `agent_storage_class` and `agent_state_storage` in `instance.yml`, then deploy.
2. A new size applies to new agents only. To make the volume of an agent larger, edit its PVC.

NOTE: On GKE, Ansible creates the StorageClass `agentdev-agent` (`pd-standard`, the cheapest disk type). GCE bills each
disk at 10 GB or more, so the default size is 10Gi. On `kind`, the class is empty: the cluster default (`standard`).

CAUTION: When the StorageClass or the volume layout changes, the operator deletes the StatefulSet and the volumes of
each agent and makes them again. The agent loses its local state. Open branches stay on the forge. Do this change
only when the agents have no job.

### Find the security events of an agent

1. Run this command:
   `kubectl -n coding-agents logs agent-<name>-0 --all-containers | grep AGENTDEV_SECURITY_EVENT`
   The output shows one JSON line for each action that a guardrail stopped.

### See the harness guide of an agent

The operator writes a harness guide for each agent (#206). The guide tells the agent what agentdev gives it and what
it does not give it. The loop adds the guide to the system prompt of each Claude run.

1. Run this command:
   `kubectl -n coding-agents get configmap agentdev-guides -o jsonpath='{.data.<name>}'`
   The output shows the guide of the agent `<name>`.
2. To change the list of hosts in the guide, change the egress allow-list. Refer to *Egress allow-list*.
   Then deploy the operator again: on `kind`, run the playbook with the tag `operator`. On GKE, run the Ansible playbook.
   The operator writes the guides again when it starts.

The fixed part of the guide is in the operator image. You cannot edit it.

All guides are in one ConfigMap. A ConfigMap can contain 1 MiB. A guide with the default settings is approximately
2 KB, and its maximum size is 6000 bytes. Thus one instance can have approximately 170 agents at the maximum size.

### Reference: the agent pod

| Item | Value |
|---|---|
| Containers | `agent` (the loop and Claude, no forge credentials, no Claude credential: only a proxy key, #66) and `forge` (the sidecar that keeps the forge token, image `agentdev-forge`, #64) |
| Log of the sidecar | `kubectl -n coding-agents logs agent-<name>-0 -c forge` |
| The sidecar refuses a `git push` | to a branch outside `agent/<name>/*`; that touches `PROTECTED_PATHS` (default: the workflow directories); of more than `MAX_PUSH_MB` (50) |
| Security events | each refusal (in the sidecar or the loop) is an `AGENTDEV_SECURITY_EVENT` JSON line. Rules: [security-model.md](security-model.md#phase-1-forge-sidecar-fixes-r4) |
| Egress | through `egress-proxy` only (`HTTPS_PROXY`), to the hosts of the allow-list. Refer to *Egress allow-list* |
| Harness guide (#206) | ConfigMap `agentdev-guides`, key `<name>`, mounted read-only at `/etc/agentdev/guide/guide.md` in the `agent` container only |
| Sidecar image | on `kind`, the playbook tag `agents` builds it (`FORGE_IMAGE`). Ansible uses `forge_image` (the same `image_tag`) |

| Setting | `kind` | GKE (Ansible) |
|---|---|---|
| Agent list | `AGENTS` in `kind.vars.yml`, then the playbook tags `forge,agents` | `agents:` in `instance.yml` |
| Own Claude credential | `.secrets/<agent>.claude` | Secret Manager `agent-<agent>-claude` (`KEY=VALUE` lines) |
| Shared Claude credential | `.env` | Secret Manager `claude-shared` |
| Concurrency | `MAX_CONCURRENT` (default 2) | `max_concurrent`, `agents[].max_concurrent` |
| Turn limit of one Claude run (#336) | `MAX_TURNS` (default 40) | `max_turns`, `agents[].max_turns` |
| Poll interval (when webhooks do not operate) | `POLL_SECONDS` | `poll_seconds` |
| Cost labels on namespaces, agent and executor pods (#184) | `COST_LABELS` (default `service=agentdev,environment=dev`) | `agentdev_labels` (plus `service: agentdev`) |
| Agent StorageClass | `AGENT_STORAGE_CLASS` (empty: cluster default) | `agent_storage_class` (default `agentdev-agent`, `pd-standard`) |
| Agent volume size | `AGENT_STATE_STORAGE` (default `10Gi`) | `agent_state_storage` (default `10Gi`) |
| Agent container ephemeral-storage request (#251) | `AGENT_EPHEMERAL_REQUEST` (default `1Gi`) | `agent_ephemeral_request` (default `1Gi`) |
| Agent container ephemeral-storage limit | `AGENT_EPHEMERAL_LIMIT` (default `8Gi`) | `agent_ephemeral_limit` (default `8Gi`) |
| Forge sidecar git mirror size limit | `FORGE_MIRROR_LIMIT` (default `4Gi`) | `forge_mirror_limit` (default `4Gi`) |
| Forge sidecar ephemeral-storage limit (at least the mirror limit) | `FORGE_EPHEMERAL_LIMIT` (default `5Gi`) | `forge_ephemeral_limit` (default `5Gi`) |

## Claude proxy and budget

The Claude proxy (`claude-proxy`) is the only pod that has the Claude credentials (#66). An agent sends its model
requests to the proxy with its proxy key. The proxy adds the real credential and sends the request to the Anthropic
API. The proxy also counts the spend of each agent and stops an agent that used its budget.

### Cost units

The budget counts **cost units**, not raw tokens (#196, [ADR 0002](adr/0002-llm-telemetry-and-cost-control.md)).
One unit is the price of one input token at 3 USD per million (Sonnet 4.x). The proxy weights each token type and
the model of the response:

| Token type | Weight |
|---|---|
| input | 1 |
| cache write, 5 minutes | 1.25 |
| cache write, 1 hour | 2 |
| cache read | 0.1 |
| output | 5 |

Model multipliers (input price / 3 USD, list prices of November 2026): Opus 5.5 1.33, Opus 4.5 to 5 1.67, Opus 4
and 4.1 5, Sonnet 4.x 1, Sonnet 5 and 5.5 0.67, Haiku 4.5 0.33, Haiku 5.5 0.03, Fable 5.x 3.33. A family name (for
example `claude-opus`) covers new versions at the current family price. A model outside every family counts with
the highest multiplier. The token weights are the same for every model; some models price cache reads lower (Opus
5.5: 0.05), so the units of those models are a little high. Change the multipliers in `claude-proxy-prices` when
Anthropic changes its prices. Claude Code reads most of its context from the cache, so a run uses far fewer units
than raw tokens (often less than 20 %).

### Set the budget of all agents

The budget is the number of cost units that one agent can use in 24 hours.

1. On `kind`: set `CLAUDE_BUDGET_UNITS` in `kind.vars.yml`. On GKE: set `claude_budget_units` in `instance.yml`.
2. Deploy again: on `kind`, run the playbook with the tag `agents`. On GKE, open a pull request in the deployment
   repository.

NOTE: The value 0 means no limit. This is the default.

NOTE: `CLAUDE_BUDGET_TOKENS` and `claude_budget_tokens` are deprecated since 1.2.0 and will be removed in 2.0.0.
Until then they still work: when only the old variable is set, its value is used as cost units (1 token = 1 unit).
When both are set, `CLAUDE_BUDGET_UNITS` / `claude_budget_units` wins. The proxy and the playbook show a
warning. To migrate, rename the variable. A budget in units allows more traffic than the same number of
tokens, so you can lower the value (see the CHANGELOG, 1.2.0 Deprecated).

### Set the budget of one agent

1. Run this command (use 0 for no limit):
   `kubectl -n coding-agents patch configmap claude-proxy-budgets --type merge -p '{"data":{"<agent>":"500000"}}'`
2. Wait one minute. Kubernetes updates the file in the proxy pod, and the proxy reads it at the next request.

NOTE: A deploy does not change this value. The template of the ConfigMap has no data.

### Change the weights or show USD

The ConfigMap `claude-proxy-prices` holds the weights. Keys: `w_in`, `w_cw5`, `w_cw1h`, `w_cr`, `w_out`,
`model.<name prefix>` (the longest prefix wins) and `usd_per_million_units`. Each value is a number with at most
6 decimals. The proxy ignores a bad key or value, keeps the built-in value and writes a `proxy-price-invalid`
security event.

1. On `kind`: set `CLAUDE_PRICES` in `kind.vars.yml`, for example to `usd_per_million_units=3`. On GKE:
   set `claude_prices` in `instance.yml`, for example `claude_prices: {usd_per_million_units: 3}`. With 3, the USD
   estimate follows the list prices, because one unit is priced at 3 USD per million.
2. Deploy again. The proxy reads the ConfigMap again within one minute.

NOTE: USD is an estimate. On a subscription the bill is fixed, and units are a fair share between agents. Set
`usd_per_million_units` only when the instance bills by API key. Without it, the web UI shows units only.

### Limit the cost of one job

A job is the work of an agent on one issue or one pull request. The proxy can stop a job that uses too many cost
units (#196, #310). There are two limits per job:

- The **soft limit** (`CLAUDE_JOB_APPROVAL_UNITS`). At this limit the agent stops and asks a person. The agent adds
  a request with the source `cost` to the approvals comment on its issue or pull request. One approval gives the job
  one more step of units (`CLAUDE_JOB_APPROVAL_STEP_UNITS`). Then the agent continues in the same Claude session.
- The **hard limit** (`CLAUDE_JOB_BUDGET_UNITS`). At this limit the job stops. An approval cannot change it.

1. On `kind`: set `CLAUDE_JOB_APPROVAL_UNITS`, `CLAUDE_JOB_APPROVAL_STEP_UNITS` and `CLAUDE_JOB_BUDGET_UNITS` in
   `kind.vars.yml`. On GKE: set `claude_job_approval_units`, `claude_job_approval_step_units` and
   `claude_job_budget_units` in `instance.yml`.
2. On `kind`, set `APPROVALS` to `true`. On GKE, approvals are on by default.
3. Deploy again: on `kind`, run the playbook with the tags `agents,operator`. On GKE, open a pull request in the
   deployment repository.

NOTE: The value 0 means off. This is the default for all three settings. A step of 0 is the same as the soft limit.

NOTE: The soft limit must be lower than the hard limit when you set both. The playbook and the proxy stop with an
error if it is not.

NOTE: A denial, a void request or no answer within the approval timeout (`APPROVAL_TIMEOUT_MINUTES`, default 30)
ends the job as failed, as at the hard limit.

NOTE: Only the proxy raises the limit. It reads the allowed approvals of the job from the Kubernetes API (Role
`agentdev-claude-proxy-approvals`: `get` and `list` on `approvals`). The worker and the model cannot raise it.

NOTE: The job id comes from the agent (`X-Agentdev-Job`). Thus the limits per job stop a run that loops. They are
not a security limit. The budget of the agent (24 hours) is the security limit.

### Look at the spend

1. Open the web UI. The column **Claude units (24 h)** shows the units of each agent and the budget.
2. Or run this command:
   `kubectl -n coding-agents exec deploy/webhook-router -- node -e "require('http').get('http://claude-proxy:8080/status',r=>r.pipe(process.stdout))"`

   `budgets` lists units per agent. `jobs` lists each job (issue or pull request) of the last 7 days with its
   units, tokens by type and number of requests, most expensive first. The worker names the job in the header
   `X-Agentdev-Job`. The agent can change this header, so the job list is a report, not a limit.
   `weights` lists the weight of each token type. Each `AGENTDEV_USAGE` line of the proxy has `tokens` (by type),
   `multiplier` and `units`. Thus you can calculate the units again: units = sum(tokens x weight) x multiplier,
   rounded up.

### Metrics and OpenTelemetry

`claude-proxy` and the operator serve OpenTelemetry metrics in the Prometheus format on port 9464, path `/metrics`
(#196, ADR 0002 D4). A NetworkPolicy lets only the scraper pods reach this port: the Prometheus server in
`monitoring` on kind (`TELEMETRY_SCRAPER_NS`, `TELEMETRY_SCRAPER_SELECTOR`), the GMP collector in `gmp-system` on GKE
(`telemetry.scraper_ns`, `telemetry.scraper_selector`). Other pods of that namespace and agent pods cannot reach it.
If you use another scraper, set both: the namespace and the labels of its pods.

| Metric (Prometheus name) | What it shows |
|---|---|
| `gen_ai_client_token_usage` | tokens per request, by `gen_ai_token_type` (`input`, `output`) and `agentdev_cache` (`read`, `write`) |
| `gen_ai_client_operation_duration_seconds` | time of a model request |
| `agentdev_cost_units_total` | cost units by agent and model |
| `agentdev_cost_usd_total` | estimated USD, only when `usd_per_million_units` is set |
| `agentdev_budget_used`, `agentdev_budget_limit` | units of the agent in 24 hours, and its budget |
| `agentdev_ratelimit_utilization` | last 5 h and 7 d utilization of each subscription (0 to 1) |
| `agentdev_requests_refused_total` | refused requests by `agentdev_reason` (`budget`, `job_budget`, `rate_limited`, `bad_key`) |
| `agentdev_usage_paused` | operator: 1 while an agent is paused by a usage limit, by reason |

Every series has the label `agentdev_instance`. It is the name of the agentdev instance (default `agentdev`). With
it, one external Prometheus and Grafana can show many instances.

1. On `kind`: set `TELEMETRY_INSTANCE` in `kind.vars.yml`, then run the playbook with the tags `agents,operator`.
2. On GKE: set `telemetry: {instance: <name>}` in `instance.yml`, then run the playbook.

The name is a DNS label: `a-z`, `0-9` and `-`, at most 63 characters.

The metrics have no job label and no prompt content. To send metrics and spans (one `chat` span per model request,
with the job id) to an OTLP backend such as Langfuse, Phoenix or an OpenTelemetry Collector, set
`TELEMETRY_OTLP_URL` in `kind.vars.yml` (kind) or `telemetry.otlp_url` in `instance.yml` (GKE). The URL must be
`https://`. Then run the playbook (on kind, with the tags `agents,operator`).

### Reference: the Claude proxy

| Item | Value |
|---|---|
| Image | `agentdev-credproxy`. On `kind`, the playbook tag `agents` builds it (`CLAUDE_PROXY_IMAGE`). Ansible uses `credproxy_image` |
| Credentials | the Secrets `claude-shared` and `claude-<agent>`, mounted only in the proxy pod |
| Proxy key of an agent | the Secret `agent-<agent>-proxy`, key `ANTHROPIC_AUTH_TOKEN`. It is the agent name and an HMAC of the name |
| Token of an agent (#352) | a projected service account token of `agent-<agent>`, audience `claude-proxy`, 10 minutes. Only in the `agent` container, at `/var/run/secrets/agentdev/proxy/token` |
| Accepted identity | `CLAUDE_PROXY_AUTH` (`kind`) or `claude_proxy_auth` (GKE): `key`, `both` (default) or `token` |
| Access to the API server | the proxy has the service account `claude-proxy`, bound to the ClusterRole `agentdev-claude-proxy-tokenreview` (`tokenreviews` `create` only). Its NetworkPolicy allows only the addresses of the Service `default/kubernetes` |
| Signing key | the Secret `claude-proxy-signing`. On `kind`: `.secrets/claude-proxy-signing.key`. On GKE: Secret Manager `claude-proxy-signing` |
| Forwarded requests | `POST /v1/messages` and `POST /v1/messages/count_tokens` only |
| When the budget is used | the proxy gives HTTP 429 and writes one `budget-exhausted` security event. The agent continues when the 24-hour window has free tokens |
| Usage of the subscription | the proxy reads the rate-limit headers of each response and writes `AGENTDEV_USAGE` lines (#93) |
| Old Claude login on the volume | when the proxy is on, the agent removes `~/.claude/.credentials.json` and the keys in `~/.claude.json` at each start |
| Usage of the subscription (#93) | only from the proxy. The forge sidecar refuses `POST /usage` |
| Turn off the proxy | `CLAUDE_PROXY=false` (`kind`) or `claude_proxy: false` (GKE). This is deprecated: the credential is then in the agent pod again (R11) |

### Agent identity at the Claude proxy (#352)

Each agent has its own Kubernetes service account `agent-<agent>`. The operator makes it. The agent container gets a
token of this service account with the audience `claude-proxy`. The token expires after 10 minutes, and Kubernetes
renews it. The proxy checks the token with the Kubernetes TokenReview API.

The setting `CLAUDE_PROXY_AUTH` (`kind`) or `claude_proxy_auth` (GKE) selects what the proxy accepts:

| Value | The proxy accepts |
|---|---|
| `key` | only the proxy key (the behaviour before 1.1) |
| `both` | the proxy key or the token. This is the default |
| `token` | only the token. The proxy refuses the proxy key |

A worker of 1.1 or later sends the token. An older worker sends the proxy key.

To accept only the token:

1. Make sure that each agent runs a worker of 1.1 or later:
   `kubectl -n coding-agents get pods -l app=coding-agent -o jsonpath='{range .items[*]}{.metadata.name} {.spec.containers[0].image}{"\n"}{end}'`
2. Make sure that no `proxy-key-invalid` security event shows `wrong agent token` after the upgrade.
3. On `kind`: set `CLAUDE_PROXY_AUTH` to `token` in `kind.vars.yml`. On GKE: set `claude_proxy_auth: token` in `instance.yml`.
4. Deploy again: on `kind`, run the playbook with the tag `agents`. On GKE, open a pull request in the deployment
   repository.
5. Run `scripts/e2e-gke-security.sh`. Make sure that the proxy key check shows PASS.

CAUTION: Do not set `token` while an agent runs an older worker. The proxy then refuses all calls of that agent.

To go back, set `both` and deploy again.

WARNING: Do not give the proxy key of an agent to a person. The key lets the holder use the Claude budget of that
agent from inside the cluster.

## Scale to zero

With scale to zero, the router stops an agent that has no work, and starts it again when there is new work. Scale to
zero is on by default (#181). On GKE, the agent node pool goes to zero nodes when no agent runs.

The router starts a stopped agent when:

- A webhook for the agent arrives.
- The wake poll finds new work for the agent. Every `wake_poll_seconds` (default 300), the router reads the open
  issues and PRs of each stopped agent on the forge. This works without webhooks.

NOTE: The first response of a stopped agent is slower. The pod must start (and on GKE, a node must start). Expect
1 to 2 minutes after the webhook, or up to `wake_poll_seconds` more on the wake poll.

NOTE: The worker asks the proxy once at start whether it takes the token. When the proxy answers 401 (mode `key`, or
a proxy from before 1.1) or does not answer, the agent uses its key. So agents keep working during an upgrade in
any order and after a switch back to `key`.

NOTE: The NetworkPolicy of the proxy allows only the addresses of the Service `default/kubernetes` at deploy time.
On GKE these addresses can change (control plane IP rotation, a change of the private endpoint). Then the token
check fails with 401 until the next playbook run. In mode `both` the agents use their key meanwhile. In mode
`token` all agents stop. After such a change, run the playbook again. `scripts/e2e-gke-security.sh` checks that the
proxy reaches the API server.

### Settings

| Setting | `kind` (`kind.vars.yml`) | GKE (`instance.yml`) | Default |
|---|---|---|---|
| Scale to zero, all agents | `SCALE_TO_ZERO` | `scale_to_zero` | `true` |
| Scale to zero, one agent | (not available) | `agents[].scale_to_zero` | the value for all agents |
| Idle time before the stop | `IDLE_SECONDS` | `idle_seconds` | `900` |
| Interval of the wake poll; `0` = off | `WAKE_POLL_SECONDS` | `wake_poll_seconds` | `300` |

The wake poll uses a read-only forge credential in the Secret `webhook-router-poll`. Only the router pod reads it.

- GitHub: the router uses the GitHub App. It asks for installation tokens with read access only.
- Gitea on `kind`: the playbook tag `agents` (role `kind_agents`) makes the account `agentdev-router` (read access to the repository) and its token.
- Gitea on GKE: put a read-only token (scopes `read:repository`, `read:issue`) in the Secret Manager entry
  `router-poll-forge-token`.

CAUTION: If you set `wake_poll_seconds: 0`, agents start again only on a webhook. The playbook stops with the error
"scale_to_zero needs webhooks or wake_poll_seconds" when an agent scales to zero and there are no webhooks
(`ingress.webhook_host` or `ingress.host` is not set).

### Keep an agent running

1. On `kind`: set `SCALE_TO_ZERO=false` (all agents).
   On GKE: set `scale_to_zero: false` for the agent in `agents`, or for all agents.
   In the web UI: clear **Scale to zero** for the agent.
2. Deploy again.

### Examine the state of scale to zero

1. Show the replicas of the agents:
   `kubectl -n coding-agents get sts -o custom-columns=NAME:.metadata.name,REPLICAS:.spec.replicas`
2. Show the scale actions of the router:
   `kubectl -n coding-agents logs deploy/webhook-router | grep scaled`

NOTE: The router must have a namespaced Role to scale the agent StatefulSets. The manifest has this Role.

NOTE: When you apply the manifests again (a new deployment), stopped agents start one time. They stop again after the
idle time.

### Health checks for Flux and Argo CD

The kinds `Agent`, `Executor` and `ProjectSecret` have a `Ready` condition and `status.observedGeneration` (#386).
Flux and Argo CD read them. A stopped agent is ready: the router can start it again.

| Kind | `Ready` is `True` | `Ready` is `False` (reason) |
|---|---|---|
| `Agent` | phase `running`, `starting`, `stopped` or `paused` | phase `blocked` (`Blocked`), phase `invalid` (`SecretNotBound`) |
| `Executor` | phase `ready` or `paused` | phase `invalid` or `error` |
| `ProjectSecret` | phase `ready` (`Synced`) | phase `pending` (`Pending`) or `error` (`Error`) |

`status.image` of an `Agent` shows the worker image of its StatefulSet. A stopped agent starts with this image at its
next wake.

To make Flux wait for the agents, use a `Kustomization` with `wait: true` or with `healthChecks`:

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: agents
  namespace: flux-system
spec:
  interval: 10m
  path: ./agents
  prune: true
  sourceRef:
    kind: GitRepository
    name: deployment
  dependsOn:
    - name: agentdev        # the layer with the CRDs and the operator
  timeout: 5m
  healthChecks:
    - apiVersion: agentdev.dev/v1alpha1
      kind: Agent
      name: alice
      namespace: coding-agents
```

For Argo CD, add a health check for each kind to `argocd-cm`. Do not use `Suspended` for a stopped agent: one
suspended resource makes all of the Application suspended.

```yaml
resource.customizations.health.agentdev.dev_Agent: |
  hs = {status = "Progressing", message = "waiting for the operator"}
  if obj.status ~= nil and obj.status.conditions ~= nil then
    if (obj.status.observedGeneration or 0) < obj.metadata.generation then return hs end
    for _, c in ipairs(obj.status.conditions) do
      if c.type == "Ready" then
        hs.message = c.message
        if c.status == "True" then hs.status = "Healthy" else hs.status = "Degraded" end
      end
    end
  end
  return hs
```

Use the same script for `agentdev.dev_Executor` and `agentdev.dev_ProjectSecret`.

To show the state of all agents, do this command:
`kubectl -n coding-agents get agents`. The column `READY` shows the `Ready` condition.

## Labels

agentdev labels its cloud resources and Kubernetes objects with `service` and `environment`, so the billing export
and GKE cost allocation split costs per instance, and the Infracost tagging policy passes. GCP labels are lower case.

### Set the labels of an instance

1. In `terraform/instance.tfvars` of the deployment repository, set `labels = { environment = "prod" }`.
   Use `dev`, `stage` or `prod`. The module adds `service = "agentdev"`.
2. In `ansible/instance.yml`, set `agentdev_labels: {environment: prod}`. Use the same values as in Terraform.
3. In `bootstrap.tfvars`, set the same `labels`.
4. Run `terraform plan`.
   Make sure that the plan shows only in-place updates.
5. Run `terraform apply`.
6. Run the Ansible playbook.

NOTE: To change the `service` label, set `service` in `labels`.

| Where | Labels |
|---|---|
| Terraform module | every resource with a `labels` field (provider `default_labels`), the cluster (`resource_labels`, also on the node VMs and disks), both node pools, the budget notification channels (`user_labels`) |
| Bootstrap | the project and the state bucket |
| Kubernetes | the namespaces `coding-agents`, `agentdev-platform` and `agentdev-executors`; agent StatefulSets and pods; executor Jobs and pods (operator `COST_LABELS`) |

NOTE: The `labels` field of a notification channel is its configuration (`email_address`), not resource labels. A tag
scanner that reads it reports a false positive.

## Spot system pool (GKE)

The system pool runs the router, the operator, the UI and the proxies. On GKE it can run on Spot VMs to cut its cost
by about 60-70%. Spot is off by default.

### Run the system pool on Spot VMs

1. In the deployment repository, set `system_pool_spot = true` (Terraform) and pass it to the module.
2. Run `terraform plan` and `terraform apply`. The system pool is replaced: the platform is down until the new nodes
   are ready.
3. Run the Ansible playbook again. It waits until the operator and the agents are ready.

### What happens when Google reclaims a node

1. Google gives the node about 30 seconds of notice, then stops it.
2. The router, the operator, the UI and the proxies are down until the pool has a new node, usually about a minute.
   If the zone has no Spot capacity, the gap is longer.
3. Webhooks sent in the gap are lost. Each agent finds its issues at the next poll (`poll_seconds`).
4. Agents keep their state on their PVCs. A job that needs the Claude proxy or the egress proxy during the gap fails
   and is retried at the next poll.

NOTE: The platform components have one replica each, so no PodDisruptionBudget is set: a budget with one replica
blocks node upgrades, and a Spot preemption does not respect it. The router has a readiness probe, so the Service
sends traffic only to a ready pod.

### Make the system pool smaller

GKE add-ons use CPU and memory on the system nodes. By default agentdev switches off the add-ons that it does not use
(#208): Managed Prometheus, the Filestore CSI driver and the extra monitoring components. The GKE load balancer and the
Gateway API are on only when `public_hosts` is set.

WARNING: Do not switch off Cloud Logging. The security events are in Cloud Logging (`security-model.md`).

1. Run `terraform plan` after you upgrade. The plan must show changes to the cluster only. No node pool is replaced.
2. Run `terraform apply`. GKE removes the add-on pods from the nodes.
3. Run `kubectl top nodes` and `kubectl describe nodes`. Compare the CPU requests with the node size.
4. If the system pool still needs two nodes, set `system_machine_type` to a larger type.

To keep an add-on, set the Terraform input in the deployment repository:

- `enable_managed_prometheus = true`
- `enable_filestore_csi = true`
- `monitoring_components = null` (the GKE default list)
- `gateway_enabled = true` (the load balancer without `public_hosts`)

NOTE: `gateway_enabled = false` with a `public_hosts` value is refused by Terraform. The managed certificate and the
Gateway need the GKE load balancer.

## Agent dashboard (botboard)

Botboard ([source](https://github.com/valentinalexeev/botboard), `ghcr.io/valentinalexeev/botboard`) shows the work
of the agents. It shows one card for each issue.

### Deploy botboard

1. Run the playbook with the tag `botboard`: `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags botboard`
   The playbook deploys botboard in the namespace `botboard`. A plain run does not deploy botboard.
2. Run the playbook with the tag `agents` again: `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags agents`
   The playbook finds botboard and sets `BOTBOARD_URL` on each agent.

### Open botboard

On GKE, open `https://<auth_host>/` and use the password from
[Keep a break-glass admin](#keep-a-break-glass-admin). On kind:

1. Start the port-forward: `kubectl -n botboard port-forward svc/botboard 8090:80`
2. Open http://localhost:8090 in a browser.

NOTE: Botboard keeps its state in memory. When botboard starts again, its state is empty. The agents send their cards again while they work.

### Connect the agents to botboard on GKE

WARNING: Do not make botboard available on a public URL. Botboard has no authentication. Any pod in the cluster can
send cards.

1. Run botboard with `TS_AUTHKEY` in your tailnet.
2. In `ansible/instance.yml`, set `botboard_url` to `http://botboard.<tailnet>`.
3. Set `forge_public_url` to the public URL of the forge.

NOTE: The playbook does not deploy botboard yet. The card links use `FORGE_PUBLIC_URL` (`forge_public_url` in Ansible;
on `kind`, the URL of the Gitea host). Thus, the links open in a browser and do not go to the forge address in the
cluster.

## MCP tools

MCP tools give the agents of a project more tools through ContextForge. On `kind`, the playbook tag `mcp`
deploys ContextForge. On GKE, the Ansible playbook deploys it.

### Deploy the platform namespace

1. Optional: in `kind.vars.yml`, set `CONTEXTFORGE_IMAGE`, `POSTGRES_IMAGE` and `CF_ADMIN_EMAIL`.
2. To use MCP tools, in `kind.vars.yml`, set `MCP_PROJECTS` to the list of paths of the project specifications.
3. Run the playbook with the tag `mcp`: `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags mcp`
   The playbook deploys the namespace `agentdev-platform` with Postgres.
   If `MCP_PROJECTS` is set, the playbook also deploys ContextForge and makes one `McpProject` for each specification.
   The operator (tag `operator`) syncs each `McpProject` to ContextForge.

NOTE: If `MCP_PROJECTS` is empty, the playbook does not deploy ContextForge. If ContextForge is already deployed,
the playbook removes it and the MCP token secrets of the agents. The ContextForge database stays in Postgres.

NOTE: Postgres has the databases `contextforge` and `agentdev`. The generated passwords and keys are in
`.secrets/platform/`, never in git.

### Deploy the platform namespace on GKE

The playbook deploys Postgres in `agentdev-platform` on each run. It deploys ContextForge only if `mcp_projects` is
not empty. ContextForge requests 100m CPU and 512 MiB memory. The playbook makes the passwords and keys on the first
run and keeps them in Secret Manager as `platform-<key>`, for example `platform-cf-admin`.

If `mcp_projects` is empty, the playbook removes ContextForge and the MCP token secrets of the agents. The ContextForge
database stays in Postgres. To keep ContextForge without `mcp_projects`, set `platform_contextforge_enabled: true`.

1. In the deployment repository, put the project specifications in a directory, for example `projects/`.
2. In `ansible/instance.yml`, list the specifications:

   ```yaml
   mcp_projects:
     - "{{ lookup('env', 'PWD') }}/projects/web.yaml"
   ```

   Use full paths on the machine that runs the playbook. `PWD` is the deployment repository.
   List the hosts of the MCP servers that you trust in `mcp_allowed_hosts` (refer to the next section).
3. Run the playbook (refer to `infra/ansible/README.md`).
   The playbook makes one `McpProject` for each specification. The operator syncs it to ContextForge.
   The machine that runs the playbook does not read the ContextForge admin password.
4. Make sure that the pods are ready: `kubectl -n agentdev-platform get pods`

CAUTION: Do not delete a `platform-pg-*` secret. Postgres keeps the first password on its volume. A new value stops
the connection from ContextForge.

NOTE: In the dry run (`--check`) on a new cluster, the `McpProject` CRD does not exist yet. The playbook skips the
`McpProject` objects in that case.

### Add MCP servers to a project

1. In the project specification, list the servers:

   ```yaml
   mcp:
     servers:
       - {name: docs, url: http://docs-mcp.tools.svc.cluster.local:8080/mcp}
   ```

2. Add the host of each server to the allow-list. Use `host` or `host:port`.
   - On kind: `MCP_ALLOWED_HOSTS` in `kind.vars.yml` (a list of hosts).
   - On GKE: `mcp_allowed_hosts` in `ansible/instance.yml`.

   CAUTION: Add only servers that you trust. The names, descriptions and outputs of their tools go to the model.
   A bad server can put instructions in them (prompt injection). Refer to R51 in `docs/security.md`.

   The operator makes no changes in ContextForge if a server is on a host that is not in the list. The
   `McpProject` shows the cause: `kubectl -n coding-agents get mcpp` (`Ready` is `False`, reason `HostNotAllowed`).
3. Run the playbook (on kind, with the tag `mcp`). The operator syncs the `McpProject`.
   You can also write the `McpProject` yourself, for example from Git with Flux (refer to "Reference: the MCP sync").
   NOTE: Since 1.2.0, the operator does the sync (#383). `scripts/mcp-sync.sh <spec>` is deprecated. It stays only
   because `scripts/bootstrap-project.sh` still runs it. It is removed when the bootstrap writes an `McpProject` (#547).
4. Start each agent again to get the new tools: `kubectl -n coding-agents delete pod agent-<name>-0`

NOTE: If a specification has `mcp:` and you did not run the playbook tag `mcp`, the bootstrap stops before it deploys the
agents. The bootstrap tells you the cause.

### Give agents the observability tools

The observability tools let agents read metrics and logs. They cannot change data. The pod `obs-mcp` in
`agentdev-platform` gives these tools:

- `prometheus_query`: reads metrics from Prometheus with a PromQL query.
- `loki_query`: reads logs from Loki with a LogQL query.
- `cloud_logging_query` (add-on, GKE only): reads Cloud Logging entries of one GCP project.

The pod keeps the credential of each backend. Agents use the tools through ContextForge with their own token.
The agent pod never gets a backend credential.

Before you start, make sure that Prometheus or Loki operates in the cluster. agentdev does not install them.

1. Set the backend addresses:
   - On kind: in `kind.vars.yml`, set `OBS_MCP` to `true`, and set `PROMETHEUS_URL` and `LOKI_URL`.
   - On GKE: in `ansible/instance.yml`, set `obs_mcp.enabled: true`, `obs_mcp.prometheus_url` and `obs_mcp.loki_url`.

   Use addresses in the cluster Service range, for example `http://prometheus-server.monitoring.svc.cluster.local`.
   An address with a user name or a password is refused.
2. Optional: give a read-only bearer token for Prometheus and Loki.
   - On kind: write the token to `.secrets/platform/obs-token`.
   - On GKE: put the token in Secret Manager. Set `obs_mcp.token_secret` to the name of the secret.

   WARNING: Use a token that can only read. Do not use an admin token.
3. Deploy the tools:
   - On kind: `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags mcp`
   - On GKE: run the playbook.
4. In the project specification, add the server:

   ```yaml
   mcp:
     servers:
       - {name: obs, url: http://obs-mcp.agentdev-platform.svc.cluster.local:8080/mcp}
   ```

5. Add `obs-mcp.agentdev-platform.svc.cluster.local` to the allow-list (`MCP_ALLOWED_HOSTS` or `mcp_allowed_hosts`).
6. Run the playbook (on kind, with the tag `mcp`). The operator syncs the project.
7. Start each agent of the project again: `kubectl -n coding-agents delete pod agent-<name>-0`

CAUTION: The tools send log lines and metric labels to the model. The pod removes common credential shapes from the
output. This filter does not find all secrets. Do not write secrets to logs. Refer to R99 in `docs/security.md`.

NOTE: The tools of a project are the same for all its agents. Every agent of the project can read all the logs and
metrics that the backend token can read. To limit the data, give a token that can read less, or set `LOKI_TENANT`.

To use the Cloud Logging tool on GKE, do these steps:

1. Make a Google service account with the role `roles/logging.viewer` only.
2. Let the Kubernetes service account `agentdev-platform/obs-mcp` use it (Workload Identity, `roles/iam.workloadIdentityUser`).
3. In `ansible/instance.yml`, set `obs_mcp.cloud_logging_project` and `obs_mcp.cloud_logging_gsa`.
4. Run the playbook.

#### Reference: the observability tools

| Setting (kind / GKE `obs_mcp.`) | Default | Meaning |
|---|---|---|
| `OBS_MCP` / `enabled` | `false` | Deploy the pod `obs-mcp` |
| `PROMETHEUS_URL` / `prometheus_url` | empty | Prometheus address; empty: no `prometheus_query` tool |
| `LOKI_URL` / `loki_url` | empty | Loki address; empty: no `loki_query` tool |
| `LOKI_TENANT` / `loki_tenant` | empty | Loki tenant (`X-Scope-OrgID`) |
| - / `cloud_logging_project` | empty | GCP project for `cloud_logging_query`; empty: no tool |
| `OBS_MCP_MAX_OUTPUT` / `max_output` | `16384` | Maximum bytes of output for one call |
| `OBS_MCP_RATE_PER_MIN` / `rate_per_min` | `30` | Calls for each minute, for each tool (all agents together) |
| `OBS_MCP_RATE_BURST` / `rate_burst` | `10` | Calls at the same time before the limit starts |

Limits for each call: the query has a maximum of 2000 characters. The time range is a maximum of 7 days. A log query
gives a maximum of 500 lines. A range query of Prometheus gives a maximum of 1100 points for each series.

The tool output starts with `UNTRUSTED DATA`. Each line has a prefix, and backticks are replaced. Thus the output
cannot close its code block. The output goes only to the model. agentdev does not use it for a decision.

### Open the ContextForge admin UI

1. Start the port-forward: `kubectl -n agentdev-platform port-forward svc/contextforge 4444:4444`

### Reference: the MCP sync

The operator syncs each `McpProject` (short name `mcpp`) in the agents namespace (#383). The name is the project:

```yaml
apiVersion: agentdev.dev/v1alpha1
kind: McpProject
metadata: {name: web, namespace: coding-agents}
spec:
  servers:
    - {name: docs, url: http://docs-mcp.tools.svc.cluster.local:8080/mcp}
  agents: [alice, bob]
```

The sync runs at each change and every 6 hours (`MCP_RESYNC_MINUTES` on the operator). It does these actions:

- It stops if a server host is not in the allow-list (`MCP_ALLOWED_HOSTS` on the operator, from `mcp_allowed_hosts`).
  The allow-list is an instance setting. An `McpProject` cannot change it.
- It registers each server as the gateway `<project>-<name>`.
- It builds the virtual server `project-<project>` from the tools of the gateways.
- It makes one token for each agent, for that server only.
- It keeps the token in the Secret `agent-<name>-mcp` (`CF_URL`, `CF_TOKEN`).

Tokens are valid for one year. A sync in the last 30 days of a token makes a new token and revokes the old token.
When you remove an agent from `agents:` (or remove the servers), the next sync deletes its Secret and revokes its
token. When you delete the `McpProject`, the operator revokes all its tokens and deletes their Secrets.

The operator cannot read the Secrets in the agents namespace (#452). It keeps the token id, the server and the renew
time in the `McpProject` status. At each sync it tells if `agent-<name>-mcp` exists with a dry-run create (no read).
- If the Secret is missing (deleted by hand), the sync makes a new token, writes the Secret and revokes the old token.
- The operator does not see a changed Secret (edit drift). To repair a changed or doubtful Secret, delete it:
  `kubectl -n coding-agents delete secret agent-<name>-mcp`. The next sync (at most `MCP_RESYNC_MINUTES`, or at once
  after any change to the `McpProject`) makes it again with a new token.

The operator reads the ContextForge admin password from the Secret `contextforge` in `agentdev-platform` (key
`PLATFORM_ADMIN_PASSWORD`). Its Role there can read only that Secret. The status shows the result, never a token:

| `Ready` reason | Meaning |
|---|---|
| `Synced` | The gateways, the virtual server and every agent token are in place |
| `NoServers` | The spec has no servers; no tokens |
| `HostNotAllowed`, `InvalidServer` | A server is refused; nothing changed in ContextForge |
| `ContextForgeUnavailable` | ContextForge is not deployed or the admin login failed |
| `NoTools` | ContextForge found no tools on the servers; make sure that they operate |
| `SyncFailed` | A ContextForge call or a Secret failed; the message names the step |

The worker gives Claude `--strict-mcp-config`, so only the tools of the gateway are available. NetworkPolicies close
Postgres to all pods except ContextForge. They close ContextForge to all namespaces except the agents namespace and
the operator pod. GKE
applies the NetworkPolicies, because the Terraform cluster uses Dataplane V2 (`ADVANCED_DATAPATH`). Local `kind`
(kindnet) does not.

To make sure that GKE applies the NetworkPolicies, do these steps:

NOTE: The test pod gets the image `curlimages/curl` from Docker Hub. The cluster must have access to the internet.

1. Start a test pod in the namespace `default`:
   `kubectl run np-test --rm -it --restart=Never --image=curlimages/curl -- curl -m 5 http://contextforge.agentdev-platform:4444/health`
2. Make sure that the command stops with a timeout.
   If you get a response, the cluster does not apply NetworkPolicies.
3. Make sure that agent pods cannot connect to Postgres:
   `kubectl -n coding-agents exec agent-<name>-0 -c agent -- bash -c 'timeout 3 bash -c "</dev/tcp/platform-db.agentdev-platform/5432" && echo open || echo closed'`
4. Make sure that the command shows `closed`.

## Identity (authentik)

authentik is the identity provider of the instance. It is in the namespace `agentdev-platform` and uses the
database `authentik` on the platform Postgres, with no Redis. On GKE, the playbook deploys it (#114). Refer to
[Deploy authentik on GKE](#deploy-authentik-on-gke).

### Deploy authentik

Do this task after the playbook tag `mcp`.

1. Optional: in `kind.vars.yml`, set `AUTHENTIK_VERSION` and `AUTHENTIK_ADMIN_EMAIL`.
2. Run the playbook with the tag `authentik`:
   `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags authentik`
   The playbook installs [authentik](https://goauthentik.io) with its Helm chart (`manifests/authentik-values.yaml`).

### Deploy authentik on GKE

The playbook deploys authentik with the same Helm chart, values and blueprints as the `kind` tag `authentik` (#114). The role
`authentik` runs after the platform Postgres and before the API.

1. In Terraform `instance.tfvars`, add the login host to `public_hosts`, for example `auth.example.com`.
   Apply, and make the DNS `A` record to `webhook_ip`.
2. In `ansible/instance.yml`, set `ingress.auth_host` to the same name. Optional: `authentik.version` and
   `authentik.admin_email`.
3. Run the playbook.
   The playbook does these steps:
   - It makes the secrets `platform-authentik-secret-key`, `platform-authentik-admin` and `platform-pg-authentik` in
     Secret Manager on the first run. It never replaces them.
   - It makes the database `authentik`, the Secret `authentik` and the ConfigMap `authentik-blueprints` (the files
     `manifests/authentik-blueprint-*.yaml` only, never `manifests/kind/`).
   - It installs the chart, adds the route `https://<auth_host>/` to the Gateway, and adds `akadmin` to
     `agentdev-admins` (add only).
   - `api.oidc_issuer` becomes `https://<auth_host>/application/o/agentdev/`, and the API reads the keys inside the
     cluster. The UI client accepts `https://<ingress.host>/callback`.

NOTE: An `api.oidc_issuer` in `instance.yml` overrides the authentik issuer. To use another OIDC provider and no
authentik, set `authentik.enabled: false`.

### Keep a break-glass admin

`akadmin` is the break-glass admin. authentik makes it at the first start with the password from Secret Manager
`platform-authentik-admin`. A password that you change in the UI stays: authentik uses the first password only once.
The Job `authentik-breakglass-<hash>` (role `authentik`, #382, `manifests/db-init/breakglass.py`) adds `akadmin` to
`agentdev-admins` with an add-only call. It never removes a member, and it fails when authentik has no active
superuser.

WARNING: Do not delete `akadmin`, do not deactivate it, and do not remove its password login. Keep its password and
the MFA recovery codes in the password manager of the owner. If the Workspace source fails, this is the only login.

To read the first password on GKE, without showing it on the screen, copy it to the clipboard:
`gcloud secrets versions access latest --secret platform-authentik-admin --project <project_id> | pbcopy`

### Log in to authentik for the first time

WARNING: Change the password and add MFA immediately after the first login. The first password is in a local file.

1. Start the port-forward:
   `kubectl -n agentdev-platform port-forward svc/authentik-server 9443:443`
2. Open https://localhost:9443/ in a browser.
   The certificate is self-signed. The session cookies must have https.
3. Log in with the user name `akadmin`. The password is in `.secrets/platform/authentik-admin`.
4. Change the password.
5. Add MFA.

NOTE: The first password is also in the Secret `authentik`, but authentik uses it only at the first start. Thus, the
password that you set in the UI is final.

### Map people to forge users

The API finds the forge user of a person from token claims (#88, #122). It does not use the authentik username,
because a user can choose a username. A person can have one login for each forge kind:

| Claim | Used for | User attribute |
|---|---|---|
| `forge_login_github` | agents on GitHub (`forge: github`) | `forge_login_github` |
| `forge_login_gitea` | agents on Gitea (`forge: gitea`) | `forge_login_gitea` |
| `forge_login` | fallback, when the claim of the agent's forge is absent | `forge_login` |

The API takes the claim of the forge of the agent's repository. If that claim is absent, it takes `forge_login`. It
never takes the claim of another forge, and it never falls back to the username. Several logins on one forge are not
supported.

The claims come from the authentik user attributes of the same names. Users cannot change these attributes in the
default setup, but only by configuration (see the WARNING below). The blueprint adds the scope `forge`, which sends
these claims to the API.

The intended setup: authentik keeps no users and passwords of its own. It signs people in through the same company
single sign-on as the forge (for example Google Workspace). The source property mapping of that identity provider
sets these attributes. For Google Workspace, the blueprint does this for you (see "Sign in with Google Workspace").

To set the forge login from the company identity provider (kind and GKE):

1. In the authentik admin interface, go to **Customization > Property Mappings**.
2. Create a source property mapping for your source type (for example **OAuth Source Property Mapping**).
3. Use an expression that gives the forge login of the person. Example, if the forge login is the local part of
   the company email:

   ```python
   email = info.get("email", "")
   return {"attributes": {"forge_login": email.split("@")[0].lower()}}
   ```

   NOTE: Use the email local part only with one verified company domain. With two domains, `a.b@x` and `a.b@y` get
   the same forge login. A reassigned mailbox also gives its forge login to the new owner. Otherwise, map from a
   unique, stable claim of the identity provider.

4. Go to **Directory > Federation and Social login**, open your source, and add the property mapping.
5. Make sure that the forge uses the same rule for its logins.

To set the forge login for one user (for example on kind, without a source):

1. Go to **Directory > Users** and open the user.
2. Click **Edit**.
3. In **Attributes**, add `forge_login_github: <GitHub login>`, `forge_login_gitea: <Gitea login>`, or the fallback
   `forge_login: <forge login>`.
4. Click **Update**.

The person logs out and logs in again. Then the new token has the claim.

WARNING: Do not let users edit `forge_login`, `forge_login_github` or `forge_login_gitea`. Only the identity
provider or the administrators set them. A person with a wrong value can act as that forge user.

- Do not add an enrollment flow. The blueprint has none.
- No flow, stage or prompt may write `attributes.forge_login`, `attributes.forge_login_github` or
  `attributes.forge_login_gitea`. authentik writes every prompt whose field key is
  `attributes.<name>` to the user. The default user settings flow (`default-user-settings-flow`) already writes
  `attributes.settings.locale` this way. A prompt with the field key `attributes.forge_login` in the user settings
  flow, an enrollment flow or a source flow lets users choose their forge user. Check this in
  **Flows and Stages > Prompts** (search `forge_login`) after setup and after each authentik upgrade.
- Use the forge login in lower case.

NOTE: If the token has neither the claim of the agent's forge nor `forge_login`, or the value has upper case letters or characters other than `a-z`, `0-9`,
`.`, `_`, `-`, the API finds no forge user. The person can read, but cannot change agents. Members of
`agentdev-admins` can pause, add and remove agents without a forge user.

NOTE: With another OIDC provider (`authentik.enabled: false`), apply
`manifests/authentik-blueprint-agentdev.yaml` to your authentik instance, or make the scope mapping in the admin
interface: scope name `forge`, the expression from the blueprint, and add it to the `agentdev` provider.

### Sign in with Google Workspace

authentik signs people in through Google Workspace, the company single sign-on (#122). The blueprint
`manifests/authentik-blueprint-sso.yaml` makes the source, the admin group and the forge login mapping. You set only
values in `instance.yml` and one secret in Secret Manager.

The source does these three things at each sign-in:

- It creates the authentik user at the first sign-in.
- It sets the user attributes `forge_login_github`, `forge_login_gitea` and `forge_login` from Workspace custom
  attributes (#88, #122).
- It synchronizes one Workspace group with the authentik group `agentdev-admins-sso`.

The group `agentdev-admins-sso` is a child of `agentdev-admins`. Thus, its members are agentdev administrators.

The synchronization obeys these rules (#103):

- It adds a person only when the Workspace group has that person.
- It removes a person from `agentdev-admins-sso` only when the person is no longer in the Workspace group. This
  occurs at the next sign-in of that person.
- It never changes the members of `agentdev-admins`. Members that you add to `agentdev-admins` (for example
  `akadmin`) stay.
- Other Workspace groups do not go to authentik.
- Only e-mail addresses of the company domains (`allowed_domains`) can sign in. authentik refuses other addresses
  before it creates a user. If Google sends the claim `hd`, it must also be a company domain.
- authentik records each change as the event `custom_agentdev_admin_sync` (**Events > Logs**). The event has the user
  name, `add` or `remove`, and the Workspace group.

NOTE: A person who left the Workspace group keeps the administrator rights until the next sign-in or until the token
expires. To remove the rights immediately, remove the person from the Workspace group and deactivate the user in
**Directory > Users**.

#### Prepare Google Workspace

You must be a Google Workspace super administrator.

1. In the Google Admin console, go to **Directory > Users > More options > Manage custom attributes**.
2. Add a custom attribute category, for example `agentdev`.
3. Add the fields `githubLogin`, `giteaLogin` and `forgeLogin`. For each, set **Visibility** to
   **Visible to admin** and **Info type** to **Text**. Users cannot edit custom attributes. Only directory
   administrators can edit them. You need only the fields of the forges that you use.
4. For each person, open the user, and set **agentdev > githubLogin** to the GitHub login and
   **agentdev > giteaLogin** to the Gitea login of that person. Use **agentdev > forgeLogin** only when the person
   has the same login on every forge.
5. Make a Workspace group for the agentdev administrators, for example `agentdev-admins@example.com`.
6. Go to **Apps > Web and mobile apps > Add app > Add custom SAML app**.
7. Write down the **SSO URL** and download the **Certificate**.
8. Set **ACS URL** to `https://<authentik host>/source/saml/agentdev-sso/acs/`.
9. Set **Entity ID** to `https://<authentik host>/source/saml/agentdev-sso/metadata/`.
10. Set **Name ID format** to **EMAIL** and **Name ID** to **Basic Information > Primary email**.
11. In **Attribute mapping**, map **agentdev > githubLogin** to the app attribute `githubLogin`,
    **agentdev > giteaLogin** to `giteaLogin`, and **agentdev > forgeLogin** to `forgeLogin`.
12. In **Group membership**, add the administrator group. Set the app attribute to
    `http://schemas.xmlsoap.org/claims/Group`.
13. Turn on the app for the people that use agentdev.

#### Configure the source

1. Put the certificate from step 7 into Secret Manager:
   `gcloud secrets create authentik-sso-idp-cert --data-file=google.pem --project <project>`
2. In `instance.yml` of the deployment repository, add `authentik.sso` (see `infra/ansible/example.vars.yml`):
   `kind: saml`, `admin_group`, `allowed_domains` and `saml_sso_url`. If your app attributes have other names than
   in step 11, also set `forge_attribute_github` (default `githubLogin`), `forge_attribute_gitea` (default
   `giteaLogin`) and `forge_attribute` (default `forgeLogin`).
3. Open a pull request in the deployment repository. After the merge, the playbook writes the Secret `authentik-sso`
   and restarts authentik.
4. Sign in to the agentdev UI with **Google Workspace**.
5. In authentik, go to **Directory > Users**, open your user, and make sure that `forge_login_github`, `forge_login_gitea` or `forge_login` is correct.

The playbook makes sure that `admin_group`, `allowed_domains` and the `forge_attribute*` values contain only safe
characters.
Without `allowed_domains`, the playbook stops. These values go into
the property mapping expressions.

NOTE: A person has at most one login for each forge kind (GitHub, Gitea), plus the fallback (decision on #122).
Several logins on one forge are not supported.

NOTE: If a custom attribute is empty or is not a valid forge login, its user attribute is empty. If the agent's forge
then has no login and `forge_login` is also empty, the person can read, but cannot change agents.

NOTE: The playbook does not deploy authentik yet (#114). Until then, the Secret `authentik-sso` waits in the
namespace `agentdev-platform`.

#### Test the sign-in on kind

On kind, a local OIDC server is used in place of Google Workspace (`manifests/kind/sso-standin.yaml`). It accepts
any user name and any claims. The same blueprint configures it as an OIDC source.

1. Run the playbook with the tag `authentik`.
   The playbook signs in a test user two times and makes sure that the rules above are obeyed. It shows
   `SSO check passed`.
2. Optional: to sign in yourself, run
   `kubectl -n agentdev-platform port-forward svc/sso-standin 18080:8080`.
3. Open authentik, and click **Company SSO (local stand-in)**.
4. Type a user name. In **claims**, type for example
   `{"groups": ["agentdev-admins@example.com"], "githubLogin": "alice"}`.

The playbook also re-applies all agentdev blueprints and makes sure that a member added by hand stays in
`agentdev-admins`. It also makes sure that a sign-in from another domain is refused. On kind, the company domain
is `example.com` (`SSO_ALLOWED_DOMAINS`).

To skip the stand-in, set `authentik_kind_sso_standin: false` in `kind.vars.yml`. To skip only the test, set
`authentik_kind_sso_check: false`.

WARNING: Do not use the stand-in outside kind. Anyone who can open it can sign in as any person.

### Give a person admin rights for agents

The members of the authentik group `agentdev-admins` can pause, add and remove all agents. The blueprint of the tag
`authentik` makes this group. On `kind`, the tag `authentik` also adds `akadmin` to the group once. On GKE, the playbook does the same
(#114).

IMPORTANT: No automated process changes the members of `agentdev-admins`. The blueprints create the group, but never
set its members, so a re-apply of a blueprint keeps the admins you add here. On `kind`, the tag `authentik` only adds
`akadmin` and never removes a member. The tag `authentik` checks this each run: it adds a test user, re-applies the blueprints, and fails
if a member is gone. Add and remove members by hand in authentik only.

1. Log in to authentik as an administrator.
2. Go to **Directory > Groups** and open `agentdev-admins`.
3. Add the user on the **Users** tab.
4. Tell the user to log in to agentdev again. The API reads the groups from the token, and an old token does not
   have the new group.

CAUTION: Give this group to platform operators only. A member can add an agent to each repository of the forge.

### Reference: authentik

- The configuration is the Secret `authentik`. The playbook generates it from `.secrets/platform/`.
- The first admin is you: the user `akadmin`, with the email `AUTHENTIK_ADMIN_EMAIL`.
- The agentdev API and web UI (#46, #48) are OIDC applications in authentik.
- The blueprint `manifests/authentik-blueprint-agentdev.yaml` makes the OIDC application and the group
  `agentdev-admins`, without members. On `kind`, the playbook tag `authentik` adds `akadmin` to the group through the authentik
  API (add only, never remove).
- The platform namespace denies all traffic by default. The API and the web UI add a NetworkPolicy rule to connect
  to authentik.

## Management API

The agentdev API (`images/api`, Go, #46) shows the agents of the instance. It also pauses, resumes, changes, adds and removes
agents (#57). On GKE, the Ansible playbook deploys it when `instance.yml` sets `api.oidc_issuer`.

### Deploy the API

Do this task after the playbook tag `authentik`.

1. Set `API_OIDC_ISSUER` in `kind.vars.yml` to the URL that you use to log in to authentik.
   On `kind`, with the 9443 port-forward, this URL is `https://localhost:9443/application/o/agentdev/`.
2. Run the playbook with the tag `api`: `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags api`
   The playbook builds the API and deploys it in `agentdev-platform`. On `kind`, it also gives the API access to the
   forge. Refer to "Give the API access to the forge".

NOTE: authentik writes the login URL into the token. Thus, `API_OIDC_ISSUER` must be the same URL.

### Run log of jobs (#425)

The agents send the run log of each job (what Claude writes, its tool calls and their results) to the API. The console
shows it in the **Run** tab of a job. The path is: the worker reads Claude's output while it runs, the forge sidecar
(`POST /run-events`, localhost only) checks and forwards it, and the API's internal listener
(`agentdev-api-internal:8081`, `POST /internal/run-events/<agent>`) stores it. The API accepts a batch only from a
running pod of that agent (`app=coding-agent,agent=<name>`), so an agent cannot write the run log of another agent.

| Limit | Value | Where |
|---|---|---|
| One text (assistant text, tool input, tool output) | 2 KB, then `[clipped]` | worker (`RUN_EVENT_CLIP`); the sidecar clips at 4 KB |
| One job | 2 MB, then one "truncated" step | worker (`RUN_LOG_MAX`); the API refuses more than 3 MB |
| One batch | 100 steps, 256 KB | sidecar and API |
| Retention | 30 days | Postgres table `job_events`, removed daily by `job_events_prune()` |

The worker and the sidecar replace credential shapes (GitHub, GitLab, Slack, AWS and OpenAI style keys, private keys,
`password=`/`token:` values, bare `Bearer` tokens, a private key whose END line was clipped) and the values of token variables in their environment with `[redacted]`. Without a
database (`DATABASE_URL` empty) the API keeps the run log in memory: it is lost on restart.

If the API or the sidecar does not answer, the run log of that moment is lost; the job continues. The run log is never
in a container log: to read it, use the console or `GET /api/jobs/<agent>/<job>/events?repo=<owner/name>` with a
bearer token.

### Deploy the API on GKE

The playbook deploys the API after the platform namespace. It uses the same manifest, schema and grants as the
`kind` tag `api`. The playbook does not deploy authentik yet (#114). Thus, you must use an OIDC provider that you have.

WARNING: Do not put secret values in `instance.yml`. Put them only in Secret Manager.

1. In the OIDC provider, make a public client `agentdev` with the authorization code and PKCE.
2. In the OIDC provider, make the group `agentdev-admins`. Put the groups of the user in the claim `groups`.
3. Add the API settings to `ansible/instance.yml`:

   ```yaml
   api:
     oidc_issuer: https://auth.example.com/application/o/agentdev/
   ```

4. Optional: give the API access to the forge. Refer to "Give the API access to the forge".
5. Open a pull request in the deployment repository. Do the dry run (`--check`). Merge after the checks pass.
   The playbook deploys the API in `agentdev-platform`.
6. Make sure that the API is ready: `kubectl -n agentdev-platform rollout status deploy/agentdev-api`

NOTE: Without `api.oidc_issuer`, the playbook does not deploy the API.

### External OIDC provider

The API reads the issuer's discovery document when it starts and exits when it cannot (#334). With a provider that
the playbook does not deploy (for example Pocket ID behind a reverse proxy), use this order:

1. Deploy the provider first. If the deployment repository has its own manifests for it, apply them before the
   playbook, or in an earlier CI step.
2. Make sure that `<api.oidc_discovery_url>/.well-known/openid-configuration` answers from the cluster and from the
   machine that runs the playbook.
3. Run the playbook. Before the API rollout, the role `api` waits for that URL for `api.oidc_wait_seconds`
   (default `300`). If the URL does not answer in that time, the playbook stops with a message that names it.
   If the API rollout still fails (a pod in crash-loop back-off from an earlier run), the role restarts the API once
   and waits again.

A discovery URL in the cluster (`*.svc` or `*.svc.cluster.local`, authentik on GKE) is not waited for: the role
`authentik` waits for authentik.

### Reference: the API settings on GKE

| `instance.yml` | Default | `kind` (`kind.vars.yml`) |
|---|---|---|
| `api.oidc_issuer` | none (the API is not deployed) | `API_OIDC_ISSUER` |
| `api.oidc_discovery_url` | `api.oidc_issuer` | the authentik Service in the cluster |
| `api.oidc_audience` | `agentdev` | `agentdev` |
| `api.oidc_wait_seconds` | `300` | none |
| `api.forge_url` | `forge_url` | the Gitea Service in the cluster |
| `api.db_retention_days` | `365` | `SECURITY_DB_RETENTION_DAYS` |
| `api.approval_deny_accounts` | `[]` | `APPROVAL_DENY_ACCOUNTS` (comma-separated) |
| `approvals.enabled` | `true` | `APPROVALS` (`false`) |
| `usage.five_hour`, `usage.weekly` | `80`, `90` | `USAGE_FIVE_HOUR`, `USAGE_WEEKLY` |
| `notifications.channels` | `[]` (off) | `NOTIFY_CHANNELS` (empty; `webpush` turns it on) |
| `notifications.subject` | none (required with `webpush`) | `NOTIFY_SUBJECT` (`mailto:admin@agentdev.local`) |
| `notifications.push_hosts` | FCM, Mozilla, Apple, Windows push services | the same list |

- The API and the operator get the same `approvals.enabled` and `usage` values.

- The playbook makes the database password one time in Secret Manager `platform-pg-agentdev-api`.
- The database login `agentdev_api` can only add and read rows. The role `agentdev_owner` owns the tables.
- The playbook writes the Secrets `agentdev-api-db`, `agentdev-api-forge` and `agentdev-api-siem`. When a Secret
  changes, the playbook restarts the API.
- The optional secrets `agentdev-api-forge-token` and `security-siem` can be empty. When a secret has no enabled
  version, the playbook acts as if the secret is not there.

### Turn on push notifications

The API can send Web Push notifications to the people who subscribe in the web UI (#347). Each person chooses the
types and mutes projects in **Settings**. The instance value turns the channel on or off.

1. In `instance.yml`, add:
   ```yaml
   notifications:
     channels: [webpush]
     subject: mailto:ops@example.com
   ```
   On `kind`, set `NOTIFY_CHANNELS` to `webpush` and set `NOTIFY_SUBJECT` in `kind.vars.yml`.
2. Run the playbook (`deploy.yml`), or `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags api` on `kind`.
3. Check the result: `kubectl -n agentdev-platform get notificationchannels`.
   The list shows `webpush` with the type `webpush`.
4. Check the API log: `kubectl -n agentdev-platform logs deploy/agentdev-api | grep "web push"`.
   The log shows "web push: on (NotificationChannel webpush)" in less than one minute.

To turn it off, remove `webpush` from `notifications.channels` and run the playbook again. The playbook removes the
`NotificationChannel`. The key pair stays, so the subscriptions work again when you turn it on.

CAUTION: Do not delete or replace the VAPID key pair. All browser subscriptions then stop, and each person must
subscribe again.

### Reference: push notifications

- `NotificationChannel` (`agentdev.dev/v1alpha1`, short name `nch`) is a resource in the platform namespace. Only
  cluster admins write it. Fields: `type` (`webpush`), `subject` (`mailto:` or `https://`), `vapid.secretRef.name`
  and `allowedHosts` (host names or `*.suffix`, max. 20).
- The playbook makes the VAPID key pair one time: Secret Manager `platform-notify-vapid` on GKE, the file
  `.secrets/platform/notify-vapid` on `kind`. It copies the pair to the Secret `agentdev-api-vapid`. The API can read
  only this Secret, by name.
- The API reads the `NotificationChannel` each minute. Without one, the push endpoints give 404.
- A subscription is accepted only for an `https` URL on port 443 on a host of `allowedHosts`. The API refuses a
  private, loopback or link-local address after DNS.
- The API checks for new items each 15 seconds: pending approvals, agents paused by a guardrail or blocked, and
  usage pauses. It sends to each person who can see the agent (`agentdev-admins`, or write access on the forge,
  checked again for each notification) and who did not mute the project or the type.
- Max. 30 notifications for each person in 10 minutes. Max. 10 devices for each person.
- When a push service says 404 or 410, the API removes that subscription.
- The data is in the API database: `push_subscriptions`, `notify_settings`, `notify_users` and `push_action_nonces`.
- Bark: #402. Network-level egress policy for the API: #403.

| Endpoint | Gives |
|---|---|
| `GET /api/push/vapid-public-key` | `publicKey` of the VAPID key pair. 404 when push is off |
| `POST /api/push/subscriptions` | adds the `PushSubscription` of this browser (`endpoint`, `keys.p256dh`, `keys.auth`). 201, 400 for a bad endpoint or key, 409 over 10 devices |
| `DELETE /api/push/subscriptions?endpoint=` | removes one subscription of the caller |
| `GET /api/notify/settings` | `channels`, `types`, `mutes`, `projects` (the caller may mute), `subscriptions`, `snoozeMinutes`, `vapidPublicKey` |
| `PUT /api/notify/settings` | `{types: {approvals, stopped, usage}, mutes: [{project, type}]}`; `type` `*` mutes the whole project. Replaces the caller's settings. Max. 30 changes a minute (429) |
| `POST /api/push/action` | `{token}` from a notification button: `deny` or `snooze`. No bearer token: the action token is the credential. 401 for a bad or expired token, 409 when it was used |

### Connect to the API

1. Start the port-forward: `kubectl -n agentdev-platform port-forward svc/agentdev-api 8088:8080`
2. Send each `/api` request with an authentik access token of the OIDC application `agentdev`.
   Without a token, the API gives 401.

### Reference: the API

- A blueprint of the playbook tag `authentik` makes the application `agentdev`. It is a public client with the authorization code
  and PKCE.
- The API gets the keys from authentik in the cluster.
- The API reads the agents and writes Agent objects with a Role on the agents namespace.

| Endpoint | Gives |
|---|---|
| `GET /api/me` | the caller (`sub`, `preferred_username`, `email`) |
| `GET /api/agents` | each agent: `name`, `repos`, `forge`, `phase` (`running`, `starting`, `stopped`, `paused`, `blocked`, `invalid`), replicas, `managed` (an Agent object exists; `false` for a StatefulSet from before the operator), `paused`, `maxConcurrent`, `scaleToZero` (#57), `securityEvents` (count, last 7 days, #70) |
| `GET /api/audit` | audit trail of the write API (#91), newest first. Only `agentdev-admins` (403 for other users). Filters: `agent`, `result` (`ok`, `denied`, `invalid`, `error`, `unauthenticated`), `since`, `limit` (1..500, default 100), `cursor` as for security events. The action `permission` (#505) records a change of `spec.permission` of an Agent, with `before.permission` and `after.permission` |
| `GET /api/agents/{name}` | one agent for the agent drawer (#344): the fields of `GET /api/agents`, plus `pods` (`name`, `phase`, `ready`, `restarts`, `started`, `reason`), `waitingOn` (items of kind `approval`, `usage-pause`, `paused`, `blocked` or `invalid`), `recentJobs` and `guide` (the harness guide that the operator writes). Only for `agentdev-admins` members: `permission` (#505), the permission mode `mode` (`dontAsk` or `auto`) and the classifier rules `autoMode` (`allow`, `softDeny`, `environment`). An `agentdev-admins` member sees every agent. Other users see only agents with a repository where they have write access. For other agents the API gives 404. |
| `GET /api/platform` | the health of the platform for the Platform page (#344). Only `agentdev-admins` (403 for other users). It gives `version`, `checked` and `components`: `api`, `router`, `operator`, `claude-proxy`, `authentik`, `forge-token`, `egress-proxy` and `agent-pool`. Each has `status` (`ok`, `degraded`, `down`, `absent`, `unknown`), `detail` and a `command` to look deeper. It gives no Secret values. |
| `GET /api/jobs` | the jobs for the Work board (#345), newest update first. A job is one issue handed to an agent and the PR of the agent for it. Fields: `id`, `agent`, `repo`, `forge`, `issue`, `pr`, `title` (untrusted text), `url`, `prUrl`, `phase` (`queued`, `build`, `review`, `done`), `outcome` (`merged` or `closed`, for `done`), `created`, `updated`, `closed`. Filters: `agent`, `repo`, `phase`, `scope`, `limit` (1..500, default 100), `cursor`. `unavailable` lists the repositories the forge did not answer for. The source is the forge, read with the API's read-only credential and kept for 90 s, plus the pending Approval objects (phase `build`). It never reads agent output, botboard or the worker state. After 90 s, the API sends the kept data at once and reads the forge again in the background (#519). Data older than 10 min is not sent: the request waits for the forge. The API reads at most 8 repositories at the same time. Same visibility as the agent drawer. |
| `GET /api/stream` | live updates for the console (#345), server-sent events. Each message is `event: <type>`, `id: <n>`, `data: {"type", "time", "data"}`. Types: `hello` (`heartbeatSeconds`), `heartbeat` (every 15 s), `agents` (the visible agent list), `approval` (one approval, on create and change), `approval-removed` (`name`), `security-event` (one event), `usage` (read `GET /api/usage` again), `bye` (`reason`; reconnect). Only items the caller may see, with the rules of their list. The stream ends after 30 minutes, max. 5 streams for each user (429). Read it with `fetch()` and the bearer token. |
| `GET /api/security-events` | security events (`agentdev.security/v1`), newest first. Filters: `agent`, `severity`, `since` (RFC 3339 or a duration such as `24h`), `limit` (1..500, default 100); `next_cursor` in the answer, pass it as `cursor` for the next page (#70). An `agentdev-admins` member sees every event. Other users see only the events of agents with a repository where they have write access on the forge (R35, #308). `repo` and `scope` as below |

**Filters on the lists** (#345). `GET /api/agents`, `GET /api/approvals`, `GET /api/security-events` and `GET /api/jobs` take `repo=owner/name` (only items of that repository) and `scope=mine` (only repositories where the caller has write access, also for admins; `scope=all` is the default). `GET /api/agents`, `GET /api/approvals` and `GET /api/jobs` also take `limit` and `cursor`; `next_cursor` in the answer gives the next page. A filter only narrows: it never shows an item the visibility rule hides.

| `PATCH /api/agents/{name}` | changes `paused`, `maxConcurrent` (1 to 8) or `scaleToZero` (JSON merge patch). Gives the agent. With `agents_source: git`, an agent from Git accepts only `paused`; other fields give 409 (#58). |
| `POST /api/agents` | adds an agent: `name`, `repo`, and optional `forge`, `claudeSecret`, `maxConcurrent`, `scaleToZero`. Gives 201. |
| `DELETE /api/agents/{name}` | removes the Agent object. The volumes stay. `?keepState=true` is the default; `false` gives 400. Gives 204. An agent from Git gives 409 (#58). |

### Give the API access to the forge

The API asks the forge for the permission of the user before each change. Do this task before you change agents
through the API.

WARNING: Do not use a token with write access. The API only reads the permissions of the users.

Gitea tells the permission of another user only to a site admin or to an admin of the repository. With any other
token, each change gives 503. Thus, the token user must have admin rights. Use a dedicated service account (for
example `agentdev-api`) that is admin of the agent repositories only, or a site admin if you accept that.

On `kind`, the playbook tag `api` (`kind.yml --tags api`) does this task for you. The playbook makes the Gitea user `agentdev-api` and makes it admin
of `REPO` and of the repository of each Gitea agent. It makes a token with the scopes `read:repository` and
`read:user` and puts it in the Secret `agentdev-api-forge`. If the Secret has a token of `agentdev-api` that
works, the playbook keeps it. The playbook does not show the token. After you add an agent on a new repository, run
the playbook with the tag `api` again.

On GKE, do these steps manually. The playbook does not do them yet (#104).

1. Sign in to the forge as the service account. Make a token with the scopes `read:repository` and `read:user`
   only.
2. Put the token in the Secret `agentdev-api-forge`.
   On GKE: put the token in Secret Manager `agentdev-api-forge-token`, then run the playbook. Do not do step 3.
   On `kind`:
   `kubectl -n agentdev-platform create secret generic agentdev-api-forge --from-literal=FORGE_API_TOKEN=<token>`
3. Restart the API: `kubectl -n agentdev-platform rollout restart deploy/agentdev-api`

NOTE: Without the Secret, each change that needs the forge gives 503.

#### GitHub repositories

For an agent on GitHub, the API asks GitHub for the permission (#87). The API uses the GitHub App of the agents. It
makes installation tokens that can only read (metadata, issues, pull requests).

- On GKE with `forge_kind: github`, the playbook puts `github_app.app_id` and the Secret Manager secret
  `github-app-private-key` in the Secret `agentdev-api-github`. You do not do other steps.
- On `kind` with `FORGE_KIND=github`, the playbook tag `api` puts `.secrets/github-app/app-id` and
  `.secrets/github-app/private-key.pem` in the Secret `agentdev-api-github`.
- If you do not use the App, you can put a token in the Secret `agentdev-api-github` with the key
  `GITHUB_API_TOKEN`. Use a fine-grained token that can only read metadata, issues and pull requests of the agent
  repositories. Then restart the API.

The API uses the same table for GitHub as for Gitea (refer to "Reference: who can change an agent"). The GitHub
roles `maintain` and `write` give write access. The roles `triage` and `read` give read access.

NOTE: If the API cannot ask GitHub, each change that needs the forge gives 503. Members of `agentdev-admins` can
still pause, add and remove agents.

### Add an agent through the API

Before you do this task, make sure that the Secret `agent-<name>` exists. If you use `claude-<name>`, make sure that
this Secret also exists. The API does not make Secrets.

1. Send the request:
   `curl -X POST -H "Authorization: Bearer $TOKEN" -d '{"name":"carol","repo":"owner/playground"}' http://localhost:8088/api/agents`
   The API gives 201 and the new agent.

NOTE: You can also add, pause, change and delete agents in the web UI. Refer to "Control agents in the web UI"
in `user-guide.md`.

### Pause an agent through the API

1. Send the request:
   `curl -X PATCH -H "Authorization: Bearer $TOKEN" -d '{"paused":true}' http://localhost:8088/api/agents/carol`
   The API gives 200 and the agent with `"paused": true`.
2. To resume the agent, send the same request with `"paused":false`.

### Limit the number of agents

An administrator can set two limits for agents that people add in the web UI or through the API (#27). The value
`0` means no limit (the default).

| Limit | Counts | Who it stops | Setting |
|---|---|---|---|
| Instance limit | all Agent objects in the agents namespace, also agents from Ansible or `kubectl` | all users, also `agentdev-admins` | Ansible `api.max_agents`; `kind`: `MAX_AGENTS` |
| Limit for each person | the agents that one person added in the web UI or through the API | all users except `agentdev-admins` | Ansible `api.max_agents_per_user`; `kind`: `MAX_AGENTS_PER_USER` |

1. Set the limits. For GKE, in `ansible/instance.yml`:
   `api: {max_agents: 2, max_agents_per_user: 1}`
   For `kind`, in `kind.vars.yml`: set `MAX_AGENTS` to 2 and `MAX_AGENTS_PER_USER` to 1.
2. Deploy again. For GKE, run the playbook. For `kind`, run the playbook with the tag `api`.
3. Make sure that the API log shows `agent limits: max 2 in total, 1 per user`.

When a limit is reached, the API refuses a new agent with 403 and a message that gives the limit. The audit line has
the result `denied`. To add the agent, remove an agent or set a larger limit. The API writes the OIDC subject of the
person into the annotation `agentdev/created-by` of each agent that it adds.

NOTE: The limits apply only to new agents through the API. They do not stop agents that already exist, and they do not
stop Ansible or `kubectl`. The ResourceQuota from `limits:` in the project spec is the Kubernetes limit for all
callers. To stop one agent and prevent a new start, use the annotation `agentdev/blocked` (refer to "Agents as
resources (operator)").

### Reference: who can change an agent

The owner decided this rule on #57. The API checks the permission on the forge at the time of each change.

| Action | Permission |
|---|---|
| Pause an agent | write access to the repository of the agent, or the authentik group `agentdev-admins` |
| Resume an agent, change `maxConcurrent` or `scaleToZero` | write access to the repository of the agent |
| Add or remove an agent | admin access to the repository, or the group `agentdev-admins` |

- The API finds the forge user from the claim of the agent's forge, `forge_login_github` or `forge_login_gitea`, or
  else from `forge_login` (scope `forge`). If the token has neither, the API refuses the change. Refer to "Map people to forge users". The function `forgeUserOf` in
  `images/api/forge.go` does this mapping.
- The API reads the groups from the `groups` claim of the token (scope `profile`).
- If the API cannot reach the forge, it refuses the change (503). Only the group `agentdev-admins` can then pause,
  add and remove agents.
- For an agent on GitHub, the API asks GitHub. Refer to "GitHub repositories" in "Give the API access to the
  forge".
- The API sends requests without a bearer token back with 401. The API does not use cookies.
- The Role of the API can read, patch, add and delete Agent objects in the agents namespace only. It cannot read
  Secrets.

### Find the changes to agents (audit)

The API writes one line for each change request, also for a refused request. The line starts with `AGENTDEV_AUDIT`.

The API also keeps each line in the table `api_audit` of the platform Postgres (#91). The API cannot change or
delete these rows. The rows stay for `SECURITY_DB_RETENTION_DAYS` (default 365 days).

1. Get a token of a user in `agentdev-admins`.
2. Run `curl -H "Authorization: Bearer <token>" https://<api host>/api/audit?limit=50`
   The answer has `entries`. Each entry has `id` and `entry`, the JSON of one audit line.
3. To see the next page, add the value of `next_cursor` as `cursor=<value>` to the URL.

Each line has `time`, `who` (`sub`, `username`, `forgeUser`, `groups`), `action`, `agent`, `result` (`ok`,
`denied`, `invalid`, `error`, `unauthenticated`), `reason`, `request`, `before`, `after` and `grant`.

NOTE: You can also read the lines in the log of the pod:
`kubectl -n agentdev-platform logs deploy/agentdev-api | grep AGENTDEV_AUDIT`. The log of the pod is lost when the
pod stops.

## Agents as resources (operator)

With the operator, an agent is one Agent object in the agents namespace. The operator makes the Service and the
StatefulSet of the agent. The operator keeps them the same as the spec, and shows the phase in
`status.phase`.

### Install the operator

A plain run of the playbook installs the operator. To install only the operator again, do this step:

1. Run the playbook with the tag `operator`:
   `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags operator`
   The playbook installs the `Agent` CRD (`agentdev.dev/v1alpha1`) and the operator (#47).

### Add an agent with an Agent object

Before you do this task, make sure that the Secret `agent-<name>` with the forge token exists.

1. Apply the Agent object:

   ```bash
   kubectl -n coding-agents apply -f - <<'EOF'
   apiVersion: agentdev.dev/v1alpha1
   kind: Agent
   metadata: {name: carol}
   spec: {repo: owner/playground, claudeSecret: claude-shared}
   EOF
   ```

2. Show the agents: `kubectl -n coding-agents get agents`
   The output shows the columns NAME, REPO and PHASE.

### Pause and resume an agent

1. To pause the agent, run:
   `kubectl -n coding-agents patch agent carol --type merge -p '{"spec":{"paused":true}}'`
   The pod stops. The operator keeps the state.
2. To resume the agent, run the same command with `"paused":false`.
   The operator starts the pod again. With scale to zero, the router stops the agent again when it has no work.

### Delete an Agent object

CAUTION: This procedure removes the pod of the agent. The volumes stay.

1. Run `kubectl -n coding-agents delete agent carol`.

### Reference: the operator

- Blocked agents (`scripts/agent-block.sh`) stay stopped.
- With `scaleToZero`, the router (not the operator) starts and stops the pod.
- Field reference: [`images/operator/README.md`](../images/operator/README.md).
- The playbook tag `agents` makes Agent objects (#57). The API reads them (`managed: true`).
- The API adds, pauses and removes agents (#57). The next work: buttons in the UI (#57 step 4, #4).
- Agents in the deployment repository with Argo CD or Flux: #58.

## Web UI

The web UI shows the agents of the instance and their status. You sign in to the web UI through authentik. The web
UI is in the `agentdev-platform` namespace. On GKE, the role `api` deploys the UI, and the public entry point
makes it available on HTTPS (refer to [Public HTTPS entry point (GKE)](#public-https-entry-point-gke)).

### Deploy the web UI

Do this task after the playbook tag `api` (the API).

1. Run the playbook with the tag `ui`: `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags ui`

The playbook builds the web UI image and deploys it to the cluster.

### Open the web UI

1. Start the port-forward to the web UI:
   `kubectl -n agentdev-platform port-forward svc/agentdev-ui 8091:8080`
2. Start the port-forward to authentik:
   `kubectl -n agentdev-platform port-forward svc/authentik-server 9443:443`
3. Open http://localhost:8091/ in a browser.
4. Click **Sign in**.
   The browser shows the authentik sign-in page.
5. Type the user name and the password.
6. Click **Log in**.
   The web UI shows a list of the agents and the phase of each agent.

NOTE: The web UI updates the list each 15 seconds.

NOTE: At this time, an agent can read its forge token (risk R4 in [security.md](security.md)). Give agents issues
only from persons that you know. Refer to [Projects](#projects).

### Open the web UI from a different computer

The web UI works only at the address http://localhost:8091. From a different computer, use an SSH tunnel to this
computer.

1. On the different computer, open an SSH tunnel for ports 8091 and 9443:
   `ssh -L 8091:localhost:8091 -L 9443:localhost:9443 <this-computer>`
2. Do steps 3 to 6 of *Open the web UI* on the different computer.

### Sign out

1. Click **Sign out**.

The web UI ends your session and the authentik session for agentdev.

NOTE: When you close the browser tab, you also sign out.

### Reference: how the web UI works

- The source is in `images/ui` (#48). An unprivileged nginx serves the web UI.
- The web UI uses OIDC with the authorization code and PKCE. The authentik client is `agentdev`.
- The browser keeps the tokens in the session storage of the tab.
- nginx sends a strict Content-Security-Policy. Scripts come only from the web UI.
- Network calls go only to the web UI and to authentik.
- nginx runs with a read-only root file system.
- nginx creates `/config.json` from the environment of the pod (`OIDC_ISSUER`).
- nginx sends `/api` requests to the API. Because of this, the browser connects only to the web UI and to authentik.
- The address of the web UI must be a strict redirect URI in the authentik blueprint
  (`manifests/authentik-blueprint-agentdev.yaml`). authentik creates its CORS list from these URIs.

## Public HTTPS entry point (GKE)

On GKE, one load balancer gives the instance a public HTTPS address (#120). It sends only two types of request to
the cluster: GitHub webhooks to the router, and the web UI with its `/api` calls. All other services stay inside
the cluster. Google makes and renews the TLS certificate.

| Public URL | Service | Notes |
|---|---|---|
| `https://<ingress.webhook_host>/hook` | `webhook-router` | GitHub App webhook. The router examines `X-Hub-Signature-256`. |
| `https://<ingress.host>/` | `agentdev-ui` | The web UI. The UI sends `/api/` to the API inside the cluster. |
| `http://<host>/...` | none | Redirect (301) to `https://`. |

The router paths `/status`, `/register` and `/idle`, the API Service, ContextForge, Postgres and the agents have no
public route.

### Make the public entry point

Do this task after the first installation on GKE.

1. Choose one DNS name, or two DNS names (one for the UI, one for the webhook).
2. In `terraform/instance.tfvars` of the deployment repository, add the names:

   ```hcl
   public_hosts = ["agentdev.example.com"]
   ```

3. Optional: to add Cloud Armor, add `cloud_armor = true`.
   Cloud Armor refuses large requests and limits the requests of each client IP address.
4. Apply Terraform (the owner does this step).
   Terraform makes the static IP address, the managed certificate and the SSL policy, and turns on the Gateway API of the cluster.
   Without `public_hosts`, Terraform does not make the static IP address, and `webhook_ip` is empty.
5. Show the values: `terraform output ingress` and `terraform output webhook_ip`.
6. In your DNS zone, make an `A` record for each name. Use the address from `webhook_ip`.
7. In `ansible/instance.yml`, add the `ingress` values from step 5:

   ```yaml
   ingress:
     host: agentdev.example.com           # the web UI
     # webhook_host: hooks.example.com    # optional, default: host
     certificate: agentdev-public-1a2b3c4d
     # security_policy: agentdev-public   # only with cloud_armor = true
   ```

8. Run the playbook (the deployment CI or `ansible-playbook`).
9. Wait until the certificate is active. This can take 60 minutes after the DNS records are correct:
   `gcloud compute ssl-certificates describe <certificate> --format='value(managed.status)'`
   The command shows `ACTIVE`.
10. Make sure that the UI opens: `curl -sI https://agentdev.example.com/`
    The response is `200` and has the header `strict-transport-security`.
11. Make sure that an internal path is not public: `curl -s -o /dev/null -w '%{http_code}\n' https://agentdev.example.com/status`
    The response is `404`.

NOTE: The web UI is routed only when `api.oidc_issuer` is set, because the role `api` deploys the UI. Set
`ingress.ui: false` to make only the webhook public.

### Use the webhook URL

1. Set `github_app.public_url` in `instance.yml` to `https://<ingress.webhook_host>`.
2. Register the GitHub App (refer to [GitHub App](#github-app-registration)).
   The script sets the webhook URL to `https://<ingress.webhook_host>/hook`.

NOTE: If you registered the App before, change the webhook URL on github.com to `https://<host>/hook`. The load
balancer does not send `/` to the router.

### Open the UI and the router on kind

On `kind`, no load balancer is installed. Use a port-forward. The paths are the same as on GKE.

1. Start the port-forward for the UI:
   `kubectl -n agentdev-platform port-forward svc/agentdev-ui 8091:8080`
2. Open `http://localhost:8091/`.
3. To send a test webhook, start the port-forward for the router:
   `kubectl -n coding-agents port-forward svc/webhook-router 8080:8080`
4. Send the request to `http://localhost:8080/hook`.

NOTE: On `kind`, no TLS, HSTS or Cloud Armor applies. Do not make a `kind` cluster public.

### Use an external ingress

Use this procedure when a reverse proxy that you operate publishes the instance, for example a tailnet ingress
(#160). The playbook then makes no Gateway, no HTTPRoute and no certificate. Terraform needs no `public_hosts` and
no static IP address. The hosts still give the authentik issuer, the callback of the web UI and the webhook URL.

WARNING: Publish only the paths in the table below. Send only the path prefix `/hook` of the webhook host to the
public port 8080 of `webhook-router`. Never publish the internal port 8081. The public port answers 404 for all
other paths (#214), but a proxy that publishes only `/hook` also keeps scanners away from the router.

| Public URL | Service (namespace, port) | Notes |
|---|---|---|
| `https://<ingress.host>/` | `agentdev-ui` (`agentdev-platform`, 8080) | The web UI and its `/api` calls. |
| `https://<ingress.webhook_host>/hook` | `webhook-router` (`coding-agents`, 8080) | Only the path prefix `/hook`. Not the internal port 8081. If your proxy has a health check, use `POST /hook` (the answer 401 shows that the router operates). |
| `https://<ingress.auth_host>/` | `authentik-server` (`agentdev-platform`, 80, pod port 9000) | The authentik login and issuer. |

1. In `ansible/instance.yml`, set the mode and the hosts:

   ```yaml
   ingress:
     mode: external                     # default: gateway
     host: agentdev.example.com         # the web UI
     webhook_host: hooks.example.com    # optional, default: host
     auth_host: auth.example.com        # authentik
     forwarder:                         # optional: who can connect to the Services
       namespace: tailscale             # the namespace of the reverse proxy pods
       pod_selector: {app: ts-ingress}  # optional, only with namespace
       # cidrs: [100.64.0.0/10]         # or the source addresses of the reverse proxy
   ```

2. Add the source range of the reverse proxy to the trusted proxy ranges of authentik. Keep the default ranges:

   ```yaml
   authentik_trusted_proxy_cidrs: [35.191.0.0/16, 130.211.0.0/22, 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10]
   ```

3. Set the reverse proxy so that each request to authentik has these headers:
   - `Host`: the public host (`ingress.auth_host`), not the Service name.
   - `X-Forwarded-Proto`: `https`.
   - `X-Forwarded-For`: the IP address of the client.
4. Set `github_app.public_url` to `https://<ingress.webhook_host>`.
5. Run the playbook. The playbook shows the URLs and the Services to publish.
6. Make sure that the issuer is correct:
   `curl -s https://auth.example.com/application/o/agentdev/.well-known/openid-configuration`
   The value of `issuer` is `https://auth.example.com/application/o/agentdev/`.
7. Do the e2e checks with `--ingress external` (`docs/e2e-security.md`).

NOTE: Without `ingress.forwarder`, the playbook applies no forwarder NetworkPolicy, and the default-deny policy of
the platform namespace stops the reverse proxy. A reverse proxy in the cluster that uses a namespace or a pod
selector does not need `cidrs`. A reverse proxy outside the cluster needs `cidrs`. The playbook refuses a range that
is wider than `/8` (for example `0.0.0.0/0`). To allow it, set `ingress.forwarder_allow_wide_cidrs: true`. The
playbook also refuses an `authentik_trusted_proxy_cidrs` range that is wider than `/8`, unless
`authentik_trusted_proxy_allow_wide: true`.

NOTE: With `ingress.forwarder.namespace` and `pod_selector`, the operator monitors the forwarder pods (#213). When
no pod is Ready for 10 minutes, the operator writes a warning to its log. It also records one SecurityEvent with
source `ingress` and rule `forwarder-not-ready`. The SIEM outputs get the event. Each outage gives one event. To
change the time, set `ingress.forwarder_not_ready_minutes`. The playbook gives the operator a Role in the forwarder
namespace that can only read pods. Without `pod_selector`, the monitor is off.

#### Deployed forwarder

With `ingress.forwarder.deploy`, the playbook also runs the funnel forwarder of headlamp (#316). Before, the
deployment repository applied its own manifest, and a forwarder that lost its route registration stayed not Ready
until somebody restarted it (headlamp#95).

1. Put the pre-auth key in the Secret Manager secret `funnel-sidecar-authkey` (other name:
   `ingress.forwarder.deploy.authkey_secret`).
2. In `ansible/instance.yml`, set the forwarder namespace, the pod selector and `deploy`:

   ```yaml
   ingress:
     mode: external
     forwarder:
       namespace: agentdev-funnel
       pod_selector: {app: funnel-forwarder}
       deploy:
         image: ghcr.io/valentinalexeev/headlamp/funnel-sidecar:0.19.0   # a release, a sha tag or a digest
         control_url: https://headscale.example.com
   ```

3. Run the playbook. It makes the namespace, the Secret `funnel-sidecar`, a Deployment with one replica, and a
   NetworkPolicy. The forwarder can reach DNS, the UI, the router's public port, authentik and public addresses.
   Nothing can connect to it.
4. Delete the old manifest from the deployment repository (for example `k8s/funnel-forwarder.yaml`). The Deployment
   name `funnel-forwarder` stays the same, so the playbook takes over the existing Deployment. Kubernetes does not
   let a run change the selector of a Deployment: when the old selector is not `ingress.forwarder.pod_selector`, the
   run fails. Then delete the old Deployment
   (`kubectl -n agentdev-funnel delete deployment funnel-forwarder`) and run the playbook again.

The readiness probe and the liveness probe ask `/readyz`. It answers 200 only while every route is registered. The
liveness probe restarts the pod after 10 minutes without registration (`liveness_period_seconds` 30 x
`liveness_failure_threshold` 20). The published Services are, by default, the UI, `/hook` of the router, and authentik
when the role deploys it (`deploy.services`, format `name=port@host:port`). The router's internal port 8081 is never
published. The playbook refuses an image tag `main` or `latest`, and a `control_url` that does not use `https`. A new
key version in Secret Manager restarts the forwarder on the next run. The NetworkPolicy blocks private, CGNAT and
link-local addresses. When headscale or `funnel-ingress` has such an address, add it in `deploy.egress_cidrs`
(for example `[100.64.0.0/10]`; no range wider than `/8`).

NOTE: `ingress.forwarder` also operates with `mode: gateway`. Then the router policy also keeps the Google load
balancer ranges.

### Post-deploy smoke test

At the end of each run, the playbook sends requests to the public hosts from the operator computer (#213). It does
this in the two ingress modes. The run fails when one of these checks does not pass in `ingress.smoke_timeout`
seconds (default 300):

| Check | Expected answer |
|---|---|
| Forwarder pods (only with `ingress.forwarder.namespace` and `pod_selector`) | Ready |
| `POST https://<ingress.webhook_host>/hook` without a signature | 401 from the router |
| `GET https://<ingress.webhook_host>/status` | 404 (the internal endpoints are not public) |
| `GET https://<ingress.host>/` (when the UI is routed) | 200, a redirect, 401 or 403 |
| `GET https://<ingress.auth_host>/` (with authentik) | 200, a redirect, 401 or 403 |

The failure message gives the URL and the answer. A new managed certificate can need more time on the first run.
Then run the playbook again, or set a larger `ingress.smoke_timeout`. To turn the test off, set `ingress.smoke: false`.
The test does not operate in check mode (`--check`).

### Reference: the public entry point

| Control | Where | Value |
|---|---|---|
| TLS | Terraform `google_compute_ssl_policy` | TLS 1.2 or higher, profile `MODERN` |
| HTTP to HTTPS | `HTTPRoute agentdev-https-redirect` | 301, no backend on port 80 |
| HSTS | response header on both routes | `max-age=31536000; includeSubDomains` (`ingress.hsts_max_age`) |
| Request size | router (5 MB for `/hook`), UI nginx (`client_max_body_size 1m`), Cloud Armor (`public_max_request_bytes`) | |
| Rate limit | Cloud Armor (optional) | `cloud_armor_requests_per_minute` for each client IP, then `429` |
| Network | NetworkPolicy `agentdev-ui-from-lb` | only the Google load balancer ranges reach the UI pods |

The objects are in `infra/ansible/roles/ingress/templates/gateway.yaml.j2`. Risks: `docs/security.md` R66 to R68.
Cost: the forwarding rule costs approximately $18 each month; Cloud Armor adds approximately $7 each month.

## Security events

A security event is a record of an action that a guardrail stopped. The forge sidecar and the agent loop write each
security event to the log of the agent pod. The operator reads these logs and makes a SecurityEvent object for each
event. The API keeps a copy of each event in the platform Postgres for one year. Field reference and SIEM mapping:
[security-model.md](security-model.md#detecting-and-reporting-bypass-attempts).

### Look at the security events

1. Open the web UI. Refer to *Open the web UI*.
2. Click **Security**.
   The web UI shows the security events of the last 7 days, with the newest event first.
3. To see fewer events, select an agent, a severity or a time in the filters.
4. To see older events, click **Load more**.

NOTE: The **Security (7 days)** column on the **Agents** page shows the number of events for each agent. Click the
number to see the events of that agent.

NOTE: You can also use `kubectl -n coding-agents get securityevents` or `GET /api/security-events` (refer to
*Management API*).

### Turn on approvals

Approvals let agents ask a project member for permission on the forge, instead of a refusal. Design and rules:
[security-model.md](security-model.md#approvals-and-permission-notifications).

CAUTION: Make sure that the API has a forge token (refer to *Give the API access to the forge*). Without the token,
the API cannot make a decision, and each request stays open until it becomes void.

IMPORTANT: Approvals need agentdev-api, which decides the requests. On GKE, approvals are on by default
(`approvals.enabled: true`). The playbook deploys the API only when `api.oidc_issuer` is set. Thus, if
`api.oidc_issuer` is empty, the playbook stops with an error. Set `api.oidc_issuer` in `instance.yml`. To turn off
approvals, set `approvals.enabled: false`. Optional: set `approvals.timeout_minutes`.

NOTE: On kind, approvals are off by default (`APPROVALS=false`), because a plain run of the playbook does not
install the API. To turn on approvals on kind, do the steps that follow.

1. Set `APPROVALS` to `true` in `kind.vars.yml`.
2. Optional: set the time that the agent waits for a decision with `APPROVAL_TIMEOUT_MINUTES`. The default is 30
   minutes. After this time, the agent continues without the action, but the request stays open.
3. Run the playbook with the tags `api,operator`:
   `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags api,operator`
   The operator starts the agent pods again with the new setting.

NOTE: The agent asks for permission through Claude Code's `can_use_tool` host protocol (`permission-host` in the
worker image, #539). The request then shows Claude Code's reason, the blocked path and the suggested rule. The
decision rules do not change. The environment variable `APPROVAL_HOST=off` on the agent container switches back to
the MCP prompt tool `mcp__agentdev__approve`, which sends only the tool and its input.

NOTE: Decisions are in the audit log of the API (`AGENTDEV_AUDIT` lines with the action `approval-...`). The
API also keeps them in Postgres. To see them, open `GET /api/audit` (admin group). A decision in the web UI has
`"via": "ui"` and the user name of the person.

NOTE: Members of `agentdev-admins` can make a decision in the web UI for all projects. Other people must have write
access to the repository. Agents cannot make a decision in the web UI. The same procedure applies to kind and GKE.

NOTE: Agents and the forge account of the API cannot make a decision. To exclude other service accounts (for
example the account of the router), set `APPROVAL_DENY_ACCOUNTS` to a list of forge logins.

NOTE: On GitHub, all agents write as the bot account of the GitHub App (`<app-slug>[bot]`). The API always puts
this account in the deny list. The API also ignores all comments of bot accounts. You do not add the App bot to
`APPROVAL_DENY_ACCOUNTS`.

NOTE: On GitHub, the API needs the GitHub credential to make a decision (refer to "GitHub repositories"). Without
it, a GitHub request stays open until it becomes void.

NOTE: Approval objects stay in the cluster for 90 days (`APPROVAL_RETENTION_DAYS`). Use
`kubectl -n coding-agents get approvals` to see them.

### Resume an agent that the operator paused

The operator pauses an agent when a high severity event occurs. The operator also pauses an agent when one job has
more than 5 events (`SECURITY_PAUSE_AFTER`). The count starts again for a new pod. The operator also pauses an
agent when its containers write more than 10 events in one minute (rule `rate-limited`).

WARNING: Do not resume the agent before you read the events and the issue or the pull request of the job. The text
of the issue can contain a prompt injection.

1. Find the reason for the pause:
   `kubectl -n coding-agents get agent <name> -o jsonpath='{.status.conditions[?(@.type=="SecurityPaused")].message}'`
2. Read the events of the agent on the **Security** page.
3. Read the issue or the pull request of the job.
4. Resume the agent:
   `kubectl -n coding-agents patch agent <name> --type merge -p '{"spec":{"paused":false}}'`
   The agent starts again. The condition `SecurityPaused` changes to `False`.

### Send the security events to a SIEM

The API can send each new event to a webhook URL. The API signs each request with HMAC-SHA256.

WARNING: Do not put the webhook secret in git. Keep the secret only in the instance secrets.

1. Make a random secret, for example: `openssl rand -hex 32`
2. Write the URL and the secret to the file `.secrets/platform/siem-webhook`:
   ```
   SECURITY_WEBHOOK_URL=https://siem.example.com/agentdev
   SECURITY_WEBHOOK_SECRET=<the secret>
   ```
3. Run the playbook with the tag `api`: `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags api`
   The playbook puts the file into the Secret `agentdev-api-siem` and starts the API again.
4. On the SIEM, do a check of each request:
   - Calculate HMAC-SHA256 with the secret over `<X-Agentdev-Timestamp>.<body>`.
   - Compare the result with the value in `X-Agentdev-Signature` (`sha256=<hex>`).
   - Refuse the request if the timestamp is more than 5 minutes old.
   - Use a constant-time function for the comparison, for example `hmac.compare_digest` in Python or `hmac.Equal`
     in Go.
   - Keep the `hash` of each event and refuse a request with a known `hash`.

NOTE: If the SIEM does not answer with a 2xx status, the API tries again 7 times. The time between the tries
increases from 2 seconds to a maximum of 5 minutes. The event stays in the Postgres table and in the cluster log.

NOTE: The API sends each event one time at most. The API does not send an event again after the last try or after a
restart. Each agent has a queue of 200 events. When the queue is full, the API does not send the new events of
that agent and writes the number in its log. Use `GET /api/security-events` to find the events that the SIEM did not
get.

NOTE: The `source` of an event comes from the container. An event with `untrusted: true` comes from the agent
container. Claude can write such an event, so do not trust its text.

NOTE: A SIEM can also read `GET /api/security-events` with the `cursor` parameter, or read the cluster logs with the
filter `AGENTDEV_SECURITY_EVENT`.

### Send the security events to a syslog server

The API can send each new event to a syslog server over TLS (RFC 5424 and RFC 5425). The API always does a check of
the certificate of the server.

WARNING: Do not put the keys or the certificates in git. Keep them only in the instance secrets.

1. Get the address of the syslog server, for example `siem.example.com:6514`.
2. Add the address to the file `.secrets/platform/siem-webhook`:
   ```
   SECURITY_SYSLOG_ADDR=siem.example.com:6514
   ```
3. If a private CA signed the certificate of the server, add the CA certificate (PEM) to the same file:
   `SECURITY_SYSLOG_CA=-----BEGIN CERTIFICATE-----\n...\n-----END CERTIFICATE-----`
   Write the PEM on one line. Write `\n` for each line break.
4. If the name in the certificate is not the name in the address, add `SECURITY_SYSLOG_SERVER_NAME` to the same
   file.
5. If the server requires a client certificate, add `SECURITY_SYSLOG_CLIENT_CERT` and `SECURITY_SYSLOG_CLIENT_KEY`
   (PEM) to the same file.
6. Run the playbook with the tag `api`: `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags api`
   The API starts again. The API log shows no `security outputs` error.

NOTE: When the syslog server is not available, the API stops all syslog messages. The API tries each message 4 times.
The time between the tries is a maximum of 30 seconds. Then the API writes `syslog: event <id> not delivered` in its
log. When the queue of an agent is full, the API writes `syslog queue of agent <name> full` in its log. The events
stay in the Postgres table.

NOTE: Each message has facility 13 (log audit). The severity is 5 (notice) for `low`, 4 (warning) for `medium`
and 2 (critical) for `high`. The structured data `agentdev@32473` has the fields. The message text is the event in
JSON.

### Send the security events to an OpenTelemetry collector

The API can send each new event as an OTLP log record over HTTPS.

WARNING: Do not put the API key of the collector in git. Keep it only in the instance secrets.

1. Add the URL of the collector to the file `.secrets/platform/siem-webhook`:
   ```
   SECURITY_OTLP_URL=https://collector.example.com:4318/v1/logs
   ```
2. If the collector requires an API key, add the headers to the same file:
   `SECURITY_OTLP_HEADERS=Authorization=Bearer%20<key>`
3. If a private CA signed the certificate of the collector, add `SECURITY_OTLP_CA` (PEM) to the same file.
   If the name in the certificate is not the name in the URL, add `SECURITY_OTLP_SERVER_NAME` to the same file.
4. Run the playbook with the tag `api`: `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags api`
   The API starts again. The API log shows no `security outputs` error.

NOTE: The API accepts only an `https://` URL for the webhook and for OTLP. You can use the webhook, syslog and OTLP
at the same time.

NOTE: On GCP (Ansible), do not use the file `.secrets/platform/siem-webhook`. Write the addresses in `instance.yml`
under `security.siem`. Write the secret values in the Secret Manager secret `security-siem`. Refer to
`infra/ansible/README.md`.

### Do a check of the hash chain

Each event has the fields `prev` and `chain`. Use them on the SIEM to find events that are missing or changed.

1. For each agent, keep the `chain` value of the last event that you received.
2. When a new event of that agent arrives, compare its `prev` with the kept value.
   If the values are not equal, an event is missing. Use `GET /api/security-events` to find it.
3. Calculate SHA-256 over `prev`, a newline and the canonical JSON of the event (see `docs/security-model.md`,
   "Hash chain").
4. Compare the result with `chain`.
   If the values are not equal, somebody changed the event.
5. Keep the new `chain` value.

NOTE: The chain cannot show that somebody deleted the newest events of an agent. Only the copy on the SIEM shows
this.

NOTE: The events from before issue #71 have an empty `prev` and `chain`. After the API deletes old events, the
first event of an agent can have a `prev` that you do not have. This is correct.

### Reference: settings

| Setting | Default | Where |
|---|---|---|
| `SECURITY_PAUSE_AFTER` | 5 | `kind.vars.yml`; Ansible `security.pause_after` |
| `SECURITY_RETENTION_DAYS` (SecurityEvent objects) | 30 | `kind.vars.yml`; Ansible `security.retention_days` |
| `SECURITY_MAX_PER_AGENT` (SecurityEvent objects) | 500 | `kind.vars.yml`; Ansible `security.max_per_agent` |
| `SECURITY_RATE_PER_MIN` (events per minute per container) | 10 | `kind.vars.yml`; Ansible `security.rate_per_min` |
| `SECURITY_RATE_BURST` (events above the rate for a short time) | 20 | `kind.vars.yml`; Ansible `security.rate_burst` |
| `SECURITY_DB_RETENTION_DAYS` (Postgres table) | 365 | `kind.vars.yml`; the playbook tag `api` writes it to the database |

SIEM outputs: the API reads these keys from the Secret `agentdev-api-siem`. All keys are optional.

| Key | Secret | kind (tag `api`) | GKE (Ansible) |
|---|---|---|---|
| `SECURITY_WEBHOOK_URL` | no | `.secrets/platform/siem-webhook` | `security.siem.webhook_url` |
| `SECURITY_WEBHOOK_SECRET` | yes | `.secrets/platform/siem-webhook` | Secret Manager `security-siem` |
| `SECURITY_SYSLOG_ADDR` | no | `.secrets/platform/siem-webhook` | `security.siem.syslog_addr` |
| `SECURITY_SYSLOG_SERVER_NAME` | no | `.secrets/platform/siem-webhook` | `security.siem.syslog_server_name` |
| `SECURITY_SYSLOG_CA` | no | `.secrets/platform/siem-webhook` | Secret Manager `security-siem` |
| `SECURITY_SYSLOG_CLIENT_CERT`, `SECURITY_SYSLOG_CLIENT_KEY` | yes | `.secrets/platform/siem-webhook` | Secret Manager `security-siem` |
| `SECURITY_OTLP_URL` | no | `.secrets/platform/siem-webhook` | `security.siem.otlp_url` |
| `SECURITY_OTLP_SERVER_NAME` | no | `.secrets/platform/siem-webhook` | `security.siem.otlp_server_name` |
| `SECURITY_OTLP_HEADERS` | yes | `.secrets/platform/siem-webhook` | Secret Manager `security-siem` |
| `SECURITY_OTLP_CA` | no | `.secrets/platform/siem-webhook` | Secret Manager `security-siem` |
| `SECURITY_OTLP_CLIENT_CERT`, `SECURITY_OTLP_CLIENT_KEY` | yes | `.secrets/platform/siem-webhook` | Secret Manager `security-siem` |

NOTE: On GKE, a key in the Secret Manager secret `security-siem` replaces the same key from `instance.yml`. The
Ansible role `api` installs the API when `instance.yml` sets `api.oidc_issuer` (#104). The API reads the Secret.

NOTE: `SECURITY_RETENTION_DAYS` is for the `SecurityEvent` objects in the cluster. `SECURITY_DB_RETENTION_DAYS` is
for the rows in Postgres. The API cannot change the database retention.

## Egress allow-list

The agents can connect to the internet only through the egress proxy (`egress-proxy`, #65). The proxy lets an agent
connect only to the hosts on the allow-list. The default list contains the Claude API, GitHub, npm, PyPI, the Go
module proxy and crates.io. A NetworkPolicy stops all other connections from the agent pods. On `kind`, this
NetworkPolicy operates only with kind v0.24 or later. `kind.yml --tags prereqs` (and `--tags cluster`) makes sure of this.

### Add a host to the allow-list

Do this task when the build of a project needs a host that is not on the list, for example a different package
registry.

WARNING: Add only a host that you trust. An agent can send data to each host on the list.

1. Add the host to the list:
   - On `kind`: set `EGRESS_EXTRA_HOSTS` in `kind.vars.yml`, for example to `repo.maven.apache.org` and `.example.com`.
   - On GKE: add the host to `egress_extra_hosts` in `instance.yml`.
2. Write a dot before the name to also allow all subdomains: `.example.com`.
3. Apply the change:
   - On `kind`: run the playbook with the tag `agents`.
   - On GKE: run the playbook (open a pull request in the deployment repository).
   The proxy starts again with the new list.
   NOTE: During the restart of the proxy, the agents cannot connect to the internet for some seconds.

### Find the connections that the proxy refused

1. Run this command:
   `kubectl -n coding-agents logs deploy/egress-proxy | grep AGENTDEV_SECURITY_EVENT`
   The output shows one line for each refused request, with the IP address of the agent pod.
2. Open the **Security** page of the web UI. The events have the source `egress` and the rule `egress-host`.

NOTE: The operator does not pause an agent because of egress events. Look at the events and add the host, or find the
cause of the requests.

NOTE: The event shows only the method, the host and the port. The event does not show the path of the request.

### Reference: egress

| Setting | Default | Where |
|---|---|---|
| Allow-list | the Claude API, GitHub, npm, PyPI, Go, crates.io | `EGRESS_HOSTS` in `kind.vars.yml`; Ansible `egress_hosts` |
| Forge hosts | `api.github.com`, `github.com`: the forge sidecar only (proxy credential in Secret `egress-forge-proxy`); refused for the agent container | `FORGE_EGRESS_HOSTS`; Ansible `forge_egress_hosts` |
| Claude API | dropped from the agents' list while the credential proxy is on: agents call `claude-proxy` directly | `CLAUDE_PROXY`; Ansible `claude_proxy` |
| More hosts | none | `EGRESS_EXTRA_HOSTS`; Ansible `egress_extra_hosts` |
| Proxy image | `ubuntu/squid` `6.6-24.04_beta` (pinned digest; Canonical has no other tag for 24.04) | `EGRESS_PROXY_IMAGE`; Ansible `egress_proxy_image` |
| Proxy address for the agents | `http://egress-proxy.coding-agents.svc.cluster.local:3128` | operator env `EGRESS_PROXY` (empty = no proxy) |
| FQDN policy for the proxy (GKE) | on | Ansible `egress_fqdn_policy`; Terraform `enable_fqdn_network_policy`. If it is on and the cluster does not have it, the playbook stops |

NOTE: The proxy refuses all private, loopback and link-local addresses, also when an allowed name points to one.

## Deterministic executors

An executor applies a change that a person approved (#69). An AI agent designs the change with read-only
credentials. The executor has the write credentials. No AI runs in the executor. The image and the commands come
from the `Executor` object, never from the repository. Only cluster admins can write `Executor` objects.

The flow for each pull request:

1. The executor plans the head commit of the pull request. It uses the read-only credentials. It sets the commit
   status `agentdev/trusted-plan` and writes the plan output in a comment.
2. After the merge, the executor plans the merge commit again. It takes the commit from the protected branch only.
   The operator calculates the SHA-256 of the plan file and writes an approval request on the pull request.
3. A person with write access replies `/approve <id>`.
4. The executor applies the plan file with the write credentials. Before the apply, the operator image checks that the
   plan file has the approved SHA-256. Each approval is for one apply only.

### Set up an executor

Before you do this task, make sure that you have:

- An own forge account for the executor, with write access to the repository. Do not use an agent account.
- An image of the tool, with a digest (`name@sha256:...`).
- The read-only credentials and the write credentials of the environment.

1. Make the Secrets in the namespace `agentdev-executors`:

   ```bash
   kubectl -n agentdev-executors create secret generic exec-tf-prod-forge --from-literal=FORGE_TOKEN=<token>
   kubectl -n agentdev-executors create secret generic exec-tf-prod-plan --from-env-file=plan.env
   kubectl -n agentdev-executors create secret generic exec-tf-prod-apply --from-env-file=apply.env
   ```

   On GKE, use Workload Identity instead of keys. Make two service accounts, `exec-tf-prod-plan` and
   `exec-tf-prod-apply`, and set the `serviceAccountName` fields below.

2. Apply the Executor object:

   ```bash
   kubectl -n agentdev-executors apply -f - <<'EOF'
   apiVersion: agentdev.dev/v1alpha1
   kind: Executor
   metadata: {name: tf-prod}
   spec:
     environment: prod
     repo: owner/platform
     path: infra
     image: hashicorp/terraform@sha256:<digest>
     plan: {command: [terraform, plan, -input=false, -out=$(PLAN_FILE)]}
     apply: {command: [terraform, apply, -input=false, $(PLAN_FILE)]}
     planCredentials: {secretName: exec-tf-prod-plan}
     applyCredentials: {secretName: exec-tf-prod-apply}
     forgeSecret: exec-tf-prod-forge
     egressHosts: [oauth2.googleapis.com, storage.googleapis.com, cloudresourcemanager.googleapis.com]
   EOF
   ```

   `egressHosts` is the list of hosts that this executor can connect to, in addition to the default list (see
   [Add a host for an executor](#add-a-host-for-an-executor)). Write the cloud APIs that the tool uses.

3. In the forge, add the branch protection rule for the branch: require the status check `agentdev/trusted-plan`
   and require that the branch is up to date.
4. Add the forge account of the executor to `APPROVAL_DENY_ACCOUNTS` of the API. The executor writes tool output in
   comments. This step makes sure that no comment of the executor can decide an approval.
5. Show the executors: `kubectl -n agentdev-executors get executors`
   The column PHASE shows `ready`.

NOTE: With Ansible, put the executors in `executors:` of the instance file
(`examples/deployment/gcp-gke/ansible/instance.yml.example`). The playbook makes the Secrets, the service accounts
and the Executor objects.

### Add a host for an executor

The executor Jobs can connect to the internet only through the executor egress proxy (`executor-egress-proxy` in
the namespace `agentdev-executors`, #135). A NetworkPolicy (`executor-egress`) stops all other connections. The
Jobs can always use DNS, the in-cluster forge (`kind`) and the GKE metadata server (Workload Identity). Each executor
can connect only to these hosts:

- The default list for all executors: `EXECUTOR_EGRESS_HOSTS` (Terraform registry and HashiCorp releases).
- The git host of the forge (`github.com` on GitHub).
- The hosts in `spec.egressHosts` of the `Executor`.

The proxy refuses all other requests. Each refused request is a security event with the source `egress`, the rule
`egress-host` and the name of the executor.

Do this task when a plan or an apply fails because a host is refused.

WARNING: Add only a host that you trust. The plan of a pull request runs code that nobody reviewed. That code can
send data to each host on the list of the executor.

1. Find the refused host. Open the **Security** page of the web UI, or run:
   `kubectl -n agentdev-executors logs deploy/executor-egress-proxy | grep AGENTDEV_SECURITY_EVENT`
   The `action` field shows the method, the host and the port.
2. Add the host:
   - On `kind`: add the host to `spec.egressHosts` of the `Executor`, for example
     `kubectl -n agentdev-executors patch executor tf-prod --type=json -p '[{"op":"add","path":"/spec/egressHosts/-","value":"sts.googleapis.com"}]'`.
     If the list does not exist, use `--type=merge -p '{"spec":{"egressHosts":["sts.googleapis.com"]}}'`.
   - On GKE: add the host to `egress_hosts` of the executor in `instance.yml`, then run the playbook (open a pull
     request in the deployment repository). The playbook also adds the host to the FQDN policy of the proxy.
   - For a host that all executors need: set `EXECUTOR_EGRESS_HOSTS` in `kind.vars.yml` and run the tag `operator` (`kind`), or
     set `executor_egress_hosts` in `instance.yml` (GKE).
3. Write a dot before the name to also allow all subdomains: `.googleapis.com`. On GKE, the FQDN policy of the
   proxy allows only one level of subdomains for a dot entry.
4. Wait one poll (`EXECUTOR_POLL_SECONDS`) and 15 seconds. The operator writes the new list to the Secret
   `executor-egress-proxy`, and the proxy loads it. The proxy does not start again. Then start the run again: push a
   new commit to the pull request.

NOTE: A change of `spec.egressHosts` does not change the spec hash. A pending approval stays valid.

NOTE: The operator does not pause an executor because of egress events.

NOTE: If an executor cannot connect to anything (for example, the proxy is not ready), the run fails and the pull
request shows the failure. The executor never gets a direct connection. To stop the egress limit for a test only,
delete the NetworkPolicy `executor-egress` (`kubectl -n agentdev-executors delete networkpolicy executor-egress`).
The next playbook run (on `kind`, the tag `operator`) makes it again.

### Find the runs of an executor

1. Run `kubectl -n agentdev-executors get executorruns`.
   The output shows the stage (`pr`, `merge` or `apply`), the pull request, the commit and the phase.
2. To read the output of a run, run `kubectl -n agentdev-executors logs job/<run name> -c tool`.

### Stop an executor

1. Run `kubectl -n agentdev-executors patch executor tf-prod --type merge -p '{"spec":{"paused":true}}'`.
   The executor starts no new runs. A run that started continues to the end.

### Reference: executors

| Setting | Default | Where |
|---|---|---|
| `EXEC_NS` (namespace of the executors) | `agentdev-executors` | `kind.vars.yml`; Ansible `exec_ns` |
| `EXECUTOR_POLL_SECONDS` (time between forge polls) | 60 | `kind.vars.yml`; Ansible `executor_poll_seconds` |
| `EXECUTOR_RETENTION_DAYS` (finished runs; minimum 8) | 30 | `kind.vars.yml`; Ansible `executor_retention_days` |
| `EXECUTOR_APPLY_START_MINUTES` (an allowed apply starts within this time after the decision, else nothing is applied) | 30 | `kind.vars.yml`; Ansible `executor_apply_start_minutes` |
| `spec.approvalHours` (a decision counts only within this time after the request) | 24 | `Executor` |
| `spec.allowForkPRs` (also plan pull requests from forks) | false | `Executor`; Ansible `allow_fork_prs` |
| `EXECUTOR_EGRESS_HOSTS` (hosts that all executors can connect to) | `registry.terraform.io releases.hashicorp.com` | `kind.vars.yml`; Ansible `executor_egress_hosts` |
| `spec.egressHosts` (more hosts for one executor, maximum 64) | none | `Executor`; Ansible `egress_hosts` of the executor |
| Executor egress proxy | `http://executor-egress-proxy.agentdev-executors.svc.cluster.local:3128` | operator env `EXECUTOR_EGRESS_PROXY` |

- Each run start and each result is a security event with the source `executor` and the rule `executor.plan`,
  `executor.apply` or `executor.refused`.
- The executor plans only pull requests that change a file below `spec.path`. It plans only merges after the
  `Executor` object was made, and not older than 24 hours.
- One executor per environment. If two `Executor` objects have the same `spec.environment`, the oldest one runs.
  The other ones show the phase `invalid` with the reason `DuplicateEnvironment`, and an `executor.refused` event is
  recorded. The Ansible playbook stops before it changes anything if two executors have the same environment.
- By default the executor plans only pull requests from branches of the same repository. A pull request from a fork
  runs the code of its author with the plan credentials. Set `spec.allowForkPRs: true` only if the plan credentials
  can read nothing secret (risk R81). The egress allow-list of the executor limits where that code can send data.
- The approval covers the image digest, the apply command and a SHA-256 of the `Executor` spec (also the
  credentials). If you change the `Executor` after the merge plan, the pending approval no longer applies: the run
  is closed with `executor spec changed`. Merge a new commit to plan again.
- The executor Job has no Kubernetes API token. It runs in gVisor on the sandbox nodes.
- The executor Jobs use their own egress proxy and allow-list, not the list of the agents. See
  [Add a host for an executor](#add-a-host-for-an-executor).
- Design: [security model, phase 5](security-model.md#phase-5-deterministic-executors-69).

## Project secrets

A project secret is a credential that the agents of one project, or one agent, can use (#202, ADR 0003). An
example is the token of a private package registry. The agent does not get the value. The credential broker adds
the value to the requests of the agent. A `ProjectSecret` object tells who can use the secret and how. The value
is in the secret store, never in the object.

| Field | Values |
|---|---|
| `scope` | `project` (all agents of `spec.project` on `spec.forge`) or `agent` (only `spec.agent`, and only while it works on `spec.project` on `spec.forge`) |
| `forge` | `gitea` (default) or `github`. A repository is the pair of forge and name: `acme/app` on Gitea is not `acme/app` on GitHub |
| `mode` | `broker` (the broker adds the value to requests for `spec.broker.upstream`) or `env` (the agent can read the value, see below) |

Names: the value of the ProjectSecret `<n>` is the store key `agentdev-ps-<n>`. External Secrets Operator copies it
into the Secret `ps-<n>` in the agents namespace. Agent pods do not mount `ps-*` Secrets.

> **Status:** part 1 of #202. This part adds the object, the store and the copy. The broker, the write-only API and
> the playbook tasks for GKE come in the next parts. Until then, only kind has the store.

### Set up the secret store (kind)

1. Install External Secrets Operator, the store and the rules:

   ```bash
   ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags secrets
   ```

2. Make sure that the store is ready:

   ```bash
   kubectl -n coding-agents get secretstore agentdev-secrets
   ```

   The column `READY` shows `True`.

To turn the store off, set `SECRETS_STORE_ENABLED` to `false` in `kind.vars.yml`.

### Add a project secret (kind, before the API part)

1. Apply the ProjectSecret object:

   ```bash
   kubectl -n coding-agents apply -f - <<'EOF'
   apiVersion: agentdev.dev/v1alpha1
   kind: ProjectSecret
   metadata: {name: npm-registry}
   spec:
     scope: project
     project: owner/repo
     mode: broker
     broker: {upstream: "https://npm.pkg.github.com", scheme: bearer}
   EOF
   ```

2. Write the value into the store. Do not write the value on the command line: read it from a file.

   ```bash
   kubectl -n agentdev-secret-values create secret generic agentdev-ps-npm-registry --from-file=value=token.txt
   ```

3. Make sure that the copy is ready:

   ```bash
   kubectl -n coding-agents get projectsecret npm-registry
   ```

   The column `PHASE` shows `ready` after one minute or less (`SECRETS_REFRESH_SECONDS`, default 60).

To rotate the value, replace the Secret in step 2. To remove the project secret, delete the ProjectSecret object and
then the Secret `agentdev-ps-<n>`.

### Mode env

In mode `env`, the agent container gets the value as an environment variable. The model can read it and can write
it into a comment or a commit. Use `env` only for a value that has no other path, for example the password of a test
database. You must set `spec.env.riskAccepted: true`. Names such as `ANTHROPIC_*`, `GIT_*`, `PATH` and
`HTTPS_PROXY` are not permitted.

### Rules in the cluster

ValidatingAdmissionPolicies (`manifests/secrets-policy.yaml.tmpl`) make sure that:

- An ExternalSecret in the agents namespace has the name `ps-<n>`, uses only the store `agentdev-secrets` and reads
  only the key `agentdev-ps-<n>`.
- Only External Secrets Operator writes a Secret `ps-*` in the agents namespace.
- No agentdev service account writes a SecretStore or a ClusterSecretStore.

## Subscription usage limits

Each Claude subscription has a 5-hour usage window and a weekly usage window. After each Claude run, the agent loop
sends one usage report to the forge sidecar of the pod. The sidecar checks the report and writes one `AGENTDEV_USAGE`
line to the log of the `forge` container. The line gives the tokens and the use of each window. The operator reads
usage lines only from the `forge` container. A usage line in the log of the `agent` container is not used: the
operator records the SecurityEvent `usage-line-forged` (severity `medium`). The operator adds
the lines for each subscription. Agents that use the same Claude Secret use the same subscription.

When the use of a window is at the usage limit or more, the operator pauses the new runs of all agents on that
subscription. Runs in progress continue to the end. At the reset time of the window, the agents start new runs
again. You do not have to do an action. The operator records each pause as a SecurityEvent with the rule
`usage-five-hour`, `usage-weekly` or `usage-rate-limited` and the severity `low`. The operator makes one SecurityEvent
for each agent. Thus, when 3 agents use one subscription, one pause gives 3 events. The action text of each event
gives the name of the subscription.

When Claude refuses a run because of its own rate limit, that agent pauses at once until the reset time. Then the
operator pauses all agents on the same subscription until that time. If Claude gives no reset time, the pause is
1 hour.

A usage report can start a pause or make a pause longer. A usage report cannot stop a pause or make a pause shorter.
One report can pause for 5 hours at most. If the limit is still real after 5 hours, the next run reports it again.
When a report is not usual (the use goes up by more than 50 points in one report, or the reset time is after the end
of the window), the operator records the SecurityEvent `usage-anomaly` (severity `medium`). Examine these events: the
model can write a false report (risk R47 in `docs/security.md`).

The **Usage** page shows the global limits to all users. A user sees the projects and agents for which the user has
write access. Only agentdev administrators see the use of each subscription.

The **Agents** page shows a banner when an agent is paused for usage. The row of the agent shows `usage-paused`.

### Set the global usage limits

You must be in the authentik group `agentdev-admins`.

1. Open the web UI. Refer to *Open the web UI*.
2. Click **Usage**.
3. In the row **Global (administrators)**, type the 5-hour limit and the weekly limit in percent.
4. Click **Save**.
   The API records the change as an `AGENTDEV_AUDIT` line with the action `set-limits`.

NOTE: The default limits are 80 % for the 5-hour window and 90 % for the weekly window. To change the defaults, set
`USAGE_FIVE_HOUR` and `USAGE_WEEKLY` in `kind.vars.yml`, or `usage.five_hour` and `usage.weekly` in the Ansible
`instance.yml`. A value that an administrator sets in the web UI replaces the default.

NOTE: You can also send `PUT /api/usage/global` with `{"fiveHour": 80, "weekly": 90}`.

### Examine the usage of a subscription

1. Open the web UI and click **Usage**.
   The **Subscriptions** table shows the use of each window, the reset times, the tokens and the runs.
   The **Paused agents** table shows the agents that start no new runs, the reason and the end time.

NOTE: The operator keeps this data in the ConfigMap `agentdev-usage`. The agent pods read the key `paused.<agent>`.

### Reference: usage limits

| Item | Value |
|---|---|
| Global limits | ConfigMap `agentdev-usage-settings`, key `settings.json`. Only the API writes it. |
| Project limit | Lower than the global limit, or not set. The operator ignores a higher value. |
| Effective limit | The lower value of the global limit and the project limit. For an agent with more than one repository, the lowest value of all its repositories. |
| Default limits | `USAGE_FIVE_HOUR` (80) and `USAGE_WEEKLY` (90): `kind.vars.yml`; Ansible `usage.five_hour`, `usage.weekly`. |
| Scope of a rate-limit pause | All agents on the same Claude Secret. |
| Who can read the usage | `GET /api/usage`: an `agentdev-admins` member sees all. Other users see the global limits and only the projects and agents where they have write access on the forge. Claude Secret names and windows are for admins only (R35). |
| State and pauses | ConfigMap `agentdev-usage`. Only the operator writes it. Each agent pod mounts it read-only. |
| Pause event | SecurityEvent, source `operator`, rule `usage-<reason>`, severity `low`. It does not pause the Agent object. |
| Audit | `AGENTDEV_AUDIT` with `action: set-limits`, for each try (also refused tries). |

## GitHub App (registration)

On GitHub, the agents use one GitHub App (#2). The webhook URL and the redirect URL are part of the deployment. Thus,
the manifest is a template (`manifests/github-app-manifest.json.tmpl`). The script fills it from the instance file.

### Register the GitHub App

WARNING: Do not put the private key in git or in an issue. The private key opens each repository that has the App.
Put it in Secret Manager (GKE) or in a local Secret.

1. In `instance.yml` of the deployment repository, set the values of the App:

   ```yaml
   # instance.yml (deployment repo)
   github_app:
     name: my-agentdev                         # unique on GitHub
     owner: my-org                             # omit to own it with your personal account
     public_url: https://agentdev.example.com  # https://<ingress.webhook_host> of this deployment
   ```

2. Show the manifest: `scripts/github-app.sh render instance.yml`
3. Examine the permissions, the events and the URLs in the manifest.
4. Make the form: `scripts/github-app.sh form instance.yml`
   The script writes `.secrets/github-app/register.html`.
5. Open `.secrets/github-app/register.html` in a browser.
6. Confirm the App on github.com.
   GitHub sends the browser to `<public_url>/github-app/registered?code=...&state=...`.
   A 404 on this page is correct, because no service listens at this time.
7. In 1 h or less, copy the full URL from the browser.
8. Run:
   `scripts/github-app.sh convert instance.yml 'URL'`
   The script keeps the App ID, the private key and the webhook secret in `.secrets/github-app/` (mode 600).
9. Install the App on the repositories of the agents.

### Reference: the GitHub App

- The webhook URL is `<public_url>/hook`. The router examines `X-Hub-Signature-256`.
- `convert` compares `state` with the form that it generated.
- Permissions: write for contents, issues and pull requests; read for metadata, checks and statuses.
- The App has no `workflows` permission. Thus, agents cannot change `.github/workflows/`.
- The forge sidecar of each agent holds the App ID and the private key. It makes installation tokens for 1 h. Each
  token is only for the repositories of that agent.

### Connect the agents to the GitHub App

WARNING: Do not put the private key in `instance.yml` or in git. The App ID is not secret.

Before you start, register and install the App (see "Register the GitHub App").

On GKE:

1. Put the private key in Secret Manager:
   `gcloud secrets versions add github-app-private-key --data-file=.secrets/github-app/private-key.pem`
2. Put the webhook secret in Secret Manager:
   `gcloud secrets versions add github-app-webhook-secret --data-file=.secrets/github-app/webhook-secret`
3. In `instance.yml`, set `github_app.app_id` to the value in `.secrets/github-app/app-id`.
4. Make sure that each agent name has only lowercase letters, digits and hyphens.
5. Run the playbook.

On kind:

1. In `kind.vars.yml`, set `FORGE_KIND` to `github` and set `REPO` to the GitHub repository (`owner/name`).
2. Run the playbook with the tag `agents`: `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags agents`
   The agents poll GitHub. Webhooks need a public URL.

### Make the agent labels

GitHub does not let you assign an issue to an App. Thus, you give work to an agent with a label.

1. In each repository of the agents, make one label for each agent: `agent:<name>`, for example `agent:alice`.
2. Give the write permission (or more) to the persons who can give work to agents.

NOTE: The agent does the work only when the person who added the label last has the write, maintain or admin
permission. The agent examines this before each start. Else it does not do the work and it sends a security event.

### Reference: agents on GitHub

| Item | Value |
|---|---|
| Account on GitHub | `<app-slug>[bot]` for all agents |
| Commit author | `<agent> (agentdev)`, with the email address of the App account |
| Comment | ends with the line `agent <agent> (agentdev)`, added by the forge sidecar |
| Work | open issues with the label `agent:<agent>` |
| Branches | `agent/<agent>/...` |
| Security findings | the agent replies in the thread of each `github-advanced-security[bot]` comment. It does not resolve the thread |
| Not permitted | merge, labels, issue changes, GraphQL, `.github/workflows/` |

### Reference: agents on Gitea

| Item | Value |
|---|---|
| Account on Gitea | one account for each agent (the agent name) |
| Work | open issues assigned to the agent by a person with write access |
| Branches | `agent/<agent>/...` |
| Label on its own PR | `agent:<agent>` (#241). The deployment makes this label (the `kind.yml` tag `forge`, `scripts/bootstrap-project.sh`, Ansible role `gitea_forge`) |
| Router | finds the agent from the logins in the event and from the label `agent:<agent>` |
| Not permitted | merge, other labels, issue changes |

Differences from GitHub (#241):

- **Code scanning:** Gitea has no code scanning API. The agent does not reply to security findings on Gitea.
- **Inline review comments:** Gitea has no reply endpoint for a review comment. The agent answers all review
  comments in one PR comment. Gitea shows that comment in the conversation of the PR.
- **Session monitor:** `tools/ghwatch -forge gitea` reads a Gitea repository (token in `GITEA_TOKEN`). Gitea has no
  repository-wide list of issue events. Thus, the monitor does not show `action` events (who closed, who added a
  label) on Gitea. Gitea has no `blocked` merge state: the monitor shows `clean` or `conflict`.

### Connect an external Gitea on GKE

agentdev does not run Gitea on GKE. It connects to a Gitea that you already operate (#442). The in-cluster Gitea of
the Ansible role `gitea_forge` is for kind only.

1. In `ansible/instance.yml`, set `forge_kind: gitea` and `forge_url: https://<your gitea host>`.
   The playbook refuses a URL that is not `https://`. It adds the host to the egress hosts of the forge sidecar (Squid
   and the FQDN network policy). The agent container cannot reach the host.
2. On Gitea, make one account for each agent and give each a token with the scopes
   `write:repository,write:issue,read:user`.
3. Put each token in Secret Manager as `agent-<name>-forge-token`.
4. Optional: make the account `agentdev-api` with a token `read:repository,read:user` and put it in
   `agentdev-api-forge-token`. Make the account `agentdev-router` with a token `read:repository,read:issue` and put
   it in `router-poll-forge-token` (wake poll).
5. Put the webhook secret in `github-app-webhook-secret` (the name stays for Gitea too).
6. Do one of these steps for collaborators, labels, the webhook and branch protection:
   - Set them by hand on Gitea (the same rules as the reference below), or
   - Set `forge_init: {enabled: true}` and put an admin token of Gitea in the Secret Manager entry
     `gitea-admin-token`. This needs `external_secrets.mode: external`. In `terraform/instance.tfvars`, also set
     `external_secrets = true` and `forge_init = true` and apply (#459). Then the store `agentdev-platform-secrets`
     can read `gitea-admin-token` and `github-app-webhook-secret`. The Job `forge-init-<hash>` in `agentdev-platform` uses the Gitea API only. It makes the
     agents write collaborators, `agentdev-api` admin collaborator and `agentdev-router` read collaborator. It also
     makes the label `agent:<name>`, one webhook to the router, and the branch protection (one approval by
     `reviewer`). It does not make tokens or repositories. With `create_accounts: true` it also makes missing agent
     accounts. When Gitea is outside the cluster, set `forge_init.hook_url` to the public webhook URL of the router.
     For a private CA, put `ca.crt` in a ConfigMap in `agentdev-platform` and set `forge_init.ca_configmap`.

NOTE: The Job has no Kubernetes API token. Only the Job reads the Secret `forge-init` (ESO, #381). The same inputs
give the same Job name. To run it again with the same inputs, set `forge_init.rerun` to a new string.

## Limits and blocked agents

### Set a quota for the agents

CAUTION: When you set a `cpu` or `memory` limit, Kubernetes refuses each pod in the namespace that does not declare
CPU requests or memory limits.

1. In the project specification, set `limits:` (`pods`, `cpu`, `memory`).
2. Run the bootstrap.
   The bootstrap makes a `ResourceQuota` on the agents namespace (#27).

NOTE: Kubernetes refuses pods over the quota, from all sources. Until #17, all projects use the same namespace. Thus,
the quota is a limit for all projects together.

NOTE: The agents, the router and the interactive sessions of agentdev declare CPU requests and memory limits. A
different pod in this namespace must also declare them.

### Block an agent

A blocked agent stays stopped. Events do not start it (scale to zero). Deployments (the playbooks `kind.yml` and
`deploy.yml`) keep it at 0.

1. Run `scripts/agent-block.sh block <agent> [reason]`.
   The agent stops.

### Unblock an agent

1. Run `scripts/agent-block.sh unblock <agent>`.
   The agent starts and continues the work that waits.

### List the blocked agents

1. Run `scripts/agent-block.sh list`.

NOTE: The script sets the annotation `agentdev/blocked`. The UI (#12) will set the same annotation.

## Interactive sessions

`scripts/agent-chat.sh start <project.yaml>` starts a Job from `manifests/chat.yaml.tmpl`. For the user procedures,
refer to the [user guide](user-guide.md).

At the first start, the script makes the chat account `<project>-chat` of the project:

- The account is a read collaborator.
- Its token has the scope `read:repository, write:issue`.
- The token is in `.secrets/<project>-chat.token` and in the Secret `chat-<project>`.

The session pod has no Claude credential (#118):

- Claude Code sends the model calls to `claude-proxy` (`ANTHROPIC_BASE_URL`).
- The session uses the proxy key of one agent of the project (`CHAT_AGENT`). The budget and the usage of the session
  count for this agent.
- The NetworkPolicy `agent-egress` applies to the session pod. The pod cannot connect to `api.anthropic.com`. Other
  hosts are available only through `egress-proxy`.
- The session needs the credential proxy (`CLAUDE_PROXY=true` on `kind`, `claude_proxy: true` on GKE).

### Show the sessions that run

1. Run `kubectl -n coding-agents get jobs -l app=agent-chat`.

NOTE: Kubernetes removes a finished session 5 minutes after it ends.

### Reference: session settings

| Setting | Default | Meaning |
|---|---|---|
| `IDLE_SECONDS` | 1800 | The session stops after this time with no person connected. |
| maximum time | 8 h | `activeDeadlineSeconds` in the template. This applies also while a person is connected. |
| `CHAT_AGENT` | the first agent in `agents` of the project file | The agent for the Claude budget and usage of the session. The session uses the proxy key of this agent (`agent-<name>-proxy`). |
| `REMOTE_CONTROL` | false | Not available. The Claude app (#26) must have a full claude.ai login in the session pod, and the session pod has no Claude credential (#118). The script stops when the value is `true`. |
| `IMAGE_PULL_POLICY` | Never | Use `IfNotPresent` on a cluster that is not `kind`. |

If botboard runs (`kind.yml --tags botboard`, #26), each open session shows as a card (agent `chat`). The card changes to `done`
when the session stops. `CHAT_URL` sets the link of the card. When #12 is done, it will go to the chat page of the UI.

### Console chat sessions

The API can start sessions for the console Chat page (#346). These sessions are Jobs that run
`chat-session-server`. Only the API connects to the session pod (NetworkPolicy and a token for each session).

A session pod is the pod of its agent (#424). The API copies the pod template of the StatefulSet `agent-<name>` that
the operator makes, and changes only the parts that make it interactive:

- The `agent` container becomes the `chat` container. It runs `agent-chat-session` (`chat-session-server`) on port
  7681 with the session's control token, not the autonomous loop.
- The `forge` sidecar is the agent's sidecar: the same image, forge credential, egress proxy key and security context.
  It does not register with the router and does not listen for wakes (no `WAKE_TOKEN`, `REGISTER_TOKEN` or
  `SELF_URL`), and it does not file approval requests (`APPROVALS=false`).
- `/state` is an empty volume, not the agent's disk. The pod labels are `app=agent-chat`, so the pod is not in the
  agent's Service and the operator does not take it for the agent.

So the session uses the agent's paths: the repository comes through the sidecar (`agentdev-forge sync`, the same git
bundle as the loop), forge calls and pushes go through the sidecar and its rules (`agentdev-forge api`,
`agentdev-forge push agent/<name>/...`), and Claude uses the agent's identity at `claude-proxy`, its MCP Secret
`agent-<name>-mcp` and its egress allow-list. This works on GitHub too: the forge hosts are reached only by the
sidecar with its egress key. The `chat` container holds no forge token and no egress key.

Tool permissions: the session asks in the console (**Allow once** or **Deny**), not through the agent's approval
comments. The agent's approval path needs an issue or a pull request of the agent to comment on, and a session has
none. A push to a protected path is refused by the sidecar at once (as for an agent with approvals off).

The admission policy `agentdev-api-chat-jobs` allows only this shape: the containers `chat` (worker image) and
`forge` (forge image), the init container `state-init`, the service account `agent-<name>`, and only the Secrets of
the agent in the Job label, each in the container that has it in the agent pod. The API reads no Secret; its Role is
not changed. A session can start only after the operator has made the agent's StatefulSet.

1. Set `api.chat.enabled: true` in `ansible/instance.yml`.
2. Run the Ansible `deploy.yml`.
   On GKE, Ansible makes the Secret Manager entry `agentdev-api-chat-token-key` once.

| Setting (`api.chat`) | API variable | Default | Meaning |
|---|---|---|---|
| `enabled` | `CHAT_SESSIONS` | false | Turns on the session API. |
| `idle_minutes` | `CHAT_IDLE_SECONDS` | 30 | The session stops this time after the last viewer leaves. |
| `retention_days` | `CHAT_RETENTION_DAYS` | 30 | The API deletes transcripts that are older. |
| `max_sessions` | `CHAT_MAX_SESSIONS` | 0 (no limit) | Maximum sessions that run at the same time. |
| `max_sessions_per_user` | `CHAT_MAX_SESSIONS_PER_USER` | 0 (no limit) | Maximum sessions of one person. |
| | `CHAT_MAX_SECONDS` | 28800 | Maximum time of one session. |

Optional secret, on Gitea: `agentdev-chat-issue-token` (site admin, `write:issue`, used with `Sudo`, so the issue
author is the person). Since #424 there is no clone token: the Secret Manager entry `agentdev-chat-clone-token` is
not read any more (you can delete it), and the playbook removes the Secret `agentdev-chat-clone`.

Every start, message, permission answer, end and issue is an entry in the audit log (`GET /api/audit`).

Notes:

- The API and the worker must run the same `image_tag` (ADR 0006): the API starts the worker image's
  `chat-session-server`. Chat is off by default, so an instance that does not turn it on is not affected.
- A terminal view (`tmux attach -t chat` in the session pod) keeps a session open for at most 2 hours
  (`CHAT_TERMINAL_MAX_SECONDS` in the pod) after the last console viewer. Claude runs as the same user as tmux, so it
  could fake a terminal client; only console presence, reported by the API, renews the session without limit.
- The terminal view renders `/state/chat/events.jsonl`, which Claude can also write. It is display only; the console
  reads the events from the session server's memory.
- On GitHub, the issue author is the GitHub App, not the person. The issue body names the person who selected
  **Create issue**, and the audit log records them.

## Webhooks

Each repository has one webhook to the router. The playbook tag `agents` or the bootstrap registers it.

### Show the state of the router

1. Run this command:

   ```bash
   kubectl -n coding-agents exec deploy/webhook-router -- node -e \
     "require('http').get('http://localhost:8081/status',r=>r.pipe(process.stdout))"
   ```

### Reference: the router

- The router examines the forge signature with `FORGE_SECRETS`.
- The router accepts agent registrations with `REGISTER_TOKEN`.
- The router has three ports (#214, #218). The public port 8080 (`PORT`) accepts only webhooks (`POST /hook` and
  `POST /`) and answers 404 for all other paths. The internal port 8081 (`INTERNAL_PORT`) gives `/healthz`, `/status`,
  `/register` and `/idle` to the agents and the operators of the instance. The health port 8082 (`HEALTH_PORT`) gives
  only `GET /healthz` to the probes and to the health check of the load balancer. Publish only the public port.
- For github.com, the router must have a public HTTPS URL and a GitHub App. On GKE, the public entry point gives the
  URL `https://<ingress.webhook_host>/hook` (refer to [Public HTTPS entry point (GKE)](#public-https-entry-point-gke)).
- On GitHub, the router finds the agent from the label `agent:<name>` and from the branch `agent/<name>/...`. The
  login of the App account is the same for all agents. Thus, the router does not use logins on GitHub.

## Upgrades

agentdev uses strict SemVer. While the version is `0.x`, a MINOR version change can contain changes that are not compatible.

### Upgrade an instance

1. Read `CHANGELOG.md`.
2. In the deployment repository, change the module `ref=vX.Y.Z` and `image_tag` to the same version.
3. Open one pull request with the two changes.
4. Make sure that the checks of the pull request pass. The pull request checks do not use cloud credentials, so
   also do the dry run manually: `ansible-playbook <agentdev checkout>/infra/ansible/deploy.yml -e @ansible/instance.yml --check --diff`.
   If the dry run shows `no value for ${...}`, the new
   release has a manifest value that your playbook version does not fill. Make sure that `ref=` and `image_tag`
   are the same version.
5. Look for the `template-drift` warning in the job summary of the pull request (#317). If there is a warning,
   copy the changed parts of `examples/deployment/gcp-gke` at the new release into your repository.
   An upgrade does not change your copy of `.github/workflows/deploy.yml`. You must copy it.

You can also run the check on your computer:
`<agentdev checkout>/scripts/template-drift.sh <deployment repo dir>`. The script reads `ref=` from
`terraform/main.tf`. It shows the diff of `deploy.yml`, the blocks and module arguments of `main.tf`, and the keys
of `ansible/instance.yml`. Instance values are not compared. With `--strict`, the script exits 1 on drift.

### What happens to running jobs during an upgrade

The operator replaces an agent pod only when the agent has no running job (#174). An idle or stopped agent changes at
once. A busy agent finishes its jobs first, for at most `rollout.drain_max_minutes` (default 60). After that time, the
operator replaces the pod. The agent saves its work in progress, and the job continues on the new pod.

1. Look at the state of the agents:
   `kubectl -n coding-agents get agents -o custom-columns='NAME:.metadata.name,UPDATED:.status.conditions[?(@.type=="Updated")].reason'`
2. Wait until all agents show `Current`.

### Drain an agent by hand

Do this task before maintenance of a node, for example.

1. Set the annotation: `kubectl -n coding-agents annotate pod agent-<name>-0 agentdev/drain=$(date +%s)`
2. Look for the line `drain requested: no new jobs` in `kubectl -n coding-agents logs agent-<name>-0 -c agent`.
3. If the pod has the current template, it stays drained. To end the drain, remove the annotation:
   `kubectl -n coding-agents annotate pod agent-<name>-0 agentdev/drain-`

### Show the plan on deployment pull requests

Do this task once per deployment repository on GKE. Then each pull request gets a `terraform plan` comment (#291).
The plan uses the read-only service account `agentdev-plan`. This account cannot change resources.

The identity is opt-in (`create_plan_identity`, default `false` since #421), because the apply account of the
bootstrap may not create it.

1. Use an agentdev release that contains #291 and #421 (`ref=` and `image_tag`).
2. You (the owner) give the apply account these one-time grants. The apply account cannot give them to itself.
   Without them `terraform apply` stops half way with 403 (#421).
   ```bash
   SA=agentdev-automation@<project_id>.iam.gserviceaccount.com
   gcloud projects add-iam-policy-binding <project_id> --member=serviceAccount:$SA \
     --role=roles/iam.workloadIdentityPoolAdmin --condition=None
   gcloud storage buckets add-iam-policy-binding gs://<project_id>-tfstate --member=serviceAccount:$SA \
     --role=roles/storage.admin
   # only with create_budget = true (the default)
   gcloud billing accounts add-iam-policy-binding <billing_account_id> --member=serviceAccount:$SA \
     --role=roles/billing.admin
   ```
   - `roles/iam.workloadIdentityPoolAdmin`: create the pool `github-pr` (`iam.workloadIdentityPools.create`).
   - `roles/storage.admin` on the state bucket: the read grant for the plan account
     (`storage.buckets.getIamPolicy`, `storage.buckets.setIamPolicy`).
   - `roles/billing.admin` on the billing account: the `roles/billing.viewer` grant for the plan account
     (`billing.accounts.setIamPolicy`; only with `create_budget`).
3. In `terraform/main.tf` set `create_plan_identity = true` in the module block. In `ansible/instance.yml` set
   `plan_identity: true` and `plan_identity_billing_account: <billing_account_id>` (`""` with
   `create_budget = false`; `plan_identity_state_bucket` if the state is not in `<project_id>-tfstate`).
   The preflight (`scripts/upgrade-check.sh`) then checks the grants and prints the missing commands.
4. Run `terraform plan`, then apply it. The apply makes `agentdev-plan`, the Workload Identity pool `github-pr`
   and the read grants.
5. Run `terraform output github_actions_auth`.
6. In the deployment repository, set the repository variable `GCP_PLAN_WIF_PROVIDER` to
   `plan_workload_identity_provider`.
7. Set the repository variable `GCP_PLAN_SERVICE_ACCOUNT` to `plan_service_account`.
8. Copy the new `.github/workflows/deploy.yml` from `examples/deployment/gcp-gke/`.
9. In the branch protection of `main`, require the `plan` check.
10. Make sure that only trusted persons can push branches to the repository. A pull request from a branch of the
    repository can read the Terraform state (docs/security.md R115).
11. Open a test pull request. Make sure that the `plan` job posts a comment that starts with `terraform plan`.

What the plan identity can do:

- It can read all resources of the project (`roles/viewer`) and their IAM policies (`roles/iam.securityReviewer`).
- It can read the Terraform state bucket (`roles/storage.objectViewer`). It cannot lock or write the state, so the
  plan runs with `-lock=false`.
- It can read the budget (`roles/billing.viewer`, only with `create_budget`).
- It cannot read Secret Manager values. It cannot change resources or IAM.
- Only `pull_request` runs of `github_repository` get it. A pull request from a fork gets no token.

To check that the identity cannot apply, run `terraform apply` with it once. The apply must fail with `403`.
To remove the identity, set `create_plan_identity = false` and `plan_identity: false`, then apply. You can then
remove the three owner grants again.

### Render the manifests for a GitOps engine

`infra/ansible/render.yml` renders the manifests of one instance to a directory (#384). A GitOps engine (Argo CD,
Flux) can then apply them. The render does not connect to the cluster or to Secret Manager. The output contains no
secret values. `deploy.yml` uses the same renderer (`tasks/render_objects.yml`), so both give the same objects.

The render reads the cluster facts from the instance values in `platform`:

| Value | Meaning | Source |
|---|---|---|
| `platform.egress.fqdn` | `gke`: FQDNNetworkPolicy for the egress proxies and the Claude proxy. `none`: Squid only | `terraform output platform` |
| `platform.ingress.gateway_api` | the cluster serves the Gateway API (necessary for `ingress.mode: gateway`) | `terraform output platform` |
| `platform.ingress.certificate_domains` | the domains of the managed certificate; `ingress.auth_host` must be one of them | `terraform output platform` |
| `platform.kube_version` | the Kubernetes minor version of the cluster, for `helm template` | `terraform output platform` |
| `platform.kube_api_ips` | the addresses of Service `default/kubernetes`, for the Claude proxy NetworkPolicy | `terraform output platform` |

1. Apply `infra/terraform` with an agentdev release that contains #384.
2. Copy `terraform output platform` into `platform` of `ansible/instance.yml`.
3. Render the manifests:
   ```bash
   ansible-playbook infra/ansible/render.yml -e @ansible/instance.yml -e render_dir=$PWD/rendered
   ```
4. Check the output:
   ```bash
   kubeconform -strict -ignore-missing-schemas rendered/
   gitleaks dir rendered/
   ```
5. With `external_secrets.mode: playbook`, make the Secrets in the list `rendered/SECRETS.md`. Use `deploy.yml`.
   With `external_secrets.mode: external`, go to the next step. The output contains the ExternalSecrets (#457).
6. Give the directory to the GitOps engine. Apply the files in the order of their numbers. For Flux, use the
   procedure "Install an instance with Flux".

The render also writes the platform (Postgres, ContextForge, `obs-mcp`), authentik and API objects (#447):

- The authentik chart is `helm template` output. The render needs `helm`. The output has no Helm hooks.
- The database steps are the same Jobs that `deploy.yml` applies (#382): `agentdev-db-init-<hash>` (roles, schema,
  grants and the schema version that the API waits for), `authentik-db-init-<hash>` and `authentik-breakglass-<hash>`.
  They read the passwords from the Secrets `platform-db`, `agentdev-api-db` and `authentik` by name. A changed schema
  gives a new Job name. Remove the Jobs of an old hash with the pruning of the GitOps engine.
- The authentik Deployments have the annotation `secret.reloader.stakater.com/reload: authentik,authentik-sso`.
  Reloader (#445) restarts them when one of these Secrets changes. Without Reloader, restart them by hand after a
  change: `kubectl -n agentdev-platform rollout restart deploy/authentik-server deploy/authentik-worker`. The pod
  annotation `agentdev/config-hash` changes with the values file, the blueprints and the chart version.
- `helm template` uses `platform.kube_version` (`terraform output platform`), or `1.36` when it is not set.
- The render writes one `McpProject` for each spec in `mcp_projects` (#383). The operator registers the servers.
- The render does not write the Secrets. With `external_secrets.mode: playbook`, their names are in `SECRETS.md`.
- With `external_secrets.mode: external` (#457), the render writes the objects of the External Secrets Operator in
  place of `SECRETS.md`. These are the same objects that `deploy.yml` applies:
  - `secrets-policy.yaml`: the CEL policies (ValidatingAdmissionPolicy) for Secrets and ExternalSecrets.
  - `secrets-stores.yaml`: one ServiceAccount and one SecretStore `agentdev-platform-secrets` in each namespace,
    with the Google service account of `terraform output secrets_readers`.
  - `externalsecrets.yaml`: the ExternalSecrets. They contain Secret Manager ids only, never a value.
  - With `forge_init.enabled`, the ExternalSecret `forge-init` and the Job `forge-init-<hash>` (#442).

  CAUTION: The GitOps engine must install the External Secrets Operator and Stakater Reloader before these files.
  Use the chart versions of `infra/ansible/roles/external_secrets/defaults/main.yml`, with the same values as the role.
  The render does not write the two charts.

NOTE: Without `platform`, `deploy.yml` operates as before: `egress_fqdn_policy` sets `platform.egress.fqdn`, and
`deploy.yml` reads the API server addresses from the cluster. `deploy.yml` does not read the cluster to select a
template. The preflight (`crds`) compares `platform` with the cluster.

The render refuses bad values before it writes a file. It uses the same checks as `deploy.yml` (`tasks/checks.yml`):
labels, executors, approvals, Claude budget and wake source. Thus the CI of a deployment repository finds them
before a merge.

### Install an instance with Flux

Flux applies the manifests of an instance from its deployment repository (#385). The deploy workflow renders the
manifests and pushes them to the branch `gitops`. Flux applies that branch. The workflow does not call the Flux API.

The template `examples/deployment/gcp-gke/gitops/flux/` has four layers. Flux applies them in this order
(`dependsOn`):

| Layer | Flux Kustomization | Contents |
|---|---|---|
| infrastructure | `agentdev-infrastructure` | External Secrets Operator chart 2.14.0 and the Helm repositories |
| platform | `agentdev-platform` | the `render.yml` output without the agents: namespaces, CRDs, router, operator, platform, authentik, API, ingress, ExternalSecrets |
| agents | `agentdev-agents` | the Agent and Executor objects (#58) |
| reloader | `agentdev-reloader` | Stakater Reloader v2.0.0-beta.2 (chart 3.0.0-beta.5), after the platform layer |

Reloader runs in scoped mode. It makes a Role in each namespace that it watches, thus it starts after the platform
layer made these namespaces. The workflow sets the watched namespaces to the namespaces that have a
rendered SecretStore (`rendered/reloader`). This is the same set as the Ansible role `external_secrets` (#466).
Thus `agentdev-executors` is watched only with executors, and the forwarder namespace only when the forwarder is
deployed.

All layers use `prune: true`: Flux removes an object that is removed from Git. Flux also corrects drift: it applies
the state of Git again at each interval, and the HelmReleases use drift detection. The Flux controllers, the External
Secrets Operator and Reloader run on the `system` node pool. They never stop the agent pool from scaling to zero
(#181). No StatefulSet has `replicas` in Git, because the router sets the replicas of the agents.

Before you start:

- You did steps 1 to 6 of `AGENTS.md` (Terraform applied, Secret Manager values filled in).
- `ansible/instance.yml` has `platform` from `terraform output platform` and `external_secrets: {mode: external}`.
- You have the `flux` CLI v2.10.1 (the version of the agentdev release check, `scripts/check-flux-tree.sh`) and
  `kubectl` access to the cluster.

agentdev's Flux objects are named `agentdev`: the `GitRepository`, the root `Kustomization`, the deploy key Secret
and the prefix `agentdev-` of the layers and the Helm repositories. `flux bootstrap` uses the name `flux-system`. Thus
agentdev never changes the root sync of another tenant on the same cluster (#472).

First, find out if Flux is already installed on the cluster:

```bash
kubectl get crd kustomizations.kustomize.toolkit.fluxcd.io
```

- The command shows `NotFound`: agentdev installs Flux. Do all the steps below.
- The command shows the CRD: Flux is already installed. agentdev adds only its own objects and uses the Flux
  controllers that are there. Do the extra part of step 1, skip steps 2 and 9, and skip the first command of step 10.

1. Copy `examples/deployment/gcp-gke/gitops/` and `.github/workflows/deploy.yml` of the pinned release into the
   deployment repository.
   Flux already installed only: remove the line `- flux-system` from
   `gitops/flux/clusters/agentdev/kustomization.yaml`, and remove the directory
   `gitops/flux/clusters/agentdev/flux-system`. The existing Flux must be v2.x with the APIs
   `source.toolkit.fluxcd.io/v1`, `kustomize.toolkit.fluxcd.io/v1` and `helm.toolkit.fluxcd.io/v2`, and it must
   watch the namespace `flux-system`.
2. Make sure that `flux version --client` shows `v2.10.1`. Then write the Flux controllers into the repository:
   ```bash
   flux install --export > gitops/flux/clusters/agentdev/flux-system/gotk-components.yaml
   ```
   The Flux controllers run on the node pool `system` (#181). If your cluster has no pool with this name, set
   `value` in `gitops/flux/clusters/agentdev/flux-system/controller-nodepool.yaml` to the name of your system pool.
3. Copy `gitops/flux/clusters/agentdev/sync/gotk-sync.yaml.example` to `gotk-sync.yaml`.
4. In `gotk-sync.yaml`, set `url` to the SSH address of the deployment repository.
5. Set the repository variable `DEPLOY_ENGINE` to `flux`. The workflow then runs the jobs `render` and `verify` in
   place of the job `deploy`.
6. Merge these changes into `main`. The job `render` pushes the branch `gitops`. If Flux is not installed yet, the
   job `verify` skips its checks and shows the notice "Flux not installed". This is expected.
7. Make a read-only deploy key and the Secret `agentdev`:
   ```bash
   kubectl create namespace flux-system    # only when Flux is not installed
   flux create secret git agentdev --namespace flux-system --url=ssh://git@github.com/<owner>/<deployment-repository>
   ```
   The command shows a public key.
8. Add the public key as a read-only deploy key of the deployment repository.
9. Install the Flux controllers and their CRDs, and wait until the CRDs are established:
   ```bash
   kubectl apply -f gitops/flux/clusters/agentdev/flux-system/gotk-components.yaml
   kubectl wait --for condition=established --timeout=120s crd --all
   ```
10. Install the node pool patch of the Flux controllers (skip this command when Flux was already installed), then
    agentdev's sync, the `GitRepository` and the root `Kustomization` named `agentdev`:
    ```bash
    kubectl apply -k gitops/flux/clusters/agentdev/flux-system
    kubectl apply -k gitops/flux/clusters/agentdev/sync
    ```
11. Wait until all layers are ready:
    ```bash
    flux get kustomizations --watch
    ```
    `agentdev`, `agentdev-infrastructure`, `agentdev-platform`, `agentdev-agents` and `agentdev-reloader` show
    `Ready True`. When Flux was already installed, the `Kustomization` objects of the other tenants do not change.
12. Run the workflow `deploy` on `main`: in GitHub, open Actions, select `deploy`, then select "Run workflow". The job
    `verify` waits until Flux applied the new revision, then it does the smoke test of the public hosts
    (`infra/ansible/smoke.yml`).

The job `verify` reads the status of the engine with `scripts/gitops-status.sh`. A layer is ready when its `Ready`
condition is `True` for the current generation and for the commit of the job `render`. A stalled layer stops the job
at once. A stopped agent is ready (#386), thus the instance is healthy with all agents stopped.

CAUTION: Do not run `deploy.yml` of Ansible on a cluster that Flux operates. Both change the same objects.

NOTE: Flux does not make the Secrets. The External Secrets Operator makes them from Secret Manager. A change of an
agent (for example a new image) does not start a stopped agent. The change applies when the agent starts next.

### Upgrade the local installation

Do this task after you pull a new `main` into an existing `kind` cluster.

1. Read the **BREAKING** entries in `CHANGELOG.md` to find the playbook tags that you must run again.

CAUTION: Do not run only the tag `api`. The API needs the CRDs from the tag `operator`, and the agent pods need the
forge image from the tag `agents`. If you remove Docker images (for example with `docker image prune`), run the tag
`agents` again.

2. Load the secrets: `set -a; source .env; set +a`
3. Run the playbook with these tags:
   `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags agents,api,operator`
   - The tag `agents` builds the worker, forge and router images and loads them into `kind`.
   - The tag `api` builds the API and applies the database grants.
   - The tag `operator` builds the operator and applies the CRDs, for example `SecurityEvent`.
4. Make sure that the agent pods show `2/2 Running`:
   `kubectl --context kind-agentdev -n coding-agents get pods`
   The agent pods start with the new worker and forge images.

NOTE: The tag `agents` restarts the router, the Claude proxy and the agents only when an image or a Secret changed.
The tag `agents` needs the operator. A plain run of the playbook installs it.

### Move from run.sh to the playbook (1.3.0)

`run.sh`, the numbered step scripts `00-prereqs.sh` to `13-secrets.sh` and `99-teardown.sh` are removed in 1.3.0
(#417). They were deprecated in 1.1.0 (#106). The Coder spike (`04-coder.sh`) is removed and has no tag. The
playbook `infra/ansible/kind.yml` does all the other steps. It does not read `config.env`.

Before you start, install `ansible-playbook` (ansible-core 2.17 or later).

1. Copy the example settings to the repository root:
   `cp infra/ansible/kind.example.vars.yml kind.vars.yml`
2. Find each value that you changed in `config.env`.
   The example file shows the Ansible variable next to each `config.env` name.
3. Write each changed value to its Ansible variable in `kind.vars.yml`.
4. Keep the namespaces in `config.env` the same as in `kind.vars.yml`.
   The scripts in `scripts/` still read `config.env`.
5. Load the secrets: `set -a; source .env; set +a`
6. Run the playbook: `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml`

WARNING: Do not put a secret in `kind.vars.yml`. Keep `FORGE_ADMIN_PASSWORD`, `ANTHROPIC_API_KEY` and
`CLAUDE_CODE_OAUTH_TOKEN` in `.env`.

NOTE: `kind.vars.yml` is in `.gitignore`. The files in `.secrets/` do not change. The playbook does not write
`results.md`.

| Old command | New command (`ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml ...`) |
|---|---|
| `./run.sh` (steps 00, 01, 05, 06 and the operator) | no tags (a plain run: prereqs, cluster, forge, operator, agents) |
| `./run.sh 00` | `--tags prereqs` |
| `./run.sh 01` | `--tags cluster` |
| `./run.sh 02` | `--tags sandbox` |
| `./run.sh 03` | `--tags sandbox-test` |
| `./run.sh 04` | removed, no tag |
| `./run.sh 05` | `--tags forge` |
| `./run.sh 06` | `--tags agents` |
| `./run.sh 07` | `--tags botboard` |
| `./run.sh 08` | `--tags mcp` |
| `./run.sh 09` | `--tags authentik` |
| `./run.sh 10` | `--tags api` |
| `./run.sh 11` | `--tags ui` |
| `./run.sh 12` | `--tags operator` |
| `./run.sh 13` | `--tags secrets` |
| `./99-teardown.sh` | `--tags teardown` |

A plain run does not run the tags `sandbox`, `sandbox-test`, `botboard`, `mcp`, `authentik`, `api`, `ui`, `secrets`
and `teardown`. Run them with `--tags`.

## Remove agentdev

### Reference: the kind playbook and the GKE playbook

The playbook `infra/ansible/deploy.yml` does on GKE what `infra/ansible/kind.yml` does on `kind`. It uses the same
manifests. You do not need a manual `kubectl` command on GKE.

| Component | `kind` (`kind.yml` tag) | GKE (playbook) |
|---|---|---|
| Host prerequisites (tools, engine, kind version) | `prereqs` | not used |
| Cluster with gVisor | `cluster` | `infra/terraform` |
| agent-sandbox and warm pool (evaluation) | `sandbox` | not used |
| Checks of the warm pool, suspend and resume (evaluation) | `sandbox-test` | not used |
| Gitea forge, accounts, seeded repo | `forge` | not used (GitHub, #2) |
| botboard (optional) | `botboard` | not used (`botboard_url`, an external botboard) |
| Webhook router | `agents` (role `kind_agents`) | `deploy.yml` |
| Egress proxy | `agents` | `deploy.yml` |
| Claude credential proxy | `agents` | `deploy.yml` (`claude_proxy: true`) |
| Operator and CRDs | `operator` | `deploy.yml` |
| Scoped secrets store (External Secrets Operator, policies, store) | `secrets` (role `secrets_store`) | not yet (GKE parity in #202 part 4, PR #274) |
| Platform namespace | `mcp` (role `platform`) | role `platform` |
| authentik | `authentik` (roles `authentik` and `authentik_kind`) | role `authentik` |
| API | `api` (role `api`) | role `api` (`api.oidc_issuer`) |
| Web UI | `ui` (role `api`) | role `api` (`api.ui`) |
| Public entry point | port-forward | role `ingress` (`ingress.host`) |
| Delete the cluster | `teardown` (tagged `never`) | `terraform destroy` |

- The playbook waits until the operator, router, egress proxy, Claude proxy, API and web UI are ready. If one of
  them is not ready, the playbook stops with an error.
- The playbook fills the manifest values of each component. If a manifest has a value that the playbook does not
  fill, the playbook stops before it changes the cluster. The error shows the name of the value, for example
  `${OIDC_ISSUER}`.

### Remove the local installation

CAUTION: This procedure deletes the cluster and all data in it.

1. Run `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags teardown`.
   The tag `teardown` is also tagged `never`: no other run of `kind.yml` deletes the cluster.

### Remove a GKE instance

CAUTION: This procedure deletes the instance and all data in it.

1. In the deployment repository, run `terraform destroy`.

NOTE: To remove an agent, refer to *Remove an agent*.

## Documentation

The documentation is in `docs/`. Persons review it in pull requests. The GitHub wiki is a copy of `docs/` on `main`.
A workflow updates the wiki. Thus, the wiki always has the reviewed text.
