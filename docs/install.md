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

WARNING: Do not put a secret in `kind.vars.yml`. Put the secrets only in `.env`.

1. Clone the repository:
   `git clone https://github.com/valentinalexeev/agentdev && cd agentdev`
2. Write the Claude credential into `.env`:
   `echo 'CLAUDE_CODE_OAUTH_TOKEN=...' > .env`
   You can use `ANTHROPIC_API_KEY=...` in place of `CLAUDE_CODE_OAUTH_TOKEN`.
3. Optional: write a password for the Gitea owner account into `.env`:
   `echo 'FORGE_ADMIN_PASSWORD=...' >> .env`
   If you do not set it, the password is `SpikePassw0rd!`.
4. Copy the example settings to the repository root:
   `cp infra/ansible/kind.example.vars.yml kind.vars.yml`
5. Optional: change the settings in `kind.vars.yml`, for example the agent names.
   Git ignores `kind.vars.yml`.

### Install with the playbook

1. Load the secrets into the shell: `set -a; source .env; set +a`
2. Run the playbook from the repository root:
   `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml`

The playbook does these parts in this sequence:

- `prereqs`: it checks the tools and the size of the VM.
- `cluster`: it makes a `kind` cluster with a sandbox node pool and the gVisor runtime.
- `forge`: it installs Gitea and makes the owner account, the agent accounts and the branch protection.
- `operator`: it installs the Agent operator and its CRDs.
- `agents`: it builds the worker image and the router image. It deploys the router and the agents and registers the
  webhook.

NOTE: To do one part again, add its tag, for example `--tags agents`. Add `-e @kind.vars.yml` to each command.

NOTE: `run.sh` and the numbered step scripts are removed in 1.3.0 (#417). To move an installation that used
them, refer to [Move from run.sh to the playbook](admin.md#move-from-runsh-to-the-playbook-130).

### Make sure that the installation operates

1. Open http://localhost:3000 in a browser.
   The browser shows Gitea.
2. Log in with the user name `owner` and the password from `FORGE_ADMIN_PASSWORD` (default `SpikePassw0rd!`).
   Gitea shows the repository `owner/playground`.
3. Write an issue and assign it to `alice`.
   The agent `alice` opens a pull request.
4. Review the pull request. Refer to the [user guide](user-guide.md).

The installation has two agents (`alice` and `bob`) and the webhook router.

### Do the optional parts

A plain run of the playbook does not do these parts. Do only the parts that you need. Some parts are necessary
before other parts.

| Tag | Do before | Result |
|---|---|---|
| `botboard` | (none). Do the tag `agents` again after it. | [botboard](https://github.com/valentinalexeev/botboard), a dashboard that shows the work of the agents. |
| `mcp` | (none) | The platform namespace with Postgres and ContextForge, for [MCP tools](admin.md#mcp-tools) for each project. |
| `authentik` | `mcp` | The [authentik](https://goauthentik.io) identity provider on the platform Postgres, for the management UI (#12). |
| `api` | `authentik` | The agentdev API (Go) behind authentik OIDC: an overview of the agents, read-only. |
| `ui` | `api` | The agentdev web UI (React): sign in with authentik and see the agents. |
| `secrets` | (none) | The scoped secrets store for [project secrets](admin.md#project-secrets). |

1. Load the secrets into the shell: `set -a; source .env; set +a`
2. Run the playbook with the tag: `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags <tag>`

### Connect a different repository

1. Refer to [Administration → Projects](admin.md#projects).

### Remove the local installation

CAUTION: This procedure deletes the cluster and all data in it.

1. Run `ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags teardown`.
   The tag `teardown` is also tagged `never`: no other run of `kind.yml` deletes the cluster.

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

If you already have a project with billing linked, set `create_project = false` in `bootstrap.tfvars` (and omit `billing_account_id`). The bootstrap then makes only the state bucket and enables its APIs. It does not change the project or billing.

1. In `infra/terraform/bootstrap`, run the bootstrap:
   `terraform apply -var-file=<deployment>/terraform/bootstrap.tfvars`
   The bootstrap makes the project, the billing link and the state bucket.
   With user credentials, set a quota project first: `export GOOGLE_BILLING_PROJECT=<project_id> USER_PROJECT_OVERRIDE=true`

### Deploy the instance

WARNING: Do not put secret values in git, in issues or in logs. Put the secret values only in Secret Manager.

NOTE: The Billing Budgets API needs a quota project. Before plan and apply, set
`GOOGLE_BILLING_PROJECT=<project_id>` and `USER_PROJECT_OVERRIDE=true`. The example `deploy.yml` sets them for CI.

CAUTION: The deployment CI uses the automation account. The first apply gives the automation account a role on the
billing account (#199). The identity of that apply must have `billing.accounts.setIamPolicy` on the billing account.
Usually this is the owner. CI cannot give this role to itself. To make no budget and no billing role, set
`create_budget = false`.

NOTE: You do not apply all of `terraform/` before the first deploy (#471). The owner makes only the CI identity.
The CI makes the cluster and the other resources. The checks that need the cluster run after `terraform apply` in
the CI. When you deleted the cluster to stop the cost, the next deploy makes it again. You do no local step.

1. In `terraform/` of the deployment repository, make only the CI identity (the owner does this step one time):

   ```bash
   terraform init -backend-config="bucket=<project_id>-tfstate"
   terraform apply -var-file=instance.tfvars \
     -target=module.agentdev.google_service_account_iam_member.github_wif \
     -target=module.agentdev.google_project_iam_member.automation \
     -target=module.agentdev.google_billing_account_iam_member.automation_budgets \
     -target=module.agentdev.google_service_account_iam_member.github_pr_wif \
     -target=module.agentdev.google_project_iam_member.plan \
     -target=module.agentdev.google_storage_bucket_iam_member.plan_state \
     -target=module.agentdev.google_billing_account_iam_member.plan_budgets
   ```

   Examine the plan before you type `yes`. It must show no cluster. Terraform warns that `-target` is for
   exceptional use. This warning is expected here.
2. Get the CI values: `terraform output github_actions_auth`
3. Set the CI variables of the deployment repository to these values (refer to `AGENTS.md`, step 6).
4. Put the secret values into Secret Manager. The list is in `infra/ansible/README.md`.
5. Open a pull request in the deployment repository.
   The CI does the checks and, with the plan identity, shows the `terraform plan`.
6. Examine the plan.
7. Merge the pull request.
   The CI makes the infrastructure and deploys the instance.

8. Do the end-to-end security checks. Refer to [e2e-security.md](e2e-security.md).

NOTE: [Private deployment](private-deployment.md) gives the full procedure for GKE, GitHub and Google Workspace.

NOTE: [`AGENTS.md`](../AGENTS.md) gives all the steps in detail. An AI agent can do these steps with you.

NOTE: For the costs and the size of the instance, refer to
[`infra/terraform/README.md`](../infra/terraform/README.md).

## Reference: container images

- Each release publishes `ghcr.io/valentinalexeev/agentdev-worker` and `ghcr.io/valentinalexeev/agentdev-router`.
- A deployment gets the images at the `image_tag` that it sets.
- The local installation builds the images.
