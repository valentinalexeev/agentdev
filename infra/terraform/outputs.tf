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
  description = "Values for google-github-actions/auth in a workflow."
  value = {
    workload_identity_provider = google_iam_workload_identity_pool_provider.github.name
    service_account            = google_service_account.automation.email
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

output "registry_proxy" {
  description = "Values for instance.yml registry_proxy_artifact_registry (#203); null when registry_proxy is false."
  value = length(google_artifact_registry_repository.proxy) == 0 ? null : {
    service_account = google_service_account.registry_cache[0].email
    npm             = "https://${var.region}-npm.pkg.dev/${var.project_id}/${google_artifact_registry_repository.proxy["npm"].repository_id}/"
    pypi            = "https://${var.region}-python.pkg.dev/${var.project_id}/${google_artifact_registry_repository.proxy["pypi"].repository_id}/"
    go              = "https://${var.region}-go.pkg.dev/${var.project_id}/${google_artifact_registry_repository.proxy["go"].repository_id}/"
  }
}
