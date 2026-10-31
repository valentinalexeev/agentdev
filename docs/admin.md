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

Each agent uses exactly one Claude credential: its own if present, otherwise the shared one (#5).
An agent's memory limit is 3 GiB, which fits 2 parallel items; add about 1 GiB per extra slot in
`manifests/agent.yaml.tmpl`.

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
