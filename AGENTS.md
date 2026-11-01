# AGENTS.md: bootstrap an agentdev instance on GCP

Instructions for an AI coding agent (Claude Code, jcode, ...) asked to stand up agentdev on Google Cloud.
Read `CLAUDE.md` first for the working rules (bot identity, issues, PRs, owner approval). Requested in #11.

**Start from the template, don't hand-roll.** An instance lives in its own **deployment repository**,
created from `examples/deployment/gcp-gke/`. That repository pins a **release** of this one (strict SemVer,
`CLAUDE.md` §7) and holds every instance value. This repository stays generic.

## Ground rules for this task

- **Money and irreversible actions belong to the owner.** Never run `terraform apply` for `bootstrap/`
  (it creates a project and links billing) and never change billing yourself. Prepare, explain, and hand
  the exact commands to the owner. You may run `terraform plan` and the Ansible `--check` dry run.
- **Secrets never enter git, issues, PRs or logs.** Values go to Secret Manager; Terraform only creates
  empty secret containers; Ansible tasks touching secrets use `no_log`.
- **No instance values in this repository.** Project ids, regions, people and agent lists go into the
  deployment repo (`terraform/instance.tfvars`, `ansible/instance.yml`).

## Checklist

1. **Deployment repository:** the owner creates a new private repo; copy `examples/deployment/gcp-gke/`
   into it, rename the `*.example` files and fill them in. Pin `ref=` and `image_tag` to the same agentdev
   release.
2. **Tools on the operator machine:** `gcloud`, `terraform` ≥ 1.9, `ansible` ≥ 2.17, `kubectl`.
3. **Owner logs in** (interactive; ask them): `gcloud auth login && gcloud auth application-default login`.
   Check: `gcloud auth list` shows an active account.
4. **Owner runs the bootstrap** (project + billing + state bucket) from a checkout of the pinned agentdev release:
   ```bash
   cd agentdev/infra/terraform/bootstrap && terraform init
   terraform apply -var-file=<deployment repo>/terraform/bootstrap.tfvars    # see bootstrap/example.tfvars
   ```
5. **Plan the infrastructure** in the deployment repo (you may run plan; the owner applies the first time):
   ```bash
   cd terraform
   terraform init -backend-config="bucket=<project_id>-tfstate"
   terraform plan -var-file=instance.tfvars -out plan.bin
   ```
   Summarize the plan for the owner (resources, rough monthly cost per `infra/terraform/README.md`), then the
   owner runs `terraform apply plan.bin`. This also creates the Workload Identity setup the deployment CI uses.
6. **Wire the deployment CI:** set repository variables `GCP_WIF_PROVIDER` and `GCP_SERVICE_ACCOUNT`
   (from `terraform output github_actions_auth`) and `GCP_PROJECT`; ask the owner to protect `main`.
7. **Images:** nothing to build. Deployments pull `ghcr.io/valentinalexeev/agentdev-worker` and
   `agentdev-router` at `image_tag` (#28).
8. **Secret values** (the owner, or you with the owner's values; never echo them): fill the Secret Manager
   entries listed in `infra/ansible/README.md` (`claude-shared`, `router-register-token`,
   `github-app-webhook-secret`, `agent-<name>-forge-token`).
9. **Install agentdev:** open a PR in the deployment repo; its CI runs `ansible-playbook --check`, and
   after the owner approves and merges, the playbook runs for real. Manually, from the deployment repo:
   ```bash
   ansible-playbook <agentdev checkout>/infra/ansible/deploy.yml -e @ansible/instance.yml --check --diff
   ```
10. **Verify:**
    - `kubectl -n coding-agents get pods` → `webhook-router` and `agent-<name>-0` Running.
    - The router lists every agent: `kubectl -n coding-agents exec deploy/webhook-router -- node -e "require('http').get('http://localhost:8080/status',r=>r.pipe(process.stdout))"`.
    - Assign a small test issue to an agent and see it comment and open a PR (polling works even before the
      webhook URL is public).
    - Security: `scripts/e2e-gke-security.sh --host <ui host> --webhook-host <webhook host>` (the owner runs it;
      `docs/e2e-security.md`). Every line PASS, no FAIL.
11. **Report** in the deployment repo's bootstrap issue: what was created, the checks and their results, the
    monthly budget, and what's left (GitHub App and webhook DNS/TLS per #2).

## When something fails

- `terraform` 403 or "API not enabled": the module enables APIs; re-run plan after a minute.
- Agent pod Pending on GKE: check it tolerates `agentdev/pool=sandboxes` and `sandbox.gke.io/runtime=gvisor`
  and that the agent pool autoscaler is allowed to scale (`agent_pool_max_nodes`).
- A product bug (module, playbook, image): open an issue in agentdev, not a local patch in the deployment repo.
  An outage of the instance: an incident issue in the deployment repo.
