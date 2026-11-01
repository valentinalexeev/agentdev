# Install

You can install agentdev in two ways:

- A **local evaluation** on `kind`. This installation is for a laptop and takes approximately 15 minutes.
- A **production** installation on GCP/GKE. This installation uses a separate deployment repository.

## Install agentdev for a local evaluation (kind)

### Prepare the computer

1. Make sure that Docker (or OrbStack or Colima) has 4 CPUs and 6 GiB of RAM.
2. Make sure that these tools are installed: `kind`, `kubectl`, `helm`, `jq`, `yq`, `envsubst` and
   `ansible-playbook` (ansible-core 2.17 or later, for example `pipx install ansible-core`).
3. Make sure that you have a Claude credential.

### Get the source and the credential

WARNING: Do not add the `.env` file to git. The file contains your Claude credential. Git ignores `.env`.

1. Clone the repository:
   `git clone https://github.com/valentinalexeev/agentdev && cd agentdev`
2. Write the Claude credential into `.env`:
   `echo 'CLAUDE_CODE_OAUTH_TOKEN=...' > .env`
   You can use `ANTHROPIC_API_KEY=...` in place of `CLAUDE_CODE_OAUTH_TOKEN`.

### Do the necessary run.sh steps

1. Do run.sh step 00: `./run.sh 00`
   The script checks the tools and the size of the VM.
2. Do run.sh step 01: `./run.sh 01`
   The script makes a `kind` cluster with a sandbox node pool and the gVisor runtime. Step 01 runs the Ansible
   playbook `infra/ansible/kind.yml` (tag `cluster`). You can also run the playbook directly:
   `ansible-playbook infra/ansible/kind.yml --tags cluster`
3. Do run.sh step 05: `./run.sh 05`
   The script installs Gitea and makes the owner account, the agent accounts and the branch protection.
4. Do run.sh step 06: `./run.sh 06`
   The script builds the worker image and the router image. It deploys the router and the agents and registers the
   webhook.

NOTE: You can do the four run.sh steps in one command: `./run.sh 00 01 05 06`.

NOTE: The run.sh steps are deprecated (#106). Each step moves to the Ansible playbook `infra/ansible/kind.yml`, one
step at a time. A step that moved shows a `DEPRECATED` message and runs the playbook. The result is the same.

### Make sure that the installation operates

1. Open http://localhost:3000 in a browser.
   The browser shows Gitea.
2. Log in with the user name `owner` and the password `SpikePassw0rd!`.
   Gitea shows the repository `owner/playground`.
3. Write an issue and assign it to `alice`.
   The agent `alice` opens a pull request.
4. Review the pull request. Refer to the [user guide](user-guide.md).

The installation has two agents (`alice` and `bob`) and the webhook router.

### Do the optional run.sh steps

Do only the run.sh steps that you need. Some run.sh steps are necessary before other run.sh steps.

| run.sh step | Do before | Result |
|---|---|---|
| `07` | (none). Do `./run.sh 06` again after it. | [botboard](https://github.com/valentinalexeev/botboard), a dashboard that shows the work of the agents. |
| `08` | (none) | The platform namespace with Postgres and ContextForge, for [MCP tools](admin.md#mcp-tools) for each project. |
| `09` | `08` | The [authentik](https://goauthentik.io) identity provider on the platform Postgres, for the management UI (#12). |
| `10` | `09` | The agentdev API (Go) behind authentik OIDC: an overview of the agents, read-only. |
| `11` | `10` | The agentdev web UI (React): sign in with authentik and see the agents. |
| `12` | `06` and `08` | The Agent operator: Agent objects (`kubectl get agents`) add and remove agents. |

1. Do the run.sh step: `./run.sh <number>`

### Connect a different repository

1. Refer to [Administration → Projects](admin.md#projects).

### Remove the local installation

CAUTION: This procedure deletes the cluster and all data in it.

1. Run `./99-teardown.sh`.

## Install agentdev for production (GCP/GKE)

An instance has its own **private deployment repository**. You make it from the template
[`examples/deployment/gcp-gke`](../examples/deployment/gcp-gke/). The deployment repository sets an agentdev
**release** (strict SemVer) and contains all the values of the instance. This repository stays generic.

The installation gives:

- A GKE Standard cluster with a Spot gVisor node pool for the agents.
- Secret Manager for the credentials.
- Workload Identity for the deployment CI (no keys).
- A budget alert.

### Prepare the deployment repository

1. Make the deployment repository from the template.
2. Write the values in `terraform/instance.tfvars` and `ansible/instance.yml`.
3. Log in to Google Cloud:
   `gcloud auth login && gcloud auth application-default login`

### Run the bootstrap

CAUTION: The bootstrap makes a project and connects billing. Run the bootstrap only one time.

1. In `infra/terraform/bootstrap`, run the bootstrap:
   `terraform apply -var-file=<deployment>/terraform/bootstrap.tfvars`
   The bootstrap makes the project, the billing link and the state bucket.

### Deploy the instance

WARNING: Do not put secret values in git, in issues or in logs. Put the secret values only in Secret Manager.

1. In the deployment repository, run `terraform plan`.
2. Examine the plan.
3. Run `terraform apply`.
4. Get the CI values: `terraform output github_actions_auth`
5. Set the CI variables of the deployment repository to these values.
6. Put the secret values into Secret Manager. The list is in `infra/ansible/README.md`.
7. Open a pull request in the deployment repository.
   The CI does the Ansible dry run.
8. Merge the pull request.
   The CI deploys the instance.

9. Do the end-to-end security checks. Refer to [e2e-security.md](e2e-security.md).

NOTE: [Private deployment](private-deployment.md) gives the full procedure for GKE, GitHub and Google Workspace.

NOTE: [`AGENTS.md`](../AGENTS.md) gives all the steps in detail. An AI agent can do these steps with you.

NOTE: For the costs and the size of the instance, refer to
[`infra/terraform/README.md`](../infra/terraform/README.md).

## Reference: container images

- Each release publishes `ghcr.io/valentinalexeev/agentdev-worker` and `ghcr.io/valentinalexeev/agentdev-router`.
- A deployment gets the images at the `image_tag` that it sets.
- The local installation builds the images.
