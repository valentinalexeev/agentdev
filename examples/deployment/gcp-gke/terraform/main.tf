# Root module of an agentdev DEPLOYMENT repository (copy of examples/deployment/gcp-gke).
# It pins a released version of the agentdev module; upgrade by changing ref= (strict SemVer: read the
# CHANGELOG of agentdev for MAJOR bumps before upgrading).
terraform {
  required_version = ">= 1.9"
  backend "gcs" {
    # bucket comes from bootstrap: terraform init -backend-config="bucket=<project_id>-tfstate"
    prefix = "agentdev"
  }
}

module "agentdev" {
  source = "github.com/valentinalexeev/agentdev//infra/terraform?ref=v1.0.0"

  project_id         = var.project_id
  region             = var.region
  zone               = var.zone
  billing_account_id = var.billing_account_id
  github_repository  = var.github_repository

  budget_amount_usd    = var.budget_amount_usd
  budget_currency      = var.budget_currency
  budget_alert_emails  = var.budget_alert_emails
  agent_pool_max_nodes = var.agent_pool_max_nodes

  # public HTTPS entry point (agentdev #120)
  public_hosts = var.public_hosts
  cloud_armor  = var.cloud_armor

  # no Artifact Registry (agentdev #157): uncomment with a ref= that contains #157
  # create_artifact_registry = var.create_artifact_registry

  # node boot disks (agentdev #170): uncomment with a ref= that contains #170; changing one replaces the pool
  # system_disk_size_gb = var.system_disk_size_gb
  # system_disk_type    = var.system_disk_type
  # agent_disk_size_gb  = var.agent_disk_size_gb
  # agent_disk_type     = var.agent_disk_type
  # labels on all resources (agentdev #184): uncomment with a ref= that contains #184
  # labels = var.labels
  # Spot system pool (agentdev #171): uncomment with a ref= that contains #171
  # system_pool_spot = var.system_pool_spot
  # cluster add-ons (agentdev #208): lean by default; uncomment with a ref= that contains #208
  # gateway_enabled           = var.gateway_enabled
  # enable_managed_prometheus = var.enable_managed_prometheus
  # enable_filestore_csi      = var.enable_filestore_csi
  # monitoring_components     = var.monitoring_components
  # budget toggle and billing grant for CI (agentdev #199): uncomment with a ref= that contains #199
  # create_budget = var.create_budget
}

# Migration (agentdev #199): if this repository has the workaround resource
# google_billing_account_iam_member.ci_budgets, delete that resource block and keep this block for one apply.
# Terraform forgets the old address without a destroy; the module then creates the same grant at its own address.
# removed {
#   from = google_billing_account_iam_member.ci_budgets
#   lifecycle { destroy = false }
# }

variable "project_id" { type = string }
variable "region" { type = string }
variable "zone" { type = string }
variable "billing_account_id" { type = string }
variable "github_repository" {
  description = "This deployment repository (owner/name); its main-branch workflows get the apply identity, its pull_request runs the read-only plan identity (agentdev #291)."
  type        = string
}
variable "budget_amount_usd" {
  type    = number
  default = 150
}
variable "budget_currency" {
  description = "Currency of the billing account (USD, EUR, ...); budget_amount_usd is in this currency."
  type        = string
  default     = "USD"
}
variable "budget_alert_emails" {
  type    = list(string)
  default = []
}
variable "create_budget" {
  description = "false = no billing budget and no billing IAM for the automation account (agentdev #199)."
  type        = bool
  default     = true
}
variable "agent_pool_max_nodes" {
  type    = number
  default = 3
}

output "get_credentials" { value = module.agentdev.get_credentials }
output "webhook_ip" { value = module.agentdev.webhook_ip }
output "ingress" { value = module.agentdev.ingress } # agentdev #120: values for instance.yml `ingress`
output "github_actions_auth" { value = module.agentdev.github_actions_auth }

variable "public_hosts" {
  description = "DNS names of the public HTTPS entry point (web UI and webhook); empty = none."
  type        = list(string)
  default     = []
}
variable "cloud_armor" {
  type    = bool
  default = false
}
variable "create_artifact_registry" {
  description = "false = no Artifact Registry repo, API or roles; images come from GHCR (agentdev #157)."
  type        = bool
  default     = true
}
variable "system_disk_size_gb" {
  type    = number
  default = 100
}
variable "system_disk_type" {
  type    = string
  default = "pd-standard"
}
variable "agent_disk_size_gb" {
  type    = number
  default = 100
}
variable "agent_disk_type" {
  type    = string
  default = "pd-standard"
}
variable "labels" {
  description = "Labels on all resources (agentdev #184); service = \"agentdev\" is added. environment: dev, stage or prod."
  type        = map(string)
  default     = { environment = "prod" }
}
variable "system_pool_spot" {
  description = "Spot VMs for the system pool (agentdev #171); platform down ~1 minute when a node is reclaimed."
  type        = bool
  default     = false
}
variable "gateway_enabled" {
  description = "GKE load balancer and Gateway API (agentdev #208). null: on when public_hosts is set. false with public_hosts is refused."
  type        = bool
  default     = null
}
variable "enable_managed_prometheus" {
  description = "Google Managed Prometheus (agentdev #208); agentdev does not use it."
  type        = bool
  default     = false
}
variable "enable_filestore_csi" {
  description = "Filestore CSI driver (agentdev #208); agentdev uses persistent disks only."
  type        = bool
  default     = false
}
variable "monitoring_components" {
  description = "GKE monitoring components (agentdev #208); null keeps the GKE default list."
  type        = list(string)
  default     = ["SYSTEM_COMPONENTS"]
}
# Deterministic executors (#69), optional. Two Google service accounts per executor: read-only for the plan of every
# PR, write (only this environment) for the apply of an approved plan. Workload Identity lets only the executor's
# Kubernetes service account in agentdev-executors use each. Grant the roles your tool needs; keep plan read-only.
#
# locals { executors = ["tf-prod"] }
# resource "google_service_account" "exec" {
#   for_each   = { for p in setproduct(local.executors, ["plan", "apply"]) : "${p[0]}-${p[1]}" => p }
#   account_id = "exec-${each.key}"
# }
# resource "google_service_account_iam_member" "exec_wi" {
#   for_each           = google_service_account.exec
#   service_account_id = each.value.name
#   role               = "roles/iam.workloadIdentityUser"
#   member             = "serviceAccount:${var.project_id}.svc.id.goog[agentdev-executors/exec-${each.key}]"
# }
# resource "google_project_iam_member" "exec_plan_viewer" {
#   for_each = toset(local.executors)
#   project  = var.project_id
#   role     = "roles/viewer"
#   member   = "serviceAccount:${google_service_account.exec["${each.key}-plan"].email}"
# }
