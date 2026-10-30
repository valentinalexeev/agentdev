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
output "github_actions_auth" { value = module.agentdev.github_actions_auth }
