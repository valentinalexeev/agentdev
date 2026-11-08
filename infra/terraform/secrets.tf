# Scoped secrets store (#202, ADR 0003 D1) on GKE: two Google service accounts, both limited by IAM conditions to the
# Secret Manager secrets named agentdev-ps-* (the values of ProjectSecrets). Nothing else in the project is reachable.
#
#   agentdev-secrets-reader  External Secrets Operator's SecretStore (KSA <agents_ns>/agentdev-secrets-reader):
#                            read the values (secretAccessor), to sync them into ps-<name> next to the broker.
#   agentdev-api-secrets     the agentdev API (KSA <platform_ns>/agentdev-api): create a secret, add a version, delete.
#                            A custom role without secretmanager.versions.access: the API can write but never read.
#
# Off with secrets_store = false. The Ansible playbook reads the e-mails from `terraform output secrets_store`.

resource "google_service_account" "secrets_reader" {
  count        = var.secrets_store ? 1 : 0
  account_id   = "agentdev-secrets-reader"
  display_name = "agentdev project secrets: External Secrets Operator (read agentdev-ps-*)"
  depends_on   = [google_project_service.apis]
}

resource "google_service_account" "api_secrets" {
  count        = var.secrets_store ? 1 : 0
  account_id   = "agentdev-api-secrets"
  display_name = "agentdev project secrets: API (write-only agentdev-ps-*)"
  depends_on   = [google_project_service.apis]
}

locals {
  # IAM condition on the secret's resource name; secrets are named projects/<number>/secrets/<id>
  ps_condition = "resource.name.startsWith(\"projects/${data.google_project.this.number}/secrets/agentdev-ps-\")"
}

resource "google_project_iam_member" "secrets_reader" {
  count   = var.secrets_store ? 1 : 0
  project = var.project_id
  role    = "roles/secretmanager.secretAccessor"
  member  = "serviceAccount:${google_service_account.secrets_reader[0].email}"
  condition {
    title      = "agentdev-ps only"
    expression = local.ps_condition
  }
}

# write-only: create the secret, add versions, delete; no versions.access, no list of other secrets
resource "google_project_iam_custom_role" "api_secrets" {
  count       = var.secrets_store ? 1 : 0
  role_id     = "agentdevProjectSecretWriter"
  title       = "agentdev project secret writer"
  description = "Create, version and delete agentdev-ps-* secrets without reading them (#202)"
  permissions = [
    "secretmanager.secrets.create",
    "secretmanager.secrets.delete",
    "secretmanager.secrets.get",
    "secretmanager.versions.add",
  ]
}

resource "google_project_iam_member" "api_secrets" {
  count   = var.secrets_store ? 1 : 0
  project = var.project_id
  role    = google_project_iam_custom_role.api_secrets[0].id
  member  = "serviceAccount:${google_service_account.api_secrets[0].email}"
  condition {
    title      = "agentdev-ps only"
    expression = local.ps_condition
  }
}

# Workload Identity: the Kubernetes service accounts act as these Google accounts (the cluster's workload pool)
resource "google_service_account_iam_member" "secrets_reader_wi" {
  count              = var.secrets_store ? 1 : 0
  service_account_id = google_service_account.secrets_reader[0].name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.agents_namespace}/agentdev-secrets-reader]"
  depends_on         = [google_container_cluster.this]
}

resource "google_service_account_iam_member" "api_secrets_wi" {
  count              = var.secrets_store ? 1 : 0
  service_account_id = google_service_account.api_secrets[0].name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.platform_namespace}/agentdev-api]"
  depends_on         = [google_container_cluster.this]
}
