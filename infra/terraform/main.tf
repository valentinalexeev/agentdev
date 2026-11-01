locals {
  apis = [
    "compute.googleapis.com",
    "container.googleapis.com",
    "artifactregistry.googleapis.com",
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
# GitHub App, #2, and the web UI with its /api proxy). The name stays for existing instances.
resource "google_compute_global_address" "webhook" {
  name       = "agentdev-webhook"
  depends_on = [google_project_service.apis]
}

# ---------- GKE ----------
resource "google_service_account" "nodes" {
  account_id   = "gke-nodes"
  display_name = "GKE node service account (least privilege)"
}

resource "google_project_iam_member" "nodes" {
  for_each = toset([
    "roles/logging.logWriter",
    "roles/monitoring.metricWriter",
    "roles/monitoring.viewer",
    "roles/stackdriver.resourceMetadata.writer",
    "roles/artifactregistry.reader",
  ])
  project = var.project_id
  role    = each.value
  member  = "serviceAccount:${google_service_account.nodes.email}"
}

resource "google_container_cluster" "this" {
  provider            = google-beta
  name                = var.cluster_name
  location            = var.zone # zonal: control plane is covered by the GKE free tier
  network             = google_compute_network.vpc.id
  subnetwork          = google_compute_subnetwork.nodes.id
  deletion_protection = true

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
    http_load_balancing { disabled = false }
  }
  # Gateway API for the public HTTPS entry point (#120, Ansible role ingress); in-place change on an existing cluster
  gateway_api_config { channel = "CHANNEL_STANDARD" }
  depends_on = [google_project_service.apis]
}

# Always-on pool for platform components (router, operator, UI). Small and on-demand.
resource "google_container_node_pool" "system" {
  name     = "system"
  cluster  = google_container_cluster.this.id
  location = var.zone
  autoscaling {
    min_node_count = 1
    max_node_count = 2
  }
  node_config {
    machine_type    = var.system_machine_type
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
    spot            = var.agent_pool_spot
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
resource "google_artifact_registry_repository" "images" {
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
resource "google_billing_budget" "monthly" {
  billing_account = var.billing_account_id
  display_name    = "agentdev ${var.project_id} monthly"
  budget_filter {
    projects = ["projects/${data.google_project.this.number}"]
  }
  amount {
    specified_amount {
      currency_code = "USD"
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
  for_each     = toset(var.budget_alert_emails)
  display_name = "budget ${each.value}"
  type         = "email"
  labels       = { email_address = each.value }
}
