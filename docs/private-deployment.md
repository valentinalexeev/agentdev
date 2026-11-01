# Private deployment (GKE + GitHub + Google Workspace)

This guide is for admins. It tells you how to deploy one agentdev instance from a private deployment repository.
The instance runs on GKE, works on GitHub repositories, and signs people in through Google Workspace. The short
steps are in [Install](install.md#install-agentdev-for-production-gcpgke). An AI agent can do the steps with you
(`AGENTS.md`).

NOTE: This guide is for GKE. The local evaluation on kind
([Install](install.md#install-agentdev-for-a-local-evaluation-kind)) is different in these ways:

| Item | kind | GKE (this guide) |
|---|---|---|
| Instance values | `config.env` | `ansible/instance.yml` and `terraform/instance.tfvars` in the deployment repository |
| Secret values | files in `.secrets/` | Secret Manager |
| Install and upgrade | `./run.sh` | the playbook `deploy.yml`, from the deployment CI |
| Sandbox pool | one node with gVisor | a GKE Sandbox (gVisor) node pool with an autoscaler |
| GitHub events | polling (no public URL) | webhook on `<public_url>/hook`, and polling |
| Google Workspace sign-in | not available | the authentik Workspace source |

The deployment repository contains all the values of the instance. agentdev itself stays generic. The deployment
repository uses one agentdev release: the Terraform `ref=vX.Y.Z` and the Ansible `image_tag: "X.Y.Z"`.

## Prepare

### Get the tools and the access

1. Install these tools on the operator computer: `gcloud`, `terraform` 1.9 or later, `ansible` 2.17 or later,
   `kubectl`, `gh`.
2. Get these accesses:
   - A Google Cloud billing account that you can link to a new project.
   - A DNS zone where you can add `A` records.
   - Owner rights on the GitHub organization of the agent repositories.
   - Admin rights in Google Workspace (to make the OAuth client and the admin group).
   - A Claude credential (`CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY`).
3. Log in to Google Cloud:
   `gcloud auth login && gcloud auth application-default login`
4. Make sure that the account is active: `gcloud auth list`

### Make the deployment repository

1. Make a new **private** repository.
2. Copy `examples/deployment/gcp-gke/` of the agentdev release into the repository.
3. Remove `.example` from the file names `terraform/instance.tfvars.example` and `ansible/instance.yml.example`.
4. Write your values into the two files: project, region, zone, billing account, repository name, reviewer, agents.
5. Set the same release in two places: `ref=vX.Y.Z` in `terraform/main.tf` and `image_tag: "X.Y.Z"` in
   `ansible/instance.yml`. The image tag has no `v`.

WARNING: Do not put secret values in the deployment repository, in issues or in logs. Put them only in Secret Manager.

## Make the GCP project and the DNS names

### Run the bootstrap

CAUTION: The bootstrap makes a project and links billing. The owner of the billing account runs it, one time only.

1. Go to `infra/terraform/bootstrap` in a checkout of the agentdev release.
2. Run `terraform init`.
3. Run `terraform apply -var-file=<deployment repo>/terraform/bootstrap.tfvars`
   The bootstrap makes the project, the billing link and the state bucket `<project_id>-tfstate`.

### Apply the infrastructure

1. In `terraform/instance.tfvars`, set the public DNS names:

   ```hcl
   public_hosts = ["agentdev.example.com"]
   ```

2. Go to `terraform/` in the deployment repository.
3. Run `terraform init -backend-config="bucket=<project_id>-tfstate"`
4. Run `terraform plan -var-file=instance.tfvars -out plan.bin`
5. Examine the plan. For the costs, refer to `infra/terraform/README.md`.
6. Run `terraform apply plan.bin` (the owner does this step the first time).
7. Show the values: `terraform output ingress`, `terraform output webhook_ip` and
   `terraform output github_actions_auth`.
8. In your DNS zone, make an `A` record for each name in `public_hosts`. Use the address from `webhook_ip`.
9. Copy the `ingress` values into `ansible/instance.yml`. Refer to
   [Administration, "Make the public entry point"](admin.md#make-the-public-entry-point).

### Connect the deployment CI

1. In the deployment repository, set the repository variables `GCP_WIF_PROVIDER` and `GCP_SERVICE_ACCOUNT` from
   `github_actions_auth`, and `GCP_PROJECT`.
2. Protect `main`: require a pull request, 1 approval and the `deploy` checks.

From now on, each change to the instance is a pull request in the deployment repository.

## Register the GitHub App

On GitHub, the agents use one GitHub App, not a bot account with a personal token. The forge sidecar of each agent
makes installation tokens for 1 h.

WARNING: Do not put the private key in git or in an issue. The private key opens each repository that has the App.

1. Set `github_app.name`, `github_app.owner` and `github_app.public_url` in `ansible/instance.yml`.
   `public_url` is `https://<ingress.webhook_host>`. You set `github_app.app_id` in step 4.
2. Do the procedure in [Administration, "Register the GitHub App"](admin.md#register-the-github-app).
   The webhook URL is `<public_url>/hook`.
3. Install the App on the repositories of the agents.
4. Set `github_app.app_id` in `ansible/instance.yml` to the value in `.secrets/github-app/app-id`. This value is
   necessary and is not secret.
5. Make sure that each agent name has only lowercase letters, digits and hyphens.
6. Make the agent labels. Refer to [Administration, "Make the agent labels"](admin.md#make-the-agent-labels).

NOTE: Do not run the playbook now.
[Administration, "Connect the agents to the GitHub App"](admin.md#connect-the-agents-to-the-github-app) puts the two
GitHub secrets in Secret Manager and runs the playbook. In this guide, the next section puts the secrets in Secret
Manager, and "Install agentdev" runs the playbook.

NOTE: The App has no `workflows` permission. Thus, agents cannot change `.github/workflows/`.

## Put the secret values in Secret Manager

Terraform makes only empty secret containers. You add the values. Ansible reads them and never writes them.

WARNING: Do not show a secret value on the screen or in a shell history. Read it from a file or from standard input.

1. For each secret in this table, add a version:
   `gcloud secrets versions add <secret> --data-file=<file> --project=<project_id>`

   | Secret | Value |
   |---|---|
   | `claude-shared` | `CLAUDE_CODE_OAUTH_TOKEN=...` or `ANTHROPIC_API_KEY=...` |
   | `router-register-token` | a random string (step 2) |
   | `github-app-webhook-secret` | the webhook secret from `.secrets/github-app/` |
   | `github-app-private-key` | the private key from `.secrets/github-app/private-key.pem` |
   | `agent-<name>-claude` | optional: the Claude credential of one agent |
   | `security-siem` | optional: the SIEM output secrets |

2. Make the router token and put it in Secret Manager in one command. `--data-file=-` reads the value from
   standard input, thus the value is not shown:
   `openssl rand -hex 32 | gcloud secrets versions add router-register-token --data-file=- --project=<project_id>`
3. Delete the local files in `.secrets/github-app/` after you add them, or keep them in a password manager.

NOTE: The full list, with the secrets that the playbook makes on the first run, is in `infra/ansible/README.md`.
`agent-<name>-forge-token` is only for Gitea. On GitHub, the sidecar uses the App.

## Set up Google Workspace sign-in

The API accepts tokens from authentik. authentik signs people in through Google Workspace and puts the groups and the
`forge_login` claim into the token. The Workspace source and the group sync are in PR #134 (#122). Until that PR is
merged and released, use the procedure in that PR.

1. In the Google Cloud console of the instance project, make an OAuth client of type **Web application**. Use the
   redirect URL that PR #134 gives for the authentik Workspace source.
2. Put the client secret in Secret Manager, with the name that PR #134 gives.
3. In Google Workspace, make the group for agentdev admins (for example `agentdev-admins@example.com`).
4. Set the SSO values in `ansible/instance.yml` as PR #134 gives, and set `api.oidc_issuer` to the authentik issuer:

   ```yaml
   api:
     oidc_issuer: https://auth.example.com/application/o/agentdev/
   ```

5. Make sure that `forge_login` is the **GitHub login** of the person, from a stable and unique attribute. Refer to
   [Administration, "Map people to forge users"](admin.md#map-people-to-forge-users).

WARNING: Do not let users edit `forge_login`. A person with a wrong `forge_login` can act as that GitHub user.

## Install agentdev

1. Open a pull request in the deployment repository with the values of the steps before.
   The CI runs the dry run:
   `ansible-playbook <agentdev checkout>/infra/ansible/deploy.yml -e @ansible/instance.yml --check --diff`
2. Examine the diff.
3. Merge the pull request.
   The CI runs the playbook.

NOTE: On the first dry run, the platform secrets do not exist yet. Thus, the diff shows empty passwords. This is correct.

## Make the first admin

agentdev has two types of admin. The authentik administrator (`akadmin`) controls authentik. The members of
`agentdev-admins` control the agents. No automated process changes the members of `agentdev-admins`.

WARNING: Do not remove the last way to log in as an administrator. Keep `akadmin` with a strong password and MFA,
and keep it as the break-glass account. Do not delete it or change it to a Workspace-only login.

1. Log in to authentik as `akadmin`. Refer to
   [Administration, "Log in to authentik for the first time"](admin.md#log-in-to-authentik-for-the-first-time).
2. Change the password and add MFA.
3. Keep the new password and the MFA recovery codes in the password manager of the owner.
4. Log in to the agentdev UI one time with your Workspace account.
   authentik makes your user.
5. Make sure that you are a member of `agentdev-admins`: by the Workspace group sync (PR #134), or by hand. Refer to
   [Administration, "Give a person admin rights for agents"](admin.md#give-a-person-admin-rights-for-agents).
6. Log out of the UI and log in again.
   The new token has the group.
7. Make sure that you can pause an agent in the UI. Then resume it.
8. Only after step 7 succeeds, change other settings of the Workspace source.

CAUTION: If the Workspace source fails, log in to authentik as `akadmin` and correct the source. Do not change the
membership of `agentdev-admins` by a script.

## Make sure that the instance operates

1. Get the cluster credentials: `$(terraform -chdir=terraform output -raw get_credentials)`
2. Make sure that the pods run: `kubectl -n coding-agents get pods`
   `webhook-router` and each `agent-<name>-0` show `2/2 Running`.
3. Make sure that the API is ready: `kubectl -n agentdev-platform rollout status deploy/agentdev-api`
4. Make sure that the router knows all agents:
   `kubectl -n coding-agents exec deploy/webhook-router -- node -e "require('http').get('http://localhost:8080/status',r=>r.pipe(process.stdout))"`
5. Make sure that the UI opens: `curl -sI https://agentdev.example.com/`
   The response is `200` and has the header `strict-transport-security`.
6. Make sure that an internal path is not public:
   `curl -s -o /dev/null -w '%{http_code}\n' https://agentdev.example.com/status`
   The response is `404`.
7. Assign a small test issue to an agent on GitHub.
   The agent writes a comment and opens a pull request.
8. Write a review comment on the pull request.
   The agent pushes a follow-up commit.
9. Do the security checklist of #123 on the instance.

## Make a backup

The state of the instance is in four places. Make a backup of each.

| Data | Where | Backup |
|---|---|---|
| Infrastructure state | bucket `<project_id>-tfstate` | object versioning is on (bootstrap) |
| Instance values | the deployment repository | git |
| Secret values | Secret Manager | old versions stay until you destroy them |
| Agents, audit, security events, authentik | Postgres `platform-db` in `agentdev-platform` | `pg_dumpall` (below) |

CAUTION: The dump contains the authentik data and the audit records. Keep it with the same protection as the secrets.

1. Make a dump of the platform database:
   `kubectl -n agentdev-platform exec platform-db-0 -- pg_dumpall -U postgres > platform-$(date +%F).sql`
2. Put the file in a private bucket with a retention policy.
3. Delete the local file.

NOTE: To restore, apply the dump to an empty `platform-db` with `psql -U postgres`, then run the playbook again.

## Upgrade the instance

agentdev uses strict SemVer. While the version is `0.x`, a MINOR version can have changes that are not compatible.
After `1.0.0`, only a MAJOR version can have changes that are not compatible.

1. Read `CHANGELOG.md` from your version to the new version. Find all **BREAKING** entries.
2. Make a backup (see "Make a backup").
3. In one pull request, change `ref=vX.Y.Z` in `terraform/main.tf` and `image_tag: "X.Y.Z"` in `ansible/instance.yml`.
4. Add the input changes of the **BREAKING** entries to the same pull request.
5. Examine the Terraform plan and the Ansible dry run in the CI.
6. Merge the pull request.
7. Do the steps of "Make sure that the instance operates".
8. After an authentik upgrade, make sure that no prompt writes `attributes.forge_login`
   (**Flows and Stages > Prompts**, search `forge_login`).

NOTE: Do not deploy from a branch. Use only release tags.

## Security checklist

Do these checks before you give the instance to users, and after each upgrade. The full e2e checklist is in #123.

- [ ] The deployment repository is private, and `main` is protected.
- [ ] No secret value is in git, in an issue, in a pull request or in a CI log.
- [ ] `akadmin` has a strong password and MFA. The recovery codes are with the owner.
- [ ] `agentdev-admins` contains only platform operators.
- [ ] No prompt or flow writes `attributes.forge_login`. authentik has no enrollment flow.
- [ ] The GitHub App is installed only on the agent repositories and has no `workflows` permission.
- [ ] The branch rules of the agent repositories require a review from a person. Agents never merge.
- [ ] Only `/hook` and the UI are public. `/status` gives `404`.
- [ ] Approvals are on (`approvals.enabled`), and the security events go to your SIEM.
- [ ] The budget alert sends email to the owner.
