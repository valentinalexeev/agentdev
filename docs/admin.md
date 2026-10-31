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

On GKE, set `botboard_url` in `ansible/instance.yml`. botboard is meant to be reached over your tailnet: run it with
`TS_AUTHKEY` (from Secret Manager) instead of local mode, and use `http://botboard.<tailnet>` as the URL.
`FORGE_PUBLIC_URL` (agent env) makes card links point at the forge's public address instead of the in-cluster one.

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
