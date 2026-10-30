# agentdev on GCP: Terraform (#11, phase 1)

Creates the infrastructure for a self-hosted agentdev in project `valabs-agentdev` (defaults; everything is a variable).

| What | Notes |
|---|---|
| VPC, subnet, Cloud NAT | private nodes; egress narrowed later by #6 |
| GKE Standard, zonal | Dataplane V2 (NetworkPolicy), Workload Identity, regular channel |
| `system` node pool | `e2-standard-2`, 1 to 2 nodes, on-demand: router, operator, UI |
| `agents` node pool | GKE Sandbox (gVisor), Spot `e2-standard-4`, **0** to 3 nodes, taint `agentdev/pool=sandboxes` |
| Artifact Registry | `agentdev` Docker repo, keeps the 20 newest versions |
| Secret Manager | empty containers only (`claude-shared`, `github-app-*`, `router-register-token`); values never pass through Terraform |
| Static IP | for the webhook router Ingress (GitHub App webhook, #2) |
| Billing budget | $150/month, alerts at 50/90/100% (alerts only; it does not stop spending) |
| Automation SA | `agentdev-automation`, least-privilege roles; GitHub Actions on `main` reach it through Workload Identity Federation (no key) |

## First run (owner, once)

```bash
gcloud auth login && gcloud auth application-default login
gcloud billing accounts list                     # pick the billing account id

# 1. project, billing link, state bucket (local state)
cd infra/terraform/bootstrap
terraform init
terraform apply -var billing_account_id=XXXXXX-XXXXXX-XXXXXX

# 2. everything else (state in GCS)
cd ..
terraform init -backend-config="bucket=valabs-agentdev-tfstate"
terraform plan  -var billing_account_id=XXXXXX-XXXXXX-XXXXXX -out plan.bin   # review
terraform apply plan.bin
$(terraform output -raw get_credentials)        # kubectl access
```

Another region: `-var region=europe-west1 -var zone=europe-west1-b` (or a `.tfvars` file per environment).

## Automation credentials

- **GitHub Actions (preferred, no key):** use `google-github-actions/auth` with the values from `terraform output github_actions_auth`. Only workflows on `main` of `valentinalexeev/agentdev` can impersonate the account.
- **Local fallback (key file):** only if needed, kept outside the repo:
  ```bash
  gcloud iam service-accounts keys create ~/.config/agentdev/automation-key.json \
    --iam-account "$(terraform output -raw automation_service_account)"
  chmod 600 ~/.config/agentdev/automation-key.json
  export GOOGLE_APPLICATION_CREDENTIALS=~/.config/agentdev/automation-key.json
  ```
  Delete the key when done (`gcloud iam service-accounts keys delete ...`); prefer short-lived impersonation:
  `gcloud config set auth/impersonate_service_account <automation SA>`.

## Cost (rough guide)

GKE zonal control plane: covered by the free tier credit. `system` pool: one `e2-standard-2` running all month. `agents` pool: Spot, billed only while agents run, scales to zero. Plus NAT, a static IP, the registry and secrets. Expected range for 2 to 3 agents: about $40 to $90/month, plus Claude usage. The budget alerts at $75, $135 and $150.

## Not here

Installing agentdev onto the cluster is phase 2 (Ansible, `infra/ansible/`). DNS and TLS for the webhook Ingress come with #2.
