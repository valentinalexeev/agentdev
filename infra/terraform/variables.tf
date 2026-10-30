variable "project_id" {
  description = "GCP project id (#11 default: valabs-agentdev)."
  type        = string
  default     = "valabs-agentdev"
}

variable "region" {
  description = "Region for the cluster, registry and network. Change per environment in a .tfvars file."
  type        = string
  default     = "europe-west4"
}

variable "zone" {
  description = "Zone for the zonal GKE cluster (zonal keeps the control plane within the GKE free tier)."
  type        = string
  default     = "europe-west4-a"
}

variable "cluster_name" {
  type    = string
  default = "agentdev"
}

variable "system_machine_type" {
  description = "Node type for the always-on system pool (router, forge connector, operator, management UI)."
  type        = string
  default     = "e2-standard-2"
}

variable "agent_machine_type" {
  description = "Node type for the gVisor agent pool. Each agent pod requests ~0.2 CPU / 512Mi and can burst to 2 CPU / 2Gi."
  type        = string
  default     = "e2-standard-4"
}

variable "agent_pool_max_nodes" {
  description = "Upper bound for the agent pool autoscaler. The pool scales to zero when no agents run."
  type        = number
  default     = 3
}

variable "agent_pool_spot" {
  description = "Use Spot VMs for agent nodes (cheaper, can be preempted; agents resume from their PVC state)."
  type        = bool
  default     = true
}

variable "budget_amount_usd" {
  description = "Monthly billing budget; alerts at 50/90/100%. The budget alerts, it does not stop spending."
  type        = number
  default     = 150
}

variable "billing_account_id" {
  description = "Billing account the budget is attached to (format 012345-6789AB-CDEF01). Needed only for the budget."
  type        = string
}

variable "budget_alert_emails" {
  description = "Extra e-mail recipients for budget alerts (billing admins always get them)."
  type        = list(string)
  default     = []
}

variable "github_repository" {
  description = "owner/repo allowed to use the automation service account via Workload Identity Federation (GitHub Actions)."
  type        = string
  default     = "valentinalexeev/agentdev"
}
