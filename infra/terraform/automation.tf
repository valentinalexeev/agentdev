# Automation identity for later Terraform runs and CI (#11 decision 4).
# Preferred: GitHub Actions authenticate through Workload Identity Federation (no key file).
# Fallback for local runs: export a key (see README), kept outside the repo.

resource "google_service_account" "automation" {
  account_id   = "agentdev-automation"
  display_name = "agentdev automation (Terraform, CI deploys)"
}

# Scoped to what the configuration manages; no Owner/Editor.
resource "google_project_iam_member" "automation" {
  for_each = toset([
    "roles/container.admin",
    "roles/compute.networkAdmin",
    "roles/compute.securityAdmin",
    "roles/artifactregistry.admin",
    "roles/secretmanager.admin",
    "roles/iam.serviceAccountUser",
    "roles/iam.serviceAccountAdmin",
    "roles/resourcemanager.projectIamAdmin",
    "roles/serviceusage.serviceUsageAdmin",
    "roles/monitoring.notificationChannelEditor",
    "roles/storage.objectAdmin", # Terraform state bucket
  ])
  project = var.project_id
  role    = each.value
  member  = "serviceAccount:${google_service_account.automation.email}"
}

resource "google_iam_workload_identity_pool" "github" {
  workload_identity_pool_id = "github"
  display_name              = "GitHub Actions"
  depends_on                = [google_project_service.apis]
}

resource "google_iam_workload_identity_pool_provider" "github" {
  workload_identity_pool_id          = google_iam_workload_identity_pool.github.workload_identity_pool_id
  workload_identity_pool_provider_id = "github-oidc"
  display_name                       = "GitHub OIDC"
  attribute_mapping = {
    "google.subject"       = "assertion.sub"
    "attribute.repository" = "assertion.repository"
    "attribute.ref"        = "assertion.ref"
  }
  # Only this repository's workflows on main may impersonate the automation account.
  attribute_condition = "assertion.repository == \"${var.github_repository}\" && assertion.ref == \"refs/heads/main\""
  oidc { issuer_uri = "https://token.actions.githubusercontent.com" }
}

resource "google_service_account_iam_member" "github_wif" {
  service_account_id = google_service_account.automation.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.github.name}/attribute.repository/${var.github_repository}"
}
