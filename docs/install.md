# Install

Two paths: a **local evaluation** on kind (a laptop, 15 minutes), and **production** on GCP/GKE through a
separate deployment repository.

## Local evaluation (kind)

**Needs:** Docker (or OrbStack/Colima) with 4 CPUs and 6 GiB RAM, `kind`, `kubectl`, `helm`, `jq`, `yq`,
`envsubst`, and a Claude credential.

```bash
git clone https://github.com/valentinalexeev/agentdev && cd agentdev
echo 'CLAUDE_CODE_OAUTH_TOKEN=...' > .env    # or ANTHROPIC_API_KEY=...; gitignored
./run.sh 00 01 05 06                         # prereqs, cluster + gVisor, Gitea, agents
open http://localhost:3000                   # Gitea, login owner / SpikePassw0rd!
```

This gives an in-cluster Gitea with a seeded repo `owner/playground`, two agents (`alice`, `bob`) and the webhook
router. File an issue, assign it to `alice`, and review the PR she opens.

| Step | Does |
|---|---|
| `00` | checks tools and VM size |
| `01` | kind cluster with a sandbox node pool, gVisor runtime |
| `05` | Gitea, owner and agent accounts, branch protection |
| `06` | builds worker and router images, deploys the router and agents, registers the webhook |
| `07` | optional: [botboard](https://github.com/valentinalexeev/botboard), a live dashboard agents report to; run `06` again afterwards |
| `08` | optional: platform namespace with Postgres and ContextForge, for [MCP tools](admin.md#mcp-tools) per project |
| `09` | optional: [authentik](https://goauthentik.io) identity provider on the platform Postgres (after `08`), for the management UI (#12) |
| `10` | optional: agentdev API (Go) behind authentik OIDC: read-only overview of the agents (after `09`) |
| `11` | optional: agentdev web UI (React): sign in with authentik, agent overview (after `10`) |

To connect **another repository** in the local Gitea, see [Administration → Projects](admin.md#projects).
Remove everything with `./99-teardown.sh`.

## Production on GCP / GKE

An instance lives in its own **private deployment repository**, created from
[`examples/deployment/gcp-gke`](../examples/deployment/gcp-gke/). It pins an agentdev **release** (strict SemVer)
and holds every instance value; this repository stays generic.

What you get: a GKE Standard cluster with a Spot gVisor node pool for agents, Secret Manager for credentials,
Workload Identity for the deployment CI (no keys), and a budget alert.

1. Create the deployment repo from the template and fill in `terraform/instance.tfvars` and
   `ansible/instance.yml`.
2. `gcloud auth login && gcloud auth application-default login`.
3. Run the **bootstrap** once (project, billing link, state bucket):
   `infra/terraform/bootstrap`, `terraform apply -var-file=<deployment>/terraform/bootstrap.tfvars`.
4. `terraform plan` / `apply` the instance from the deployment repo.
5. Set the CI variables from `terraform output github_actions_auth`.
6. Put the secret values into Secret Manager (list in `infra/ansible/README.md`).
7. Open a PR in the deployment repo: CI runs the Ansible dry run; merging deploys.

The step-by-step version, written so an AI agent can follow it with you, is [`AGENTS.md`](../AGENTS.md).
Costs and sizing: [`infra/terraform/README.md`](../infra/terraform/README.md).

## Container images

Releases publish `ghcr.io/valentinalexeev/agentdev-worker` and `ghcr.io/valentinalexeev/agentdev-router`.
Deployments pull them at the pinned `image_tag`; the local path builds them.
