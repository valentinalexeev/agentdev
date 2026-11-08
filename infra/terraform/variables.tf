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

variable "labels" {
  description = "Labels on every resource (#184), merged over service = \"agentdev\". Set environment to dev, stage or prod. GCP rules: lower-case keys and values."
  type        = map(string)
  default     = {}
  validation {
    condition     = alltrue([for k, v in var.labels : can(regex("^[a-z][a-z0-9_-]{0,62}$", k)) && can(regex("^[a-z0-9_-]{0,63}$", v))])
    error_message = "labels: keys must match ^[a-z][a-z0-9_-]{0,62}$ and values ^[a-z0-9_-]{0,63}$ (GCP label rules, lower case)."
  }
  validation {
    condition     = !contains(keys(var.labels), "environment") || contains(["dev", "stage", "prod"], lookup(var.labels, "environment", ""))
    error_message = "labels.environment must be dev, stage or prod."
  }
}
variable "system_pool_spot" {
  description = "Use Spot VMs for the system pool (about 60-70% cheaper). When Google reclaims the node, router, operator and proxies are down for about a minute; polling (poll_seconds) picks up missed webhooks. Changing it replaces the pool."
  type        = bool
  default     = false
}

# Cluster add-ons (#208). Lean by default: the system pool is sized for the platform, so add-ons agentdev does not use
# are off. Every add-on costs CPU and memory on the system pool nodes. Set an input back to keep an old cluster as it was.
variable "gateway_enabled" {
  description = "Keep the GKE load balancer controller and the Gateway API channel. null (default): on when public_hosts is set, off when it is empty. true with an empty public_hosts keeps them anyway. false with public_hosts is refused: the Gateway and the managed certificate need the GKE load balancer."
  type        = bool
  default     = null
  validation {
    condition     = var.gateway_enabled != false || length(var.public_hosts) == 0
    error_message = "gateway_enabled = false needs an empty public_hosts: the Gateway and the managed certificate need the GKE load balancer."
  }
}

variable "enable_managed_prometheus" {
  description = "Google Managed Service for Prometheus on the cluster (operator and a collector on every node). agentdev does not use it, so it is off by default."
  type        = bool
  default     = false
}

variable "enable_filestore_csi" {
  description = "Filestore CSI driver add-on (a pod on every node and a controller). agentdev uses persistent disks only, so it is off by default."
  type        = bool
  default     = false
}

variable "monitoring_components" {
  description = "GKE monitoring components. The default [\"SYSTEM_COMPONENTS\"] leaves out POD, DAEMONSET, DEPLOYMENT and STATEFULSET, which run kube-state-metrics. null keeps the GKE default list. Must include SYSTEM_COMPONENTS. Cloud Logging is not an input: it carries the security events (#70)."
  type        = list(string)
  default     = ["SYSTEM_COMPONENTS"]
  validation {
    condition     = var.monitoring_components == null || contains(coalesce(var.monitoring_components, []), "SYSTEM_COMPONENTS")
    error_message = "monitoring_components must include SYSTEM_COMPONENTS (GKE requires it)."
  }
}

variable "create_budget" {
  description = "Create the monthly billing budget, its notification channels and the roles/billing.costsManager grant for the automation account on billing_account_id (#199). The first apply needs billing.accounts.setIamPolicy on the billing account. false = none of these."
  type        = bool
  default     = true
}

variable "budget_amount_usd" {
  description = "Monthly billing budget, in budget_currency (the name is historical); alerts at 50/90/100%. The budget alerts, it does not stop spending."
  type        = number
  default     = 150
}

variable "budget_currency" {
  description = "ISO 4217 currency of the budget. Must equal the billing account currency, else the Budgets API answers 400 with no detail."
  type        = string
  default     = "USD"
  validation {
    condition     = can(regex("^[A-Z]{3}$", var.budget_currency))
    error_message = "budget_currency must be a 3-letter ISO 4217 code such as USD or EUR."
  }
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

# Node boot disks (#170). Size defaults to the GKE default (100 GB); type defaults to pd-standard (owner decision on
# PR #191), so a pool created with the GKE default pd-balanced is replaced once. Changing any of these replaces the
# node pool (nodes are recreated; agents resume from their PVCs).
variable "system_disk_size_gb" {
  description = "Boot disk size (GB) of system pool nodes. They hold only images and ephemeral pod data; 30 is enough. Changing it replaces the pool."
  type        = number
  default     = 100
  validation {
    condition     = var.system_disk_size_gb >= 10
    error_message = "system_disk_size_gb must be at least 10 (GKE minimum)."
  }
}

variable "system_disk_type" {
  description = "Boot disk type of system pool nodes: pd-standard, pd-balanced or pd-ssd. Changing it replaces the pool."
  type        = string
  default     = "pd-standard"
  validation {
    condition     = contains(["pd-standard", "pd-balanced", "pd-ssd"], var.system_disk_type)
    error_message = "system_disk_type must be pd-standard, pd-balanced or pd-ssd."
  }
}

variable "agent_disk_size_gb" {
  description = "Boot disk size (GB) of agent pool nodes (worker image plus gVisor overhead; 50 is enough). Changing it replaces the pool."
  type        = number
  default     = 100
  validation {
    condition     = var.agent_disk_size_gb >= 10
    error_message = "agent_disk_size_gb must be at least 10 (GKE minimum)."
  }
}

variable "agent_disk_type" {
  description = "Boot disk type of agent pool nodes: pd-standard, pd-balanced or pd-ssd. Changing it replaces the pool."
  type        = string
  default     = "pd-standard"
  validation {
    condition     = contains(["pd-standard", "pd-balanced", "pd-ssd"], var.agent_disk_type)
    error_message = "agent_disk_type must be pd-standard, pd-balanced or pd-ssd."
  }
}

variable "secrets_store" {
  description = "Scoped secrets store (#202): Google service accounts for External Secrets Operator (read) and the API (write-only), limited to Secret Manager secrets agentdev-ps-*."
  type        = bool
  default     = true
}

variable "agents_namespace" {
  description = "Kubernetes namespace of the agents (Ansible agents_ns); used for Workload Identity bindings."
  type        = string
  default     = "coding-agents"
}

variable "platform_namespace" {
  description = "Kubernetes namespace of the platform (Ansible platform_ns); used for Workload Identity bindings."
  type        = string
  default     = "agentdev-platform"
}
