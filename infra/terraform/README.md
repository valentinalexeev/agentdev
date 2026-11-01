# agentdev on GCP: Terraform (#11, phase 1)

Reusable Terraform **module** for the infrastructure of one self-hosted agentdev instance. It has no instance
values and no backend: a deployment repository (template: `examples/deployment/gcp-gke`) calls it by release
tag and provides `project_id`, `region`, `zone`, `billing_account_id`, `github_repository` (see `example.tfvars`).

| What | Notes |
|---|---|
| VPC, subnet, Cloud NAT | private nodes; egress narrowed later by #6 |
| GKE Standard, zonal | Dataplane V2 (NetworkPolicy), Workload Identity, regular channel |
| `system` node pool | `e2-standard-2`, 1 to 2 nodes, on-demand: router, operator, UI |
| `agents` node pool | GKE Sandbox (gVisor), Spot `e2-standard-4`, **0** to 3 nodes, taint `agentdev/pool=sandboxes` |
| Artifact Registry | `agentdev` Docker repo, keeps the 20 newest versions |
| Secret Manager | empty containers only (`claude-shared`, `github-app-*`, `router-register-token`); values never pass through Terraform |
| Static IP | public entry point: GKE Gateway of the Ansible role `ingress` (#120) |
| Managed certificate, SSL policy | only with `public_hosts`: Google-managed certificate for those names, TLS 1.2+ `MODERN` (#120) |
| Cloud Armor (optional) | `cloud_armor = true`: deny bodies over `public_max_request_bytes`, `cloud_armor_requests_per_minute` per IP |
| Billing budget | $150/month, alerts at 50/90/100% (alerts only; it does not stop spending) |
| Automation SA | `agentdev-automation`, least-privilege roles; GitHub Actions on `main` reach it through Workload Identity Federation (no key) |

## First run (owner, once)

From the deployment repository (template `examples/deployment/gcp-gke`):

```bash
gcloud auth login && gcloud auth application-default login
gcloud billing accounts list                     # pick the billing account id

# 1. project, billing link, state bucket (local state), from a checkout of the pinned agentdev release.
#    With an existing project (billing already linked) set `create_project = false` in bootstrap.tfvars:
#    then only the bootstrap APIs and the state bucket are created.
cd agentdev/infra/terraform/bootstrap
terraform init
terraform apply -var-file=<deployment repo>/terraform/bootstrap.tfvars     # see bootstrap/example.tfvars

# 2. everything else, in the deployment repo (state in GCS)
cd <deployment repo>/terraform
terraform init -backend-config="bucket=<project_id>-tfstate"
terraform plan  -var-file=instance.tfvars -out plan.bin   # review
terraform apply plan.bin
$(terraform output -raw get_credentials)        # kubectl access
```

Another region: change `region`/`zone` in the deployment's `instance.tfvars`.

## Automation credentials

- **GitHub Actions (preferred, no key):** use `google-github-actions/auth` with the values from `terraform output github_actions_auth`. Only workflows on `main` of the **deployment repository** (`github_repository`) can impersonate the account.
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

GKE zonal control plane: covered by the free tier credit. `system` pool: one `e2-standard-2` running all month. `agents` pool: Spot, billed only while agents run, scales to zero. Plus NAT, a static IP, the registry and secrets. With `public_hosts`: the load balancer forwarding rule, about $18/month; `cloud_armor` adds about $7/month. Expected range for 2 to 3 agents: about $40 to $90/month, plus Claude usage. The budget alerts at $75, $135 and $150.

## Not here

Installing agentdev onto the cluster is phase 2 (Ansible, `infra/ansible/`). For the public HTTPS entry point (#120), set `public_hosts`, point DNS `A` records at `webhook_ip` and copy `terraform output ingress` into `instance.yml` (`docs/admin.md`, "Public HTTPS entry point (GKE)").
