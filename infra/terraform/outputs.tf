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

output "secrets_store" {
  description = "Google service accounts of the scoped secrets store (#202) for instance.yml `secrets_store`; null when secrets_store is false."
  value = var.secrets_store ? {
    reader_gsa = google_service_account.secrets_reader[0].email
    api_gsa    = google_service_account.api_secrets[0].email
  } : null
}
