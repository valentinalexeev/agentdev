# AGENTS.md: bootstrap an agentdev instance on GCP

Instructions for an AI coding agent (Claude Code, jcode, ...) asked to stand up agentdev on Google Cloud.
Read `CLAUDE.md` first for the working rules (bot identity, issues, PRs, owner approval). This file covers
only the bootstrap. Requested in #11.

## Ground rules for this task

- **Money and irreversible actions belong to the owner.** Never run `terraform apply` for `bootstrap/`
  (it creates a project and links billing) and never change billing yourself. Prepare, explain, and hand
  the exact commands to the owner. You may run `terraform plan` and the Ansible `--check` dry run.
- **Secrets never enter git, issues, PRs or logs.** Values go to Secret Manager; Terraform only creates
  empty secret containers; Ansible tasks touching secrets use `no_log`.
- Everything is a variable: project id, region and zone have defaults (`valabs-agentdev`, `europe-west4`,
  `europe-west4-a`) but must be overridable, never hardcoded in new code.

## Checklist

1. **Tools on the operator machine:** `gcloud`, `terraform` ≥ 1.9, `ansible` ≥ 2.17 with
   `ansible-galaxy collection install -r infra/ansible/requirements.yml`, `kubectl`, `docker`.
2. **Owner logs in** (interactive; ask them): `gcloud auth login && gcloud auth application-default login`.
   Check: `gcloud auth list` shows an active account.
3. **Owner runs the bootstrap** (project + billing + state bucket), see `infra/terraform/README.md`:
   ```bash
   cd infra/terraform/bootstrap && terraform init && terraform apply -var billing_account_id=<id>
   ```
4. **Plan the infrastructure** (you may run plan; the owner applies):
   ```bash
   cd infra/terraform
   terraform init -backend-config="bucket=<project_id>-tfstate"
   terraform plan -var billing_account_id=<id> -out plan.bin
   ```
   Summarize the plan for the owner (resources, rough monthly cost per the README), then the owner runs
   `terraform apply plan.bin`.
5. **Cluster access:** `$(terraform output -raw get_credentials)`; check `kubectl get nodes` (system pool up,
   agent pool at 0 nodes is expected).
6. **Images:** use the published images `ghcr.io/valentinalexeev/agentdev-worker` and `agentdev-router` (#28),
   pinned to a release tag. No local build or registry push is needed.
7. **Secret values** (the owner, or you with the owner's values; never echo them): fill the Secret Manager
   entries listed in `infra/ansible/README.md` (`claude-shared`, `router-register-token`,
   `github-app-webhook-secret`, `agent-<name>-forge-token`).
8. **Install agentdev:**
   ```bash
   cd infra/ansible
   ansible-playbook deploy.yml --check --diff
   ansible-playbook deploy.yml -e image_tag=<tag>
   ```
9. **Verify:**
   - `kubectl -n coding-agents get pods` → `webhook-router` and `agent-<name>-0` Running.
   - The router lists every agent: `kubectl -n coding-agents exec deploy/webhook-router -- node -e "require('http').get('http://localhost:8080/status',r=>r.pipe(process.stdout))"`.
   - Assign a small test issue to an agent and see it comment and open a PR (polling fallback works even
     before the webhook URL is public).
10. **Report** in the bootstrap issue: what was created, the checks and their results, the monthly budget,
    and what's left (GitHub App and webhook DNS/TLS per #2).

## When something fails

- `terraform` 403 or "API not enabled": the main config enables APIs; re-run plan after a minute.
- Agent pod Pending on GKE: check it tolerates `agentdev/pool=sandboxes` and `sandbox.gke.io/runtime=gvisor`
  and that the agent pool autoscaler is allowed to scale (max nodes in `variables.tf`).
- Open an incident issue (CLAUDE.md §5) for anything that broke something that worked before.
