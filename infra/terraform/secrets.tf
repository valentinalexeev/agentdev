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
  # IAM condition on the secret's resource name; secrets are named projects/<number>/secrets/<id> (versions below it)
  ps_condition = "resource.name.startsWith(\"projects/${data.google_project.this.number}/secrets/agentdev-ps-\")"
  # secretmanager.secrets.create is checked on the parent, the project, before the secret exists, so the name
  # condition alone would refuse every create (review of #274). This condition admits the project as the resource
  # and nothing else: the create role holds only secrets.create, so on the project it can create a secret but not
  # read, version or delete one. The secret id is always agentdev-ps-<name> (images/api/secrets_gcp.go); a secret
  # created under another id is unusable to the API (every other permission is under ps_condition).
  ps_create_condition = "resource.type == \"cloudresourcemanager.googleapis.com/Project\" || ${local.ps_condition}"
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

# write-only: add versions, destroy old ones, delete; no versions.access, no list of other secrets. Create is a
# separate role because it needs another IAM condition (see ps_create_condition).
resource "google_project_iam_custom_role" "api_secrets" {
  count       = var.secrets_store ? 1 : 0
  role_id     = "agentdevProjectSecretWriter"
  title       = "agentdev project secret writer"
  description = "Version, destroy old versions of and delete agentdev-ps-* secrets without reading them (#202)"
  permissions = [
    "secretmanager.secrets.delete",
    "secretmanager.secrets.get",
    "secretmanager.versions.add",
    "secretmanager.versions.destroy",
  ]
}

resource "google_project_iam_custom_role" "api_secrets_create" {
  count       = var.secrets_store ? 1 : 0
  role_id     = "agentdevProjectSecretCreator"
  title       = "agentdev project secret creator"
  description = "Create agentdev-ps-* secrets (checked on the project, #202)"
  permissions = ["secretmanager.secrets.create"]
}

resource "google_project_iam_member" "api_secrets_create" {
  count   = var.secrets_store ? 1 : 0
  project = var.project_id
  role    = google_project_iam_custom_role.api_secrets_create[0].id
  member  = "serviceAccount:${google_service_account.api_secrets[0].email}"
  condition {
    title      = "agentdev-ps create"
    expression = local.ps_create_condition
  }
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
