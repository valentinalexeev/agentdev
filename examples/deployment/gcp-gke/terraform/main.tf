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
  source = "github.com/valentinalexeev/agentdev//infra/terraform?ref=v0.1.0"

  project_id         = var.project_id
  region             = var.region
  zone               = var.zone
  billing_account_id = var.billing_account_id
  github_repository  = var.github_repository

  budget_amount_usd    = var.budget_amount_usd
  budget_alert_emails  = var.budget_alert_emails
  agent_pool_max_nodes = var.agent_pool_max_nodes

  # public HTTPS entry point (agentdev #120): uncomment with a ref= that contains #120
  # public_hosts = var.public_hosts
  # cloud_armor  = var.cloud_armor
}

variable "project_id" { type = string }
variable "region" { type = string }
variable "zone" { type = string }
variable "billing_account_id" { type = string }
variable "github_repository" {
  description = "This deployment repository (owner/name); its main-branch workflows get Workload Identity access."
  type        = string
}
variable "budget_amount_usd" {
  type    = number
  default = 150
}
variable "budget_alert_emails" {
  type    = list(string)
  default = []
}
variable "agent_pool_max_nodes" {
  type    = number
  default = 3
}

output "get_credentials" { value = module.agentdev.get_credentials }
output "webhook_ip" { value = module.agentdev.webhook_ip }
# output "ingress" { value = module.agentdev.ingress }   # agentdev #120: values for instance.yml `ingress`
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
