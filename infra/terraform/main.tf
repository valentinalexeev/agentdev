locals {
  apis = concat(
    var.create_artifact_registry ? ["artifactregistry.googleapis.com"] : [],
    local.base_apis,
  )
  base_apis = [
    "compute.googleapis.com",
    "container.googleapis.com",
    "secretmanager.googleapis.com",
    "iam.googleapis.com",
    "iamcredentials.googleapis.com",
    "sts.googleapis.com",
    "dns.googleapis.com",
    "billingbudgets.googleapis.com",
    "logging.googleapis.com",
    "monitoring.googleapis.com",
  ]
}

resource "google_project_service" "apis" {
  for_each           = toset(local.apis)
  service            = each.value
  disable_on_destroy = false
}

data "google_project" "this" {}

# ---------- network ----------
resource "google_compute_network" "vpc" {
  name                    = "agentdev"
  auto_create_subnetworks = false
  depends_on              = [google_project_service.apis]
}

resource "google_compute_subnetwork" "nodes" {
  name                     = "agentdev-nodes"
  network                  = google_compute_network.vpc.id
  region                   = var.region
  ip_cidr_range            = "10.10.0.0/20"
  private_ip_google_access = true
  secondary_ip_range {
    range_name    = "pods"
    ip_cidr_range = "10.20.0.0/16"
  }
  secondary_ip_range {
    range_name    = "services"
    ip_cidr_range = "10.30.0.0/20"
  }
}

# Private nodes need NAT to reach the forge, the Anthropic API and package mirrors.
# Egress per agent/project is narrowed later by #6 (NetworkPolicy + egress proxy).
resource "google_compute_router" "nat" {
  name    = "agentdev-nat"
  network = google_compute_network.vpc.id
  region  = var.region
}

resource "google_compute_router_nat" "nat" {
  name                               = "agentdev-nat"
  router                             = google_compute_router.nat.name
  region                             = var.region
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "ALL_SUBNETWORKS_ALL_IP_RANGES"
  log_config {
    enable = true
    filter = "ERRORS_ONLY"
  }
}

# Public entry point (#120): the GKE Gateway of the Ansible role ingress uses it by name (webhook router for the
# GitHub App, #2, and the web UI with its /api proxy). The name stays for existing instances. Only with public_hosts
# (#169): without them nothing uses the address, and an unused reserved IP is billed.
resource "google_compute_global_address" "webhook" {
  count      = length(var.public_hosts) > 0 ? 1 : 0
  name       = "agentdev-webhook"
  depends_on = [google_project_service.apis]
}

# #169: the address was not counted before. Instances with public_hosts keep the same IP.
moved {
  from = google_compute_global_address.webhook
  to   = google_compute_global_address.webhook[0]
}

# ---------- GKE ----------
resource "google_service_account" "nodes" {
  account_id   = "gke-nodes"
  display_name = "GKE node service account (least privilege)"
}

resource "google_project_iam_member" "nodes" {
  for_each = toset(concat([
    "roles/logging.logWriter",
    "roles/monitoring.metricWriter",
    "roles/monitoring.viewer",
    "roles/stackdriver.resourceMetadata.writer",
    ], var.create_artifact_registry ? ["roles/artifactregistry.reader"] : []
  ))
  project = var.project_id
  role    = each.value
  member  = "serviceAccount:${google_service_account.nodes.email}"
}

locals {
  # null (default) follows public_hosts: the Gateway needs the load balancer, nothing else does
  gateway = coalesce(var.gateway_enabled, length(var.public_hosts) > 0)
}

resource "google_container_cluster" "this" {
  provider            = google-beta
  name                = var.cluster_name
  location            = var.zone # zonal: control plane is covered by the GKE free tier
  network             = google_compute_network.vpc.id
  subnetwork          = google_compute_subnetwork.nodes.id
  deletion_protection = true

  resource_labels = local.labels # #184: also on the node VMs and their boot disks

  # Node pools are managed separately below.
  remove_default_node_pool = true
  initial_node_count       = 1

  release_channel { channel = "REGULAR" }
  networking_mode = "VPC_NATIVE"
  ip_allocation_policy {
    cluster_secondary_range_name  = "pods"
    services_secondary_range_name = "services"
  }
  private_cluster_config {
    enable_private_nodes    = true
    enable_private_endpoint = false
    master_ipv4_cidr_block  = "172.16.0.0/28"
  }
  # Dataplane V2 (Cilium) enforces NetworkPolicy, needed for #6.
  datapath_provider = "ADVANCED_DATAPATH"
  # FQDNNetworkPolicy (#65): the egress proxy itself may reach only the allow-listed hosts
  enable_fqdn_network_policy = true
  workload_identity_config { workload_pool = "${var.project_id}.svc.id.goog" }
  addons_config {
    # The GKE load balancer controller and the Gateway API serve the public HTTPS entry point (#120, ingress.tf);
    # Off without public_hosts (a deployment that publishes its hosts through its own proxy, #160); see gateway_enabled.
    http_load_balancing { disabled = !local.gateway }
    # Filestore CSI: agentdev uses persistent disks only (#208)
    gcp_filestore_csi_driver_config { enabled = var.enable_filestore_csi }
  }
  # Gateway API for the public HTTPS entry point (#120, Ansible role ingress); in-place change on an existing cluster
  gateway_api_config { channel = local.gateway ? "CHANNEL_STANDARD" : "CHANNEL_DISABLED" }
  # Metrics. Cloud Logging stays untouched on purpose: it is the source of truth for security events (#70).
  monitoring_config {
    managed_prometheus { enabled = var.enable_managed_prometheus }
    # null keeps the GKE default component list
    enable_components = var.monitoring_components
  }
  depends_on = [google_project_service.apis]
}

# Always-on pool for platform components (router, operator, UI). Small; on-demand unless system_pool_spot.
resource "google_container_node_pool" "system" {
  name     = "system"
  cluster  = google_container_cluster.this.id
  location = var.zone
  autoscaling {
    min_node_count = var.system_pool_min_nodes # #313: 0 for a stopped instance (scripts/cluster-stop.sh)
    max_node_count = 2
  }
  node_config {
    machine_type    = var.system_machine_type
    disk_size_gb    = var.system_disk_size_gb
    disk_type       = var.system_disk_type
    resource_labels = local.labels         # #184
    spot            = var.system_pool_spot # #171, default false: on-demand
    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]
    workload_metadata_config { mode = "GKE_METADATA" }
    shielded_instance_config {
      enable_secure_boot = true
    }
  }
  management {
    auto_repair  = true
    auto_upgrade = true
  }
}

# Agent pool: GKE Sandbox (gVisor), Spot, scales to zero. Agent pods select it and tolerate its taint.
resource "google_container_node_pool" "agents" {
  provider = google-beta
  name     = "agents"
  cluster  = google_container_cluster.this.id
  location = var.zone
  autoscaling {
    min_node_count = 0
    max_node_count = var.agent_pool_max_nodes
  }
  node_config {
    machine_type    = var.agent_machine_type
    resource_labels = local.labels # #184
    spot            = var.agent_pool_spot
    disk_size_gb    = var.agent_disk_size_gb
    disk_type       = var.agent_disk_type
    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]
    image_type      = "COS_CONTAINERD"
    sandbox_config { sandbox_type = "gvisor" }
    labels = { "agentdev/pool" = "sandboxes" }
    # GKE adds its own sandbox.gke.io/runtime=gvisor taint; this one keeps platform pods off the pool.
    taint {
      key    = "agentdev/pool"
      value  = "sandboxes"
      effect = "NO_SCHEDULE"
    }
    workload_metadata_config { mode = "GKE_METADATA" }
    shielded_instance_config {
      enable_secure_boot = true
    }
  }
  management {
    auto_repair  = true
    auto_upgrade = true
  }
}

# ---------- images and secrets ----------
moved {
  from = google_artifact_registry_repository.images
  to   = google_artifact_registry_repository.images[0]
}

resource "google_artifact_registry_repository" "images" {
  count         = var.create_artifact_registry ? 1 : 0
  location      = var.region
  repository_id = "agentdev"
  format        = "DOCKER"
  description   = "Worker, router and platform images"
  cleanup_policies {
    id     = "keep-recent"
    action = "KEEP"
    most_recent_versions { keep_count = 20 }
  }
  depends_on = [google_project_service.apis]
}

# Secret containers only; values are added by the owner or the management UI (#12), never by Terraform,
# so no secret ever lands in the Terraform state.
resource "google_secret_manager_secret" "platform" {
  for_each  = toset(["claude-shared", "github-app-private-key", "github-app-webhook-secret", "router-register-token"])
  secret_id = each.value
  replication {
    auto {}
  }
  depends_on = [google_project_service.apis]
}

# ---------- budget ----------
# create_budget = false (#199): no budget, no budget channels, no billing IAM.
resource "google_billing_budget" "monthly" {
  count           = var.create_budget ? 1 : 0
  billing_account = var.billing_account_id
  display_name    = "agentdev ${var.project_id} monthly"
  budget_filter {
    projects = ["projects/${data.google_project.this.number}"]
  }
  amount {
    specified_amount {
      currency_code = var.budget_currency
      units         = tostring(var.budget_amount_usd)
    }
  }
  dynamic "threshold_rules" {
    for_each = [0.5, 0.9, 1.0]
    content { threshold_percent = threshold_rules.value }
  }
  all_updates_rule {
    monitoring_notification_channels = [for c in google_monitoring_notification_channel.budget : c.id]
    disable_default_iam_recipients   = false
  }
}

resource "google_monitoring_notification_channel" "budget" {
  for_each     = var.create_budget ? toset(var.budget_alert_emails) : toset([])
  display_name = "budget ${each.value}"
  type         = "email"
  labels       = { email_address = each.value } # channel configuration, not resource labels
  user_labels  = local.labels                   # #184
}

# The automation account plans and applies from CI and must read and update the budget (#199).
# roles/billing.costsManager is the smallest predefined role with billing.budgets.get/update on the billing account.
# The identity of the first apply needs billing.accounts.setIamPolicy (normally the owner); CI cannot grant it.
resource "google_billing_account_iam_member" "automation_budgets" {
  count              = var.create_budget ? 1 : 0
  billing_account_id = var.billing_account_id
  role               = "roles/billing.costsManager"
  member             = "serviceAccount:${google_service_account.automation.email}"
}

# The budget got count in #199; existing state moves without a replace.
moved {
  from = google_billing_budget.monthly
  to   = google_billing_budget.monthly[0]
}
