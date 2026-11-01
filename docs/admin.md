# Administration

This guide is for the administrator of an agentdev instance. The local paths refer to the `kind` installation
(`config.env`, `.secrets/`, `run.sh`). On GKE, the same settings are Ansible variables in the deployment repository
(`ansible/instance.yml`). On GKE, the secrets are in Secret Manager.

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

1. On `kind`: add the agent to `AGENTS` in `config.env`, for example `AGENTS="alice bob carol"`.
   On GKE: add the agent to the `agents:` list in `instance.yml`.
2. On `kind`: run `./run.sh 05 06`.
   On GKE: open a pull request in the deployment repository.

### Remove an agent

CAUTION: Step 3 deletes the work data of the agent.

1. Remove the agent from the agent list (`AGENTS` or `agents:`).
2. Deploy again: `./run.sh 06`, or a pull request in the deployment repository.
3. Delete the StatefulSet, the volumes and the forge account of the agent.

### Give an agent its own Claude credential

WARNING: Do not put the credential in git or in an issue.

1. Get a credential for the Claude subscription: `claude setup-token`.
2. On `kind`: write `CLAUDE_CODE_OAUTH_TOKEN=...` or `ANTHROPIC_API_KEY=...` in `.secrets/<agent>.claude`.
   On GKE: write the `KEY=VALUE` lines in Secret Manager `agent-<agent>-claude`.
3. Deploy again: `./run.sh 06`, or a pull request in the deployment repository.

NOTE: Each agent uses one Claude credential. It uses its own credential if it has one. If not, it uses the shared
credential (#5). The shared credential is `.env` on `kind` and Secret Manager `claude-shared` on GKE.

NOTE: Only the Claude proxy reads the credential. The agent pod gets a proxy key, not the credential (#66).

### Replace a credential

Use this task to replace a Claude credential or a forge token.

1. Replace the file or the secret.
2. Deploy again: `./run.sh 06`, or a pull request in the deployment repository.

### Set the concurrency

The concurrency is the number of items that one agent works on at the same time.

1. On `kind`: set `MAX_CONCURRENT` (the default is 2).
   On GKE: set `max_concurrent`, or `agents[].max_concurrent` for one agent.
2. Deploy again.

NOTE: The memory limit of an agent is (1 + concurrency) GiB. At the default of 2, the limit is 3 GiB. The operator
sets the limit.

### Set the poll interval

Agents read the forge at this interval when webhooks do not operate.

1. On `kind`: set `POLL_SECONDS`. On GKE: set `poll_seconds`.
2. Deploy again.

### Find the security events of an agent

1. Run this command:
   `kubectl -n coding-agents logs agent-<name>-0 --all-containers | grep AGENTDEV_SECURITY_EVENT`
   The output shows one JSON line for each action that a guardrail stopped.

### Reference: the agent pod

| Item | Value |
|---|---|
| Containers | `agent` (the loop and Claude, no forge credentials, no Claude credential: only a proxy key, #66) and `forge` (the sidecar that keeps the forge token, image `agentdev-forge`, #64) |
| Log of the sidecar | `kubectl -n coding-agents logs agent-<name>-0 -c forge` |
| The sidecar refuses a `git push` | to a branch outside `agent/<name>/*`; that touches `PROTECTED_PATHS` (default: the workflow directories); of more than `MAX_PUSH_MB` (50) |
| Security events | each refusal (in the sidecar or the loop) is an `AGENTDEV_SECURITY_EVENT` JSON line. Rules: [security-model.md](security-model.md#phase-1-forge-sidecar-fixes-r4) |
| Egress | through `egress-proxy` only (`HTTPS_PROXY`), to the hosts of the allow-list. Refer to *Egress allow-list* |
| Sidecar image | on `kind`, `./run.sh 06` builds it (`FORGE_IMAGE`). Ansible uses `forge_image` (the same `image_tag`) |

| Setting | `kind` | GKE (Ansible) |
|---|---|---|
| Agent list | `AGENTS` in `config.env`, then `./run.sh 05 06` | `agents:` in `instance.yml` |
| Own Claude credential | `.secrets/<agent>.claude` | Secret Manager `agent-<agent>-claude` (`KEY=VALUE` lines) |
| Shared Claude credential | `.env` | Secret Manager `claude-shared` |
| Concurrency | `MAX_CONCURRENT` (default 2) | `max_concurrent`, `agents[].max_concurrent` |
| Poll interval (when webhooks do not operate) | `POLL_SECONDS` | `poll_seconds` |

## Claude proxy and token budget

The Claude proxy (`claude-proxy`) is the only pod that has the Claude credentials (#66). An agent sends its model
requests to the proxy with its proxy key. The proxy adds the real credential and sends the request to the Anthropic
API. The proxy also counts the tokens of each agent and stops an agent that used its budget.

### Set the token budget of all agents

The budget is the number of tokens (input, cache and output) that one agent can use in 24 hours.

1. On `kind`: set `CLAUDE_BUDGET_TOKENS` in `config.env`. On GKE: set `claude_budget_tokens` in `instance.yml`.
2. Deploy again: `./run.sh 06`, or a pull request in the deployment repository.

NOTE: The value 0 means no limit. This is the default.

### Set the token budget of one agent

1. Run this command (use 0 for no limit):
   `kubectl -n coding-agents patch configmap claude-proxy-budgets --type merge -p '{"data":{"<agent>":"500000"}}'`
2. Wait one minute. Kubernetes updates the file in the proxy pod, and the proxy reads it at the next request.

NOTE: A deploy does not change this value. The template of the ConfigMap has no data.

### Look at the token use

1. Open the web UI. The column **Claude tokens (24 h)** shows the tokens of each agent and the budget.
2. Or run this command:
   `kubectl -n coding-agents exec deploy/webhook-router -- node -e "require('http').get('http://claude-proxy:8080/status',r=>r.pipe(process.stdout))"`

### Reference: the Claude proxy

| Item | Value |
|---|---|
| Image | `agentdev-credproxy`. On `kind`, `./run.sh 06` builds it (`CLAUDE_PROXY_IMAGE`). Ansible uses `credproxy_image` |
| Credentials | the Secrets `claude-shared` and `claude-<agent>`, mounted only in the proxy pod |
| Proxy key of an agent | the Secret `agent-<agent>-proxy`, key `ANTHROPIC_AUTH_TOKEN`. It is the agent name and an HMAC of the name |
| Signing key | the Secret `claude-proxy-signing`. On `kind`: `.secrets/claude-proxy-signing.key`. On GKE: Secret Manager `claude-proxy-signing` |
| Forwarded requests | `POST /v1/messages` and `POST /v1/messages/count_tokens` only |
| When the budget is used | the proxy gives HTTP 429 and writes one `budget-exhausted` security event. The agent continues when the 24-hour window has free tokens |
| Usage of the subscription | the proxy reads the rate-limit headers of each response and writes `AGENTDEV_USAGE` lines (#93) |
| Old Claude login on the volume | when the proxy is on, the agent removes `~/.claude/.credentials.json` and the keys in `~/.claude.json` at each start |
| Usage of the subscription (#93) | only from the proxy. The forge sidecar refuses `POST /usage` |
| Turn off the proxy | `CLAUDE_PROXY=false` (`kind`) or `claude_proxy: false` (GKE). This is deprecated: the credential is then in the agent pod again (R11) |

WARNING: Do not give the proxy key of an agent to a person. The key lets the holder use the Claude budget of that
agent from inside the cluster.

## Scale to zero

With scale to zero, the router stops an agent that has no work, and starts it when an event occurs. Scale to zero is
off by default.

### Turn on scale to zero

1. On `kind`: set `SCALE_TO_ZERO=true`.
   On GKE: set `agents[].scale_to_zero` for each agent.
2. Optional: set `IDLE_SECONDS` (the default is 900).
3. Deploy again.

### Examine the state of scale to zero

1. Show the replicas of the agents:
   `kubectl -n coding-agents get sts -o custom-columns=NAME:.metadata.name,REPLICAS:.spec.replicas`
2. Show the scale actions of the router:
   `kubectl -n coding-agents logs deploy/webhook-router | grep scaled`

NOTE: The router must have a namespaced Role to scale the agent StatefulSets. The manifest has this Role.

NOTE: When you apply the manifests again (a new deployment), stopped agents start one time. They stop again after the
idle time.

## Agent dashboard (botboard)

Botboard ([source](https://github.com/valentinalexeev/botboard), `ghcr.io/valentinalexeev/botboard`) shows the work
of the agents. It shows one card for each issue.

### Deploy botboard

1. Run run.sh step 07: `./run.sh 07`
   The script deploys botboard in the namespace `botboard`.
2. Run run.sh step 06 again: `./run.sh 06`
   The script finds botboard and sets `BOTBOARD_URL` on each agent.

### Open botboard

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

MCP tools give the agents of a project more tools through ContextForge. On `kind`, run.sh step 08 deploys
ContextForge. On GKE, the Ansible playbook deploys it.

### Deploy the platform namespace

1. Optional: in `config.env`, set `CONTEXTFORGE_IMAGE`, `POSTGRES_IMAGE` and `CF_ADMIN_EMAIL`.
2. Run run.sh step 08: `./run.sh 08`
   The script deploys the namespace `agentdev-platform` with Postgres and ContextForge.

NOTE: Postgres has the databases `contextforge` and `agentdev`. The generated passwords and keys are in
`.secrets/platform/`, never in git.

### Deploy the platform namespace on GKE

The playbook deploys Postgres and ContextForge in `agentdev-platform` on each run. It makes the passwords and keys
on the first run and keeps them in Secret Manager as `platform-<key>`, for example `platform-cf-admin`.

1. In the deployment repository, put the project specifications in a directory, for example `projects/`.
2. In `ansible/instance.yml`, list the specifications:

   ```yaml
   mcp_projects:
     - "{{ lookup('env', 'PWD') }}/projects/web.yaml"
   ```

   Use full paths on the machine that runs the playbook. `PWD` is the deployment repository.
   List the hosts of the MCP servers that you trust in `mcp_allowed_hosts` (refer to the next section).
3. Run the playbook (refer to `infra/ansible/README.md`).
   The playbook runs `scripts/mcp-sync.sh` for each specification after ContextForge is ready.
4. Make sure that the pods are ready: `kubectl -n agentdev-platform get pods`

CAUTION: Do not delete a `platform-pg-*` secret. Postgres keeps the first password on its volume. A new value stops
the connection from ContextForge.

NOTE: The dry run (`--check`) does not run the sync.

### Add MCP servers to a project

1. In the project specification, list the servers:

   ```yaml
   mcp:
     servers:
       - {name: docs, url: http://docs-mcp.tools.svc.cluster.local:8080/mcp}
   ```

2. Add the host of each server to the allow-list. Use `host` or `host:port`.
   - On kind: `MCP_ALLOWED_HOSTS` in `config.env` (separate the hosts with spaces).
   - On GKE: `mcp_allowed_hosts` in `ansible/instance.yml`.

   CAUTION: Add only servers that you trust. The names, descriptions and outputs of their tools go to the model.
   A bad server can put instructions in them (prompt injection). Refer to R51 in `docs/security.md`.

   The sync stops if a server is on a host that is not in the list. The sync makes no changes in that case.
3. Run the sync: `scripts/mcp-sync.sh <spec>`
   The bootstrap also runs this script.
4. Start each agent again to get the new tools: `kubectl -n coding-agents delete pod agent-<name>-0`

NOTE: If a specification has `mcp:` and you did not run `./run.sh 08`, the bootstrap stops before it deploys the
agents. The bootstrap tells you the cause.

### Open the ContextForge admin UI

1. Start the port-forward: `kubectl -n agentdev-platform port-forward svc/contextforge 4444:4444`

### Reference: the MCP sync

You can run `scripts/mcp-sync.sh` again with no risk. It does these actions:

- It stops if a server host is not in the allow-list.
- It registers each server as the gateway `<project>-<name>`.
- It builds the virtual server `project-<project>` from the tools of the gateways.
- It makes one token for each agent, for that server only.
- It keeps the token in the Secret `agent-<name>-mcp` (`CF_URL`, `CF_TOKEN`).

Tokens are valid for one year. A sync in the last 30 days of a token makes a new token and revokes the old token.
When you remove an agent from `agents:` (or remove `mcp:`), the next sync deletes its Secret and revokes its token.

The worker gives Claude `--strict-mcp-config`, so only the tools of the gateway are available. NetworkPolicies close
Postgres to all pods except ContextForge. They close ContextForge to all namespaces except the agents namespace. GKE
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
database `authentik` on the platform Postgres, with no Redis. On GKE, the playbook does not deploy it yet (#51).

### Deploy authentik

Do this task after run.sh step 08.

1. Optional: in `config.env`, set `AUTHENTIK_VERSION` and `AUTHENTIK_ADMIN_EMAIL`.
2. Run run.sh step 09: `./run.sh 09`
   The script installs [authentik](https://goauthentik.io) with its Helm chart (`manifests/authentik-values.yaml`).

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

The API finds the forge user of a person from the token claim `forge_login` (#88). It does not use the authentik
username, because a user can choose a username. The claim comes from the authentik user attribute `forge_login`.
Users cannot change this attribute in the default setup, but only by configuration (see the WARNING below). The blueprint adds the scope `forge`, which sends this claim to the API.

The intended setup: authentik keeps no users and passwords of its own. It signs people in through the same company
single sign-on as the forge (for example Google Workspace). The source property mapping of that identity provider
sets `forge_login`.

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
3. In **Attributes**, add `forge_login: <forge login>`.
4. Click **Update**.

The person logs out and logs in again. Then the new token has the claim.

WARNING: Do not let users edit `forge_login`. Only the identity provider or the administrators set it. A person with
a wrong `forge_login` can act as that forge user.

- Do not add an enrollment flow. The blueprint has none.
- No flow, stage or prompt may write `attributes.forge_login`. authentik writes every prompt whose field key is
  `attributes.<name>` to the user. The default user settings flow (`default-user-settings-flow`) already writes
  `attributes.settings.locale` this way. A prompt with the field key `attributes.forge_login` in the user settings
  flow, an enrollment flow or a source flow lets users choose their forge user. Check this in
  **Flows and Stages > Prompts** (search `forge_login`) after setup and after each authentik upgrade.
- Use the forge login in lower case.

NOTE: If the token has no `forge_login`, or the value has upper case letters or characters other than `a-z`, `0-9`,
`.`, `_`, `-`, the API finds no forge user. The person can read, but cannot change agents. Members of
`agentdev-admins` can pause, add and remove agents without a forge user.

NOTE: On GKE, authentik is not installed by the playbook at this time. Apply
`manifests/authentik-blueprint-agentdev.yaml` to your authentik instance, or make the scope mapping in the admin
interface: scope name `forge`, the expression from the blueprint, and add it to the `agentdev` provider.

### Give a person admin rights for agents

The members of the authentik group `agentdev-admins` can pause, add and remove all agents. The blueprint of step
09 makes this group. On `kind`, step 09 also adds `akadmin` to the group once. On other instances, the group is empty.

IMPORTANT: No automated process changes the members of `agentdev-admins`. The blueprints create the group, but never
set its members, so a re-apply of a blueprint keeps the admins you add here. On `kind`, step 09 only adds `akadmin`
and never removes a member. Step 09 checks this each run: it adds a test user, re-applies the blueprints, and fails
if a member is gone. Add and remove members by hand in authentik only.

1. Log in to authentik as an administrator.
2. Go to **Directory > Groups** and open `agentdev-admins`.
3. Add the user on the **Users** tab.
4. Tell the user to log in to agentdev again. The API reads the groups from the token, and an old token does not
   have the new group.

CAUTION: Give this group to platform operators only. A member can add an agent to each repository of the forge.

### Reference: authentik

- The configuration is the Secret `authentik`. The script generates it from `.secrets/platform/`.
- The first admin is you: the user `akadmin`, with the email `AUTHENTIK_ADMIN_EMAIL`.
- The agentdev API and web UI (#46, #48) are OIDC applications in authentik.
- The blueprint `manifests/authentik-blueprint-agentdev.yaml` makes the OIDC application and the group
  `agentdev-admins`, without members. On `kind`, `09-authentik.sh` adds `akadmin` to the group through the authentik
  API (add only, never remove).
- The platform namespace denies all traffic by default. The API and the web UI add a NetworkPolicy rule to connect
  to authentik.

## Management API

The agentdev API (`images/api`, Go, #46) shows the agents of the instance. It also pauses, resumes, changes, adds and removes
agents (#57). On GKE, the Ansible playbook deploys it when `instance.yml` sets `api.oidc_issuer`.

### Deploy the API

Do this task after run.sh step 09.

1. Set `API_OIDC_ISSUER` in `config.env` to the URL that you use to log in to authentik.
   On `kind`, with the 9443 port-forward, this URL is `https://localhost:9443/application/o/agentdev/`.
2. Run run.sh step 10: `./run.sh 10`
   The script builds the API and deploys it in `agentdev-platform`. On `kind`, it also gives the API access to the
   forge. Refer to "Give the API access to the forge".

NOTE: authentik writes the login URL into the token. Thus, `API_OIDC_ISSUER` must be the same URL.

### Deploy the API on GKE

The playbook deploys the API after the platform namespace. It uses the same manifest, schema and grants as run.sh
step 10. The playbook does not deploy authentik yet (#114). Thus, you must use an OIDC provider that you have.

WARNING: Do not put secret values in `instance.yml`. Put them only in Secret Manager.

1. In the OIDC provider, make a public client `agentdev` with the authorization code and PKCE.
2. In the OIDC provider, make the group `agentdev-admins`. Put the groups of the user in the claim `groups`.
3. Add the API settings to `ansible/instance.yml`:

   ```yaml
   api:
     oidc_issuer: https://auth.example.com/application/o/agentdev/
   ```

4. Optional: give the API access to the forge. Refer to "Give the API access to the forge".
5. Open a pull request in the deployment repository. Merge it after the dry run.
   The playbook deploys the API in `agentdev-platform`.
6. Make sure that the API is ready: `kubectl -n agentdev-platform rollout status deploy/agentdev-api`

NOTE: Without `api.oidc_issuer`, the playbook does not deploy the API.

### Reference: the API settings on GKE

| `instance.yml` | Default | `kind` (`config.env`) |
|---|---|---|
| `api.oidc_issuer` | none (the API is not deployed) | `API_OIDC_ISSUER` |
| `api.oidc_discovery_url` | `api.oidc_issuer` | the authentik Service in the cluster |
| `api.oidc_audience` | `agentdev` | `agentdev` |
| `api.forge_url` | `forge_url` | the Gitea Service in the cluster |
| `api.db_retention_days` | `365` | `SECURITY_DB_RETENTION_DAYS` |

- The playbook makes the database password one time in Secret Manager `platform-pg-agentdev-api`.
- The database login `agentdev_api` can only add and read rows. The role `agentdev_owner` owns the tables.
- The playbook writes the Secrets `agentdev-api-db`, `agentdev-api-forge` and `agentdev-api-siem`. When a Secret
  changes, the playbook restarts the API.
- The optional secrets `agentdev-api-forge-token` and `security-siem` can be empty. When a secret has no enabled
  version, the playbook acts as if the secret is not there.

### Connect to the API

1. Start the port-forward: `kubectl -n agentdev-platform port-forward svc/agentdev-api 8088:8080`
2. Send each `/api` request with an authentik access token of the OIDC application `agentdev`.
   Without a token, the API gives 401.

### Reference: the API

- A blueprint in run.sh step 09 makes the application `agentdev`. It is a public client with the authorization code
  and PKCE.
- The API gets the keys from authentik in the cluster.
- The API reads the agents and writes Agent objects with a Role on the agents namespace.

| Endpoint | Gives |
|---|---|
| `GET /api/me` | the caller (`sub`, `preferred_username`, `email`) |
| `GET /api/agents` | each agent: `name`, `repos`, `forge`, `phase` (`running`, `starting`, `stopped`, `paused`, `blocked`, `invalid`), replicas, `managed` (an Agent object exists; `false` for a StatefulSet from before the operator), `paused`, `maxConcurrent`, `scaleToZero` (#57), `securityEvents` (count, last 7 days, #70) |
| `GET /api/audit` | audit trail of the write API (#91), newest first. Only `agentdev-admins` (403 for other users). Filters: `agent`, `result` (`ok`, `denied`, `invalid`, `error`, `unauthenticated`), `since`, `limit` (1..500, default 100), `cursor` as for security events |
| `GET /api/security-events` | security events (`agentdev.security/v1`), newest first. Filters: `agent`, `severity`, `since` (RFC 3339 or a duration such as `24h`), `limit` (1..500, default 100); `next_cursor` in the answer, pass it as `cursor` for the next page (#70). Every signed-in user sees every event for now; restriction to projects with write access is #57 |

| `PATCH /api/agents/{name}` | changes `paused`, `maxConcurrent` (1 to 8) or `scaleToZero` (JSON merge patch). Gives the agent. |
| `POST /api/agents` | adds an agent: `name`, `repo`, and optional `forge`, `claudeSecret`, `maxConcurrent`, `scaleToZero`. Gives 201. |
| `DELETE /api/agents/{name}` | removes the Agent object. The volumes stay. `?keepState=true` is the default; `false` gives 400. Gives 204. |

### Give the API access to the forge

The API asks the forge for the permission of the user before each change. Do this task before you change agents
through the API.

WARNING: Do not use a token with write access. The API only reads the permissions of the users.

Gitea tells the permission of another user only to a site admin or to an admin of the repository. With any other
token, each change gives 503. Thus, the token user must have admin rights. Use a dedicated service account (for
example `agentdev-api`) that is admin of the agent repositories only, or a site admin if you accept that.

On `kind`, `./run.sh 10` does this task for you. The script makes the Gitea user `agentdev-api` and makes it admin
of `REPO` and of the repository of each Gitea agent. It makes a token with the scopes `read:repository` and
`read:user` and puts it in the Secret `agentdev-api-forge`. If the Secret has a token of `agentdev-api` that
works, the script keeps it. The script does not show the token. After you add an agent on a new repository, run
`./run.sh 10` again.

On GKE, do these steps manually. The playbook does not do them yet (#104).

1. Sign in to the forge as the service account. Make a token with the scopes `read:repository` and `read:user`
   only.
2. Put the token in the Secret `agentdev-api-forge`.
   On GKE: put the token in Secret Manager `agentdev-api-forge-token`, then run the playbook. Do not do step 3.
   On `kind`:
   `kubectl -n agentdev-platform create secret generic agentdev-api-forge --from-literal=FORGE_API_TOKEN=<token>`
3. Restart the API: `kubectl -n agentdev-platform rollout restart deploy/agentdev-api`

NOTE: Without the Secret, each change that needs the forge gives 503.

NOTE: At this time, the API can only ask a Gitea forge (#87). On GKE with GitHub, each change that needs the forge
gives 503. Members of `agentdev-admins` can still pause agents.

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

### Reference: who can change an agent

The owner decided this rule on #57. The API checks the permission on the forge at the time of each change.

| Action | Permission |
|---|---|
| Pause an agent | write access to the repository of the agent, or the authentik group `agentdev-admins` |
| Resume an agent, change `maxConcurrent` or `scaleToZero` | write access to the repository of the agent |
| Add or remove an agent | admin access to the repository, or the group `agentdev-admins` |

- The API finds the forge user from the claim `forge_login` (scope `forge`). If the token has no such claim, the
  API refuses the change. Refer to "Map people to forge users". The function `forgeUserOf` in
  `images/api/forge.go` does this mapping.
- The API reads the groups from the `groups` claim of the token (scope `profile`).
- If the API cannot reach the forge, it refuses the change (503). Only the group `agentdev-admins` can then pause,
  add and remove agents.
- The permission check of the API is for Gitea only. For an agent on GitHub, only `agentdev-admins` can pause, add
  and remove the agent.
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
StatefulSet of the agent. These are the same objects that run.sh step 06 makes. The operator keeps them the same as
the spec, and shows the phase in `status.phase`.

### Install the operator

1. Run run.sh step 12: `./run.sh 12`
   The script installs the `Agent` CRD (`agentdev.dev/v1alpha1`) and the operator (#47).

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
- run.sh step 06 makes Agent objects (#57). The API reads them (`managed: true`).
- The API adds, pauses and removes agents (#57). The next work: buttons in the UI (#57 step 4, #4).
- Agents in the deployment repository with Argo CD or Flux: #58.

## Web UI

The web UI shows the agents of the instance and their status. You sign in to the web UI through authentik. The web
UI is in the `agentdev-platform` namespace. On GKE, the role `api` deploys the UI, and the public entry point
makes it available on HTTPS (refer to [Public HTTPS entry point (GKE)](#public-https-entry-point-gke)).

### Deploy the web UI

Do this task after run.sh step 10 (the API).

1. Run run.sh step 11: `./run.sh 11`

The script builds the web UI image and deploys it to the cluster.

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
   Terraform makes the managed certificate and the SSL policy, and turns on the Gateway API of the cluster.
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

NOTE: On kind, approvals are off by default (`APPROVALS=false`), because `./run.sh` does not install the API by
default. To turn on approvals on kind, do the steps that follow.

1. Set `APPROVALS=true` in `.env`.
2. Optional: set the time that the agent waits for a decision with `APPROVAL_TIMEOUT_MINUTES`. The default is 30
   minutes. After this time, the agent continues without the action, but the request stays open.
3. Run `./run.sh 10` and then `./run.sh 12`.
   The operator starts the agent pods again with the new setting.

NOTE: Decisions are in the audit log of the API (`AGENTDEV_AUDIT` lines with the action `approval-...`).

NOTE: Agents and the forge account of the API cannot make a decision. To exclude other service accounts (for
example the account of the router or of a GitHub App), set `APPROVAL_DENY_ACCOUNTS` to a list of forge logins.

NOTE: At this time, approvals operate only on Gitea. On a GitHub repository, nobody can make a decision (issue #87).

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
3. Run run.sh step 10: `./run.sh 10`
   The script puts the file into the Secret `agentdev-api-siem` and starts the API again.
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
6. Run run.sh step 10: `./run.sh 10`
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
4. Run run.sh step 10: `./run.sh 10`
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
| `SECURITY_PAUSE_AFTER` | 5 | `config.env`; Ansible `security.pause_after` |
| `SECURITY_RETENTION_DAYS` (SecurityEvent objects) | 30 | `config.env`; Ansible `security.retention_days` |
| `SECURITY_MAX_PER_AGENT` (SecurityEvent objects) | 500 | `config.env`; Ansible `security.max_per_agent` |
| `SECURITY_RATE_PER_MIN` (events per minute per container) | 10 | `config.env`; Ansible `security.rate_per_min` |
| `SECURITY_RATE_BURST` (events above the rate for a short time) | 20 | `config.env`; Ansible `security.rate_burst` |
| `SECURITY_DB_RETENTION_DAYS` (Postgres table) | 365 | `config.env`; `./run.sh 10` writes it to the database |

SIEM outputs: the API reads these keys from the Secret `agentdev-api-siem`. All keys are optional.

| Key | Secret | kind (`./run.sh 10`) | GKE (Ansible) |
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
NetworkPolicy operates only with kind v0.24 or later. `./run.sh 00` makes sure of this.

### Add a host to the allow-list

Do this task when the build of a project needs a host that is not on the list, for example a different package
registry.

WARNING: Add only a host that you trust. An agent can send data to each host on the list.

1. Add the host to the list:
   - On `kind`: set `EGRESS_EXTRA_HOSTS` in `.env`, for example `EGRESS_EXTRA_HOSTS="repo.maven.apache.org .example.com"`.
   - On GKE: add the host to `egress_extra_hosts` in `instance.yml`.
2. Write a dot before the name to also allow all subdomains: `.example.com`.
3. Apply the change:
   - On `kind`: run `./run.sh 06`.
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
| Allow-list | the Claude API, GitHub, npm, PyPI, Go, crates.io | `EGRESS_HOSTS` in `config.env`; Ansible `egress_hosts` |
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
   EOF
   ```

3. In the forge, add the branch protection rule for the branch: require the status check `agentdev/trusted-plan`
   and require that the branch is up to date.
4. Add the forge account of the executor to `APPROVAL_DENY_ACCOUNTS` of the API. The executor writes tool output in
   comments. This step makes sure that no comment of the executor can decide an approval.
5. Show the executors: `kubectl -n agentdev-executors get executors`
   The column PHASE shows `ready`.

NOTE: With Ansible, put the executors in `executors:` of the instance file
(`examples/deployment/gcp-gke/ansible/instance.yml.example`). The playbook makes the Secrets, the service accounts
and the Executor objects.

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
| `EXEC_NS` (namespace of the executors) | `agentdev-executors` | `config.env`; Ansible `exec_ns` |
| `EXECUTOR_POLL_SECONDS` (time between forge polls) | 60 | `config.env`; Ansible `executor_poll_seconds` |
| `EXECUTOR_RETENTION_DAYS` (finished runs; minimum 8) | 30 | `config.env`; Ansible `executor_retention_days` |
| `EXECUTOR_APPLY_START_MINUTES` (an allowed apply starts within this time after the decision, else nothing is applied) | 30 | `config.env`; Ansible `executor_apply_start_minutes` |
| `spec.approvalHours` (a decision counts only within this time after the request) | 24 | `Executor` |
| `spec.allowForkPRs` (also plan pull requests from forks) | false | `Executor`; Ansible `allow_fork_prs` |

- Each run start and each result is a security event with the source `executor` and the rule `executor.plan`,
  `executor.apply` or `executor.refused`.
- The executor plans only pull requests that change a file below `spec.path`. It plans only merges after the
  `Executor` object was made, and not older than 24 hours.
- One executor per environment. If two `Executor` objects have the same `spec.environment`, the oldest one runs.
  The other ones show the phase `invalid` with the reason `DuplicateEnvironment`, and an `executor.refused` event is
  recorded. The Ansible playbook stops before it changes anything if two executors have the same environment.
- By default the executor plans only pull requests from branches of the same repository. A pull request from a fork
  runs the code of its author with the plan credentials. Set `spec.allowForkPRs: true` only if the plan credentials
  can read nothing secret (risk R81; egress limits are #65).
- The approval covers the image digest, the apply command and a SHA-256 of the `Executor` spec (also the
  credentials). If you change the `Executor` after the merge plan, the pending approval no longer applies: the run
  is closed with `executor spec changed`. Merge a new commit to plan again.
- The executor Job has no Kubernetes API token. It runs in gVisor on the sandbox nodes.
- The egress allow-list of the agents does not apply to the executors. The executor Jobs do not use the egress proxy,
  because the tools need the cloud APIs of the environment. An executor Job can connect to all hosts. An allow-list
  for the executors is planned in #135.
- Design: [security model, phase 5](security-model.md#phase-5-deterministic-executors-69).

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
`USAGE_FIVE_HOUR` and `USAGE_WEEKLY` in `config.env`, or `usage.five_hour` and `usage.weekly` in the Ansible
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
| Default limits | `USAGE_FIVE_HOUR` (80) and `USAGE_WEEKLY` (90): `config.env`; Ansible `usage.five_hour`, `usage.weekly`. |
| Scope of a rate-limit pause | All agents on the same Claude Secret. |
| Who can read the usage | Each user who can sign in to the web UI (`GET /api/usage`). Refer to risk R35 in `security.md`. |
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

1. In `config.env`, set `FORGE_KIND=github` and set `REPO` to the GitHub repository (`owner/name`).
2. Run `./run.sh 06`.
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

A blocked agent stays stopped. Events do not start it (scale to zero). Deployments (`./run.sh 06` and the Ansible
playbook) keep it at 0.

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

### Show the sessions that run

1. Run `kubectl -n coding-agents get jobs -l app=agent-chat`.

NOTE: Kubernetes removes a finished session 5 minutes after it ends.

### Reference: session settings

| Setting | Default | Meaning |
|---|---|---|
| `IDLE_SECONDS` | 1800 | The session stops after this time with no person connected. |
| maximum time | 8 h | `activeDeadlineSeconds` in the template. This applies also while a person is connected. |
| `CLAUDE_SECRET` | `claude-shared` | The Claude credential of the session. The credential is visible in the session pod. |
| `REMOTE_CONTROL` | false | Also make the session available to the Claude app (#26). This must have a full claude.ai login: run `/login` in the session. API keys and `claude setup-token` tokens do not operate (inference-only). |
| `IMAGE_PULL_POLICY` | Never | Use `IfNotPresent` on a cluster that is not `kind`. |

If botboard runs (`./run.sh 07`, #26), each open session shows as a card (agent `chat`). The card changes to `done`
when the session stops. `CHAT_URL` sets the link of the card. When #12 is done, it will go to the chat page of the UI.

## Webhooks

Each repository has one webhook to the router. The run.sh step 06 (`06-agents.sh`) or the bootstrap registers it.

### Show the state of the router

1. Run this command:

   ```bash
   kubectl -n coding-agents exec deploy/webhook-router -- node -e \
     "require('http').get('http://localhost:8080/status',r=>r.pipe(process.stdout))"
   ```

### Reference: the router

- The router examines the forge signature with `FORGE_SECRETS`.
- The router accepts agent registrations with `REGISTER_TOKEN`.
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

### Upgrade the local installation

Do this task after you pull a new `main` into an existing `kind` cluster.

1. Read the **BREAKING** entries in `CHANGELOG.md` to find the run.sh steps.

CAUTION: Do not run only step 10. The API needs the CRDs from step 12, and the agent pods need the forge image from
step 06. If you remove Docker images (for example with `docker image prune`), run step 06 again.

2. Run these steps in this sequence: `./run.sh 06 10 12`
   - Step 06 builds the worker, forge and router images and loads them into `kind`.
   - Step 10 builds the API and applies the database grants.
   - Step 12 builds the operator and applies the CRDs, for example `SecurityEvent`.
3. Make sure that the agent pods show `2/2 Running`:
   `kubectl --context kind-agentdev -n coding-agents get pods`
   The agent pods start with the new worker and forge images.

NOTE: Step 06 runs step 12 only when the operator is not installed. On an existing cluster, step 12 must come after
step 06 to update the CRDs.

## Remove agentdev

### Remove the local installation

CAUTION: This procedure deletes the cluster and all data in it.

1. Run `./99-teardown.sh`.

### Remove a GKE instance

CAUTION: This procedure deletes the instance and all data in it.

1. In the deployment repository, run `terraform destroy`.

NOTE: To remove an agent, refer to *Remove an agent*.

## Documentation

The documentation is in `docs/`. Persons review it in pull requests. The GitHub wiki is a copy of `docs/` on `main`.
A workflow updates the wiki. Thus, the wiki always has the reviewed text.
