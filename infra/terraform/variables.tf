variable "project_id" {
  description = "GCP project id of this agentdev instance (created by bootstrap/)."
  type        = string
}

variable "region" {
  description = "Region for the cluster, registry and network, e.g. europe-west4."
  type        = string
}

variable "zone" {
  description = "Zone for the zonal GKE cluster (zonal keeps the control plane within the GKE free tier), e.g. europe-west4-a."
  type        = string
}

variable "cluster_name" {
  type    = string
  default = "agentdev"
}

variable "create_artifact_registry" {
  description = "Create the Artifact Registry Docker repo, its API and IAM roles. The Ansible roles pull images from GHCR (image_registry), so false is enough unless you mirror images yourself."
  type        = bool
  default     = true
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
  description = "owner/repo of the DEPLOYMENT repository whose GitHub Actions on main may use the automation service account (Workload Identity Federation)."
  type        = string
}

variable "public_hosts" {
  description = "DNS names of the public HTTPS entry point (#120): the webhook host and the UI host (can be the same). Each needs a DNS A record to webhook_ip. Empty: no certificate, no public entry point."
  type        = list(string)
  default     = []
  validation {
    condition     = alltrue([for h in var.public_hosts : can(regex("^([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\\.)+[a-z]{2,63}$", h))])
    error_message = "public_hosts: lower-case DNS names without scheme, port, path or wildcard."
  }
}

variable "cloud_armor" {
  description = "Attach a Cloud Armor policy (size limit, per-IP rate limit) to the public backends. Costs about $5/month plus $1 per rule."
  type        = bool
  default     = false
}

variable "cloud_armor_requests_per_minute" {
  description = "Cloud Armor: requests per minute per client IP before 429."
  type        = number
  default     = 300
}

variable "public_max_request_bytes" {
  description = "Cloud Armor: largest accepted request body (Content-Length) in bytes. The router accepts at most 5 MB."
  type        = number
  default     = 5000000
}
