output "cluster" {
  value = { name = google_container_cluster.this.name, location = google_container_cluster.this.location }
}

output "get_credentials" {
  value = "gcloud container clusters get-credentials ${google_container_cluster.this.name} --zone ${var.zone} --project ${var.project_id}"
}

output "image_registry" {
  value = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.images.repository_id}"
}

output "webhook_ip" {
  description = "Static IP for the webhook router Ingress; point a DNS name at it and use https://<name>/ as the GitHub App webhook URL."
  value       = google_compute_global_address.webhook.address
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
