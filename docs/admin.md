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
| Containers | `agent` (the loop and Claude, no forge credentials) and `forge` (the sidecar that keeps the forge token, image `agentdev-forge`, #64) |
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

MCP tools give the agents of a project more tools through ContextForge. On GKE, the playbook does not deploy
ContextForge yet (#51). Until then, projects on GKE have no MCP tools.

### Deploy the platform namespace

1. Optional: in `config.env`, set `CONTEXTFORGE_IMAGE`, `POSTGRES_IMAGE` and `CF_ADMIN_EMAIL`.
2. Run run.sh step 08: `./run.sh 08`
   The script deploys the namespace `agentdev-platform` with Postgres and ContextForge.

NOTE: Postgres has the databases `contextforge` and `agentdev`. The generated passwords and keys are in
`.secrets/platform/`, never in git.

### Add MCP servers to a project

1. In the project specification, list the servers:

   ```yaml
   mcp:
     servers:
       - {name: docs, url: http://docs-mcp.tools.svc.cluster.local:8080/mcp}
   ```

2. Run the sync: `scripts/mcp-sync.sh <spec>`
   The bootstrap also runs this script.
3. Start each agent again to get the new tools: `kubectl -n coding-agents delete pod agent-<name>-0`

NOTE: If a specification has `mcp:` and you did not run `./run.sh 08`, the bootstrap stops before it deploys the
agents. The bootstrap tells you the cause.

### Open the ContextForge admin UI

1. Start the port-forward: `kubectl -n agentdev-platform port-forward svc/contextforge 4444:4444`

### Reference: the MCP sync

You can run `scripts/mcp-sync.sh` again with no risk. It does these actions:

- It registers each server as the gateway `<project>-<name>`.
- It builds the virtual server `project-<project>` from the tools of the gateways.
- It makes one token for each agent, for that server only.
- It keeps the token in the Secret `agent-<name>-mcp` (`CF_URL`, `CF_TOKEN`).

Tokens are valid for one year. A sync in the last 30 days of a token makes a new token and revokes the old token.
When you remove an agent from `agents:` (or remove `mcp:`), the next sync deletes its Secret and revokes its token.

The worker gives Claude `--strict-mcp-config`, so only the tools of the gateway are available. NetworkPolicies close
Postgres to all pods except ContextForge. They close ContextForge to all namespaces except the agents namespace. GKE
applies the NetworkPolicies. Local `kind` (kindnet) does not.

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

### Keep the usernames safe

The intended setup: authentik keeps no users and passwords of its own. It signs people in through an external
identity provider, the same single sign-on that the forge uses (for example Google Workspace). Thus, one company
account gives the authentik username and the forge login. The API uses the authentik username as the forge login
(#57). An authentik user who can pick a name can act as the forge user with that name. Thus, keep these settings:

- Connect authentik to the company identity provider as a source, and take the username from it. Use the same
  identity provider for the forge.
- Only the identity provider or the administrators make authentik users. Do not add an enrollment flow. The
  blueprint has none.
- In **System > Settings**, keep **Allow users to change username** off (the default). The blueprint cannot set
  this, so check it after each authentik upgrade.
- Give each person the same username in authentik and on the forge, in lower case.

NOTE: The API refuses a username that has upper case letters or characters other than `a-z`, `0-9`, `.`, `_`, `-`.
Such a user can read, but cannot change agents. A stable forge-login claim is planned in #88.

### Reference: authentik

- The configuration is the Secret `authentik`. The script generates it from `.secrets/platform/`.
- The first admin is you: the user `akadmin`, with the email `AUTHENTIK_ADMIN_EMAIL`.
- The agentdev API and web UI (#46, #48) are OIDC applications in authentik.
- The platform namespace denies all traffic by default. The API and the web UI add a NetworkPolicy rule to connect
  to authentik.

## Management API

The agentdev API (`images/api`, Go, #46) shows the agents of the instance. It also pauses, resumes, changes, adds and removes
agents (#57). On GKE, refer to #51.

### Deploy the API

Do this task after run.sh step 09.

1. Set `API_OIDC_ISSUER` in `config.env` to the URL that you use to log in to authentik.
   On `kind`, with the 9443 port-forward, this URL is `https://localhost:9443/application/o/agentdev/`.
2. Run run.sh step 10: `./run.sh 10`
   The script builds the API and deploys it in `agentdev-platform`.

NOTE: authentik writes the login URL into the token. Thus, `API_OIDC_ISSUER` must be the same URL.

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

1. Sign in to the forge as the service account. Make a token with the scopes `read:repository` and `read:user`
   only.
2. Put the token in the Secret `agentdev-api-forge`:
   `kubectl -n agentdev-platform create secret generic agentdev-api-forge --from-literal=FORGE_API_TOKEN=<token>`
3. Restart the API: `kubectl -n agentdev-platform rollout restart deploy/agentdev-api`

NOTE: Without the Secret, each change that needs the forge gives 503.

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

- The API finds the forge user from the authentik username. At this time, the two names must be the same.
  If the names are different, the API refuses the change. The function `forgeUserOf` in `images/api/forge.go` does
  this mapping.
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
UI is in the `agentdev-platform` namespace. For GKE, refer to issue #51.

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

### Reference: settings

| Setting | Default | Where |
|---|---|---|
| `SECURITY_PAUSE_AFTER` | 5 | `config.env`; Ansible `security.pause_after` |
| `SECURITY_RETENTION_DAYS` (SecurityEvent objects) | 30 | `config.env`; Ansible `security.retention_days` |
| `SECURITY_MAX_PER_AGENT` (SecurityEvent objects) | 500 | `config.env`; Ansible `security.max_per_agent` |
| `SECURITY_RATE_PER_MIN` (events per minute per container) | 10 | `config.env`; Ansible `security.rate_per_min` |
| `SECURITY_RATE_BURST` (events above the rate for a short time) | 20 | `config.env`; Ansible `security.rate_burst` |
| `SECURITY_DB_RETENTION_DAYS` (Postgres table) | 365 | `config.env`; `./run.sh 10` writes it to the database |

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
| More hosts | none | `EGRESS_EXTRA_HOSTS`; Ansible `egress_extra_hosts` |
| Proxy image | `ubuntu/squid` `6.6-24.04_beta` (pinned digest; Canonical has no other tag for 24.04) | `EGRESS_PROXY_IMAGE`; Ansible `egress_proxy_image` |
| Proxy address for the agents | `http://egress-proxy.coding-agents.svc.cluster.local:3128` | operator env `EGRESS_PROXY` (empty = no proxy) |
| FQDN policy for the proxy (GKE) | on | Ansible `egress_fqdn_policy`; Terraform `enable_fqdn_network_policy`. If it is on and the cluster does not have it, the playbook stops |

NOTE: The proxy refuses all private, loopback and link-local addresses, also when an allowed name points to one.

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
     public_url: https://agentdev.example.com  # HTTPS address of this deployment's webhook router
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

- The webhook URL is `<public_url>/`. The router examines `X-Hub-Signature-256`.
- `convert` compares `state` with the form that it generated.
- Permissions: write for contents, issues and pull requests; read for metadata, checks and statuses.
- The App has no `workflows` permission. Thus, agents cannot change `.github/workflows/`.
- The worker that uses the App is work that #2 must still do.

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
- For github.com, the router must have a public HTTPS URL (Ingress) and a GitHub App. This is planned in #2.

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
