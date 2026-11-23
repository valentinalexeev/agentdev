output "cluster" {
  value = { name = google_container_cluster.this.name, location = google_container_cluster.this.location }
}

output "get_credentials" {
  value = "gcloud container clusters get-credentials ${google_container_cluster.this.name} --zone ${var.zone} --project ${var.project_id}"
}

output "image_registry" {
  description = "Artifact Registry path; null when create_artifact_registry is false (images then come from GHCR, the Ansible image_registry)."
  value       = one(google_artifact_registry_repository.images[*].repository_id) == null ? null : "${var.region}-docker.pkg.dev/${var.project_id}/${one(google_artifact_registry_repository.images[*].repository_id)}"
}

output "webhook_ip" {
  description = "Static IP for the public entry point; point the DNS names at it and use https://<name>/ as the GitHub App webhook URL. null without public_hosts (#169)."
  value       = one(google_compute_global_address.webhook[*].address)
}

output "automation_service_account" {
  value = google_service_account.automation.email
}

output "github_actions_auth" {
  description = "Values for google-github-actions/auth in a workflow: the apply identity (main) and the read-only plan identity (pull_request, #291)."
  value = {
    workload_identity_provider = google_iam_workload_identity_pool_provider.github.name
    service_account            = google_service_account.automation.email
    # read-only plan identity for pull_request runs (#291); null when create_plan_identity is false
    plan_workload_identity_provider = one(google_iam_workload_identity_pool_provider.github_pr[*].name)
    plan_service_account            = one(google_service_account.plan[*].email)
  }
}

output "ingress" {
  description = "Names for instance.yml `ingress` (Ansible role ingress, #120): copy them as they are."
  value = {
    static_ip_name  = try(google_compute_global_address.webhook[0].name, "")
    certificate     = try(google_compute_managed_ssl_certificate.public[0].name, "")
    ssl_policy      = try(google_compute_ssl_policy.public[0].name, "")
    security_policy = try(google_compute_security_policy.public[0].name, "")
  }
}

output "budget" {
  description = "Name of the billing budget; null when create_budget is false (#199)."
  value       = one(google_billing_budget.monthly[*].name)
}

# #384: what the playbook read from the cluster before, as instance values. Copy into ansible/instance.yml `platform`,
# so the render mode (infra/ansible/render.yml) needs no cluster access. The preflight compares them with the cluster.
output "platform" {
  description = "Cluster facts for instance.yml `platform` (Ansible render mode, #384): copy them as they are."
  value = {
    egress       = { fqdn = "gke" }                                                                 # enable_fqdn_network_policy is always on in this module
    kube_version = join(".", slice(split(".", google_container_cluster.this.master_version), 0, 2)) # helm template (#451)
    ingress      = { gateway_api = local.gateway, certificate_domains = sort(var.public_hosts) }
    # Service default/kubernetes: the first address of the services range, and the control plane private endpoint
    kube_api_ips = compact([
      cidrhost(one([for r in google_compute_subnetwork.nodes.secondary_ip_range : r.ip_cidr_range if r.range_name == "services"]), 1),
      google_container_cluster.this.private_cluster_config[0].private_endpoint,
    ])
  }
}
