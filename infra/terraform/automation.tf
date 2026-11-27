# Automation identity for later Terraform runs and CI (#11 decision 4).
# Preferred: GitHub Actions authenticate through Workload Identity Federation (no key file).
# Fallback for local runs: export a key (see README), kept outside the repo.

resource "google_service_account" "automation" {
  account_id   = "agentdev-automation"
  display_name = "agentdev automation (Terraform, CI deploys)"
}

# Scoped to what the configuration manages; no Owner/Editor.
resource "google_project_iam_member" "automation" {
  for_each = toset(concat([
    "roles/container.admin",
    "roles/compute.networkAdmin",
    "roles/compute.securityAdmin",
    "roles/secretmanager.admin",
    "roles/iam.serviceAccountUser",
    "roles/iam.serviceAccountAdmin",
    "roles/iam.workloadIdentityPoolViewer", # refresh of the GitHub Actions WIF pool/provider
    "roles/resourcemanager.projectIamAdmin",
    "roles/serviceusage.serviceUsageAdmin",
    "roles/monitoring.notificationChannelEditor",
    "roles/storage.objectAdmin", # Terraform state bucket
    ], var.create_artifact_registry ? ["roles/artifactregistry.admin"] : []
  ))
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

# ---------- read-only plan identity for deployment PRs (#291) ----------
# A pull_request run executes PR code, so it must never reach the automation account above. It gets its own pool,
# provider and service account. The pool is separate on purpose: a principalSet binding is per pool, so a PR token
# in the "github" pool would also match the automation account's binding.

locals {
  plan_state_bucket = coalesce(var.plan_state_bucket, "${var.project_id}-tfstate")
  # Read only. tests/plan_identity.tftest.hcl refuses any other role.
  plan_project_roles = [
    "roles/viewer",               # read all resources of the project (no secret values)
    "roles/iam.securityReviewer", # read IAM policies of the project and its service accounts
  ]
}

resource "google_service_account" "plan" {
  count        = var.create_plan_identity ? 1 : 0
  account_id   = "agentdev-plan"
  display_name = "agentdev plan (read-only, deployment PRs)"
}

resource "google_project_iam_member" "plan" {
  for_each = var.create_plan_identity ? toset(local.plan_project_roles) : toset([])
  project  = var.project_id
  role     = each.value
  member   = "serviceAccount:${google_service_account.plan[0].email}"
}

# Read the Terraform state (no write: PR runs plan with -lock=false).
resource "google_storage_bucket_iam_member" "plan_state" {
  count  = var.create_plan_identity ? 1 : 0
  bucket = local.plan_state_bucket
  role   = "roles/storage.objectViewer"
  member = "serviceAccount:${google_service_account.plan[0].email}"
}

# Read the budget when the module manages one (#199).
resource "google_billing_account_iam_member" "plan_budgets" {
  count              = var.create_plan_identity && var.create_budget ? 1 : 0
  billing_account_id = var.billing_account_id
  role               = "roles/billing.viewer"
  member             = "serviceAccount:${google_service_account.plan[0].email}"
}

resource "google_iam_workload_identity_pool" "github_pr" {
  count                     = var.create_plan_identity ? 1 : 0
  workload_identity_pool_id = "github-pr"
  display_name              = "GitHub Actions PRs (plan only)"
  depends_on                = [google_project_service.apis]
}

resource "google_iam_workload_identity_pool_provider" "github_pr" {
  count                              = var.create_plan_identity ? 1 : 0
  workload_identity_pool_id          = google_iam_workload_identity_pool.github_pr[0].workload_identity_pool_id
  workload_identity_pool_provider_id = "github-pr-oidc"
  display_name                       = "GitHub OIDC (PRs)"
  attribute_mapping = {
    "google.subject"       = "assertion.sub"
    "attribute.repository" = "assertion.repository"
    "attribute.ref"        = "assertion.ref"
    "attribute.event_name" = "assertion.event_name"
  }
  # Only pull_request runs of the deployment repository. GitHub gives no OIDC token to a fork PR (its id-token
  # permission is read-only), and pull_request_target is refused here.
  attribute_condition = "assertion.repository == \"${var.github_repository}\" && assertion.event_name == \"pull_request\" && assertion.ref.startsWith(\"refs/pull/\")"
  oidc { issuer_uri = "https://token.actions.githubusercontent.com" }
}

resource "google_service_account_iam_member" "github_pr_wif" {
  count              = var.create_plan_identity ? 1 : 0
  service_account_id = google_service_account.plan[0].name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.github_pr[0].name}/attribute.repository/${var.github_repository}"
}
