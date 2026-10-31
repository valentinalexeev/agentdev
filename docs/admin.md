# Administration

Local paths below refer to the kind setup (`config.env`, `.secrets/`, `run.sh`). On GKE the same settings are
Ansible variables in the deployment repo (`ansible/instance.yml`) and secrets are in Secret Manager.

## Projects

Connect an existing repository with one spec:

```bash
cp examples/project.yaml my-project.yaml      # set forge.repo, forge.owner, agents
scripts/bootstrap-project.sh my-project.yaml
```

It creates agent accounts and tokens, sets owner-only-approval branch protection (an existing rule is kept;
`REPLACE_BRANCH_PROTECTION=1` replaces it), adds labels, proposes the process pack in a PR, deploys the agents and
waits until each agent posts **ready** on an "agentdev: project connected" issue. Re-running it is safe.

v1 limits: Gitea only, issues on the same forge, one repository per agent deployment.

## Agents

| Task | kind | GKE (Ansible) |
|---|---|---|
| Add or remove an agent | `AGENTS="alice bob carol"` in `config.env`, `./run.sh 05 06` | `agents:` list in `instance.yml` |
| Own Claude credential | `.secrets/<agent>.claude` with `CLAUDE_CODE_OAUTH_TOKEN=...` or `ANTHROPIC_API_KEY=...` | Secret Manager `agent-<agent>-claude` (`KEY=VALUE` lines) |
| Shared Claude credential | `.env` | Secret Manager `claude-shared` |
| Parallel items per agent | `MAX_CONCURRENT` (default 2) | `max_concurrent`, per agent `agents[].max_concurrent` |
| Poll interval (webhook fallback) | `POLL_SECONDS` | `poll_seconds` |

Each agent pod has two containers: `agent` (loop and Claude, no forge credentials) and `forge` (the sidecar that
holds the forge token, image `agentdev-forge`, #64). Logs: `kubectl -n coding-agents logs agent-<name>-0 -c forge`.
The sidecar refuses pushes outside `agent/<name>/*`, pushes touching `PROTECTED_PATHS` (default the workflow
directories) and pushes over `MAX_PUSH_MB` (50). Every refusal, in the sidecar or the loop, is logged as an
`AGENTDEV_SECURITY_EVENT` JSON line (rules: [security-model.md](security-model.md#phase-1-forge-sidecar-fixes-r4));
`kubectl -n coding-agents logs agent-<name>-0 --all-containers | grep AGENTDEV_SECURITY_EVENT` lists them. On kind `./run.sh 06` builds it (`FORGE_IMAGE`); Ansible uses
`forge_image` (same `image_tag`).

Each agent uses exactly one Claude credential: its own if present, otherwise the shared one (#5).
An agent's memory limit is (1 + parallel items) GiB, so 3 GiB at the default of 2 (set by the operator).

Credentials for the Claude subscription come from `claude setup-token`. Rotate a credential by replacing the file
or secret and redeploying (`./run.sh 06`, or a deployment PR).

## Scale to zero

Off by default. Turn it on with `SCALE_TO_ZERO=true` (and optionally `IDLE_SECONDS`, default 900), or per agent
with `agents[].scale_to_zero` in Ansible. The router needs a namespaced Role to scale agent StatefulSets; the
manifest includes it. Check the state:

```bash
kubectl -n coding-agents get sts -o custom-columns=NAME:.metadata.name,REPLICAS:.spec.replicas
kubectl -n coding-agents logs deploy/webhook-router | grep scaled
```

Re-applying the manifests (a redeploy) starts stopped agents once; they stop again after the idle time.

## Agent dashboard (botboard)

`./run.sh 07` deploys [botboard](https://github.com/valentinalexeev/botboard) (`ghcr.io/valentinalexeev/botboard`)
in namespace `botboard`; the next `./run.sh 06` finds it and sets `BOTBOARD_URL` on every agent. View it with
`kubectl -n botboard port-forward svc/botboard 8090:80` and http://localhost:8090. The board keeps state in memory:
a restart clears it, and agents re-publish as they work.

On GKE, agents report to whatever botboard `botboard_url` in `ansible/instance.yml` points at; the playbook does not
deploy botboard itself yet. botboard has no authentication of its own: inside the cluster any pod can post cards, so
its intended exposure is your tailnet (run it with `TS_AUTHKEY` and use `http://botboard.<tailnet>`), not a public
URL. Card links use `FORGE_PUBLIC_URL` (`forge_public_url` in Ansible; locally the Gitea host URL) so they open in a
browser rather than pointing at the in-cluster forge address.

## MCP tools

`./run.sh 08` deploys the platform namespace `agentdev-platform`: Postgres (databases `contextforge` and
`agentdev`) and ContextForge (`CONTEXTFORGE_IMAGE`, `POSTGRES_IMAGE`, `CF_ADMIN_EMAIL` in `config.env`). Generated
passwords and keys stay in `.secrets/platform/`, never in git. Then list servers in the project spec:

```yaml
mcp:
  servers:
    - {name: docs, url: http://docs-mcp.tools.svc.cluster.local:8080/mcp}
```

`scripts/bootstrap-project.sh` runs `scripts/mcp-sync.sh <spec>`, which you can also run alone after changing
`mcp:`. It is idempotent: it registers each server as gateway `<project>-<name>`, builds virtual server
`project-<project>` from their tools, mints one token per agent scoped to that server, and stores it in Secret
`agent-<name>-mcp` (`CF_URL`, `CF_TOKEN`). Restart the agent (`kubectl delete pod agent-<name>-0`) to pick up
new tools. Tokens are valid for a year; any sync in the last 30 days mints a new one and revokes the old. An agent
removed from `agents:` (or a spec without `mcp:`) loses its Secret and its token is revoked on the next sync.
The worker passes Claude `--strict-mcp-config`, so only the gateway's tools are available.
NetworkPolicies close Postgres to everything but ContextForge, and ContextForge to everything but the agents
namespace. GKE enforces them; local kind (kindnet) does not.
The ContextForge admin UI: `kubectl -n agentdev-platform port-forward svc/contextforge 4444:4444`.

If a spec has `mcp:` and `./run.sh 08` was never run, bootstrap stops before deploying agents and says so.

On GKE the Ansible playbook does not deploy ContextForge yet (#51); projects there have no MCP tools until it does.

## Identity (authentik)

`./run.sh 09` (after `08`) installs [authentik](https://goauthentik.io) with its Helm chart (`AUTHENTIK_VERSION` in
`config.env`, values in `manifests/authentik-values.yaml`) in `agentdev-platform`, using database `authentik` on
the platform Postgres and no Redis. Its configuration is Secret `authentik`, generated from `.secrets/platform/`.
The first admin is **you**: user `akadmin` (email `AUTHENTIK_ADMIN_EMAIL`), password in
`.secrets/platform/authentik-admin`. Open it with
`kubectl -n agentdev-platform port-forward svc/authentik-server 9443:443` and https://localhost:9443/ (self-signed;
session cookies need https). Change the password and add MFA after the first login. The agentdev API and UI
(#46, #48) will be OIDC applications in it; they add a NetworkPolicy rule to reach it (the platform namespace is
deny-by-default). The bootstrap password stays in Secret `authentik` but only applies on first start, so changing
it in the UI is final. On GKE the playbook does not deploy it yet (#51).

## Management API

`./run.sh 10` (after `09`) builds and deploys the agentdev API (`images/api`, Go) in `agentdev-platform` (#46).
Every `/api` call needs an authentik access token of the OIDC application `agentdev` (created by a blueprint in
`09`, public client, authorization code + PKCE); without one it answers 401. Phase 1 is read-only:

| Endpoint | Returns |
|---|---|
| `GET /api/me` | the caller (`sub`, `preferred_username`, `email`) |
| `GET /api/agents` | each agent: `name`, `repos`, `forge`, `phase` (`running`, `starting`, `stopped`, `paused`, `blocked`, `invalid`), replicas, `managed` (an Agent object exists; `false` for a StatefulSet from before the operator), `paused`, `maxConcurrent`, `scaleToZero` (#57), `securityEvents` (count, last 7 days, #70) |
| `GET /api/security-events` | security events (`agentdev.security/v1`), newest first. Filters: `agent`, `severity`, `since` (RFC 3339 or a duration such as `24h`), `limit` (1..500, default 100); `next_cursor` in the answer, pass it as `cursor` for the next page (#70). Every signed-in user sees every event for now; restriction to projects with write access is #57 |

authentik writes the URL you logged in through into the token, so `API_OIDC_ISSUER` (`config.env`) must be that
URL: locally `https://localhost:9443/application/o/agentdev/` with the 9443 port-forward. The API fetches keys
from authentik inside the cluster. It reads agents with a read-only Role on the agents namespace. Reach it with
`kubectl -n agentdev-platform port-forward svc/agentdev-api 8088:8080`. On GKE: #51.

## Agents as resources (operator)

`./run.sh 12` installs the `Agent` CRD (`agentdev.dev/v1alpha1`) and the operator (#47). An agent is then one
object in the agents namespace; the operator creates its Service and StatefulSet (the same ones `06` renders),
keeps them in line with the spec, and reports `status.phase`:

```bash
kubectl -n coding-agents apply -f - <<'EOF'
apiVersion: agentdev.dev/v1alpha1
kind: Agent
metadata: {name: carol}
spec: {repo: owner/playground, claudeSecret: claude-shared}
EOF
kubectl -n coding-agents get agents                       # NAME REPO PHASE
kubectl -n coding-agents patch agent carol --type merge -p '{"spec":{"paused":true}}'   # pause, state kept
kubectl -n coding-agents delete agent carol               # removes the pod; volumes kept
```

The forge token Secret `agent-<name>` must exist first. Blocked agents (`scripts/agent-block.sh`) stay stopped;
with `scaleToZero` the router, not the operator, starts and stops the pod. Field reference:
[`images/operator/README.md`](../images/operator/README.md). `06` creates Agent objects (#57), and the API reads
them (`managed: true`); letting the UI add, pause and remove agents is the next step (#57, #4);
keeping Agents in the deployment repo with Argo CD or Flux is #58.
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
more than 5 events (`SECURITY_PAUSE_AFTER`).

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

NOTE: If the SIEM does not answer with a 2xx status, the API tries again 7 times. The time between the tries
increases from 2 seconds to a maximum of 5 minutes. The event stays in the Postgres table and in the cluster log.

NOTE: A SIEM can also read `GET /api/security-events` with the `cursor` parameter, or read the cluster logs with the
filter `AGENTDEV_SECURITY_EVENT`.

### Reference: settings

| Setting | Default | Where |
|---|---|---|
| `SECURITY_PAUSE_AFTER` | 5 | `config.env`; Ansible `security.pause_after` |
| `SECURITY_RETENTION_DAYS` (SecurityEvent objects) | 30 | `config.env`; Ansible `security.retention_days` |
| `SECURITY_MAX_PER_AGENT` (SecurityEvent objects) | 500 | `config.env`; Ansible `security.max_per_agent` |
| `SECURITY_DB_RETENTION_DAYS` (Postgres table) | 365 | `config.env` |

## GitHub App (registration)

On GitHub the agents act as one GitHub App (#2). Its webhook and redirect URLs belong to the deployment, so the
manifest is a template (`manifests/github-app-manifest.json.tmpl`) filled from the instance file:

```yaml
# instance.yml (deployment repo)
github_app:
  name: my-agentdev                         # unique on GitHub
  owner: my-org                             # omit to own it with your personal account
  public_url: https://agentdev.example.com  # HTTPS address of this deployment's webhook router
```

```bash
scripts/github-app.sh render instance.yml        # review the manifest (permissions, events, URLs)
scripts/github-app.sh form instance.yml          # writes .secrets/github-app/register.html; open it, confirm on github.com
scripts/github-app.sh convert instance.yml 'URL'   # within 1 h, the full redirect URL: App ID, private key, webhook secret
```

The webhook URL is `<public_url>/` (the router verifies `X-Hub-Signature-256`). After you confirm, GitHub redirects
to `<public_url>/github-app/registered?code=...&state=...` (a 404 there is fine: nothing listens yet); pass that whole
URL to `convert`, which checks `state` against the form it generated and stores the results in `.secrets/github-app/`
(mode 600). Then install the App on the
repositories the agents work. Permissions: contents, issues and pull requests write; metadata, checks and statuses
read. It deliberately has no `workflows` permission: agents cannot change `.github/workflows/`. The private key opens every repository the App is installed on: it goes to Secret Manager (GKE) or a local
Secret, never into git or an issue. The worker using the App is the rest of #2.

## Limits and blocking

- **Quota (#27):** `limits:` in the project spec (`pods`, `cpu`, `memory`) becomes a `ResourceQuota` on the agents
  namespace when you run bootstrap. Kubernetes refuses pods beyond it, whatever starts them. All projects share that
  namespace until #17, so the quota caps them together. With a `cpu` or `memory` limit set, Kubernetes rejects any pod
  in the namespace that does not declare CPU requests or memory limits. agentdev's agents, router and interactive
  sessions all declare them; anything else you run there must too.
- **Block an agent:** `scripts/agent-block.sh block <agent> [reason]` stops it and keeps it stopped: events don't
  start it (scale to zero), and redeploys (`./run.sh 06` and the Ansible playbook) keep it at 0. `unblock` starts it
  again and it picks up waiting work.
  `list` shows blocked agents. The UI (#12) will set the same `agentdev/blocked` annotation.

## Interactive sessions

`scripts/agent-chat.sh start <project.yaml>` creates, on first use, the project's chat account `<project>-chat`
(read collaborator, token scoped to `read:repository, write:issue`; kept in `.secrets/<project>-chat.token` and the
Secret `chat-<project>`), then starts a Job from `manifests/chat.yaml.tmpl`.

| Setting | Default | Meaning |
|---|---|---|
| `IDLE_SECONDS` | 1800 | end the session after this long with nobody attached |
| hard cap | 8 h | `activeDeadlineSeconds` in the template, even while attached |
| `CLAUDE_SECRET` | `claude-shared` | Claude credential the session uses (it is visible inside the session pod) |
| `REMOTE_CONTROL` | false | also expose the session to the Claude app (#26). Needs a full claude.ai login: run `/login` inside the session. API keys and `claude setup-token` tokens are refused (inference-only) |
| `IMAGE_PULL_POLICY` | Never | `IfNotPresent` outside kind |

Finished sessions are removed 5 minutes after they end. Running ones: `kubectl -n coding-agents get jobs -l app=agent-chat`.
If botboard runs (`./run.sh 07`, #26), each session shows as a card (agent `chat`) while it is open and turns `done` when
it ends. `CHAT_URL` sets the card's link; it will point at the UI's chat page once #12 exists.

## Webhooks

One webhook per repository points at the router (`06-agents.sh` or bootstrap registers it). The router verifies
the forge signature with `FORGE_SECRETS` and accepts agent registrations with `REGISTER_TOKEN`. Its state:

```bash
kubectl -n coding-agents exec deploy/webhook-router -- node -e \
  "require('http').get('http://localhost:8080/status',r=>r.pipe(process.stdout))"
```

For github.com the router needs a public HTTPS URL (Ingress) and a GitHub App, planned in #2.

## Upgrades

agentdev follows strict SemVer. In the deployment repo, change the module `ref=vX.Y.Z` and `image_tag` together in
one PR, after reading `CHANGELOG.md`. While the version is `0.x`, a MINOR bump may contain breaking changes.

## Removing

- An agent: remove it from the agent list and redeploy, then delete its StatefulSet, volumes and forge account.
- The local setup: `./99-teardown.sh`.
- A GKE instance: `terraform destroy` in the deployment repo.

## Documentation

The docs are in `docs/` and are reviewed in PRs. The GitHub wiki is a published copy, synced from `docs/` on
`main` by a workflow, so the wiki never drifts from the reviewed text.
