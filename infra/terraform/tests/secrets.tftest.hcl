# Plan-only checks of the project secrets IAM (#202 part 4, on the #381 platform secrets). No credentials, no cloud calls:
#   terraform init -backend=false && terraform test
mock_provider "google" {
  mock_data "google_project" {
    defaults = { number = "123456789" }
  }
}
mock_provider "google-beta" {}

variables {
  project_id         = "p"
  region             = "europe-west1"
  zone               = "europe-west1-b"
  github_repository  = "owner/deploy"
  billing_account_id = "000000-000000-000000"
  external_secrets   = true
}
mock_provider "random" {}

run "off_without_external_secrets" {
  command = plan
  variables {
    external_secrets = false
  }
  assert {
    condition     = length(google_service_account.api_secrets) + length(google_service_account.project_secrets_reader) == 0
    error_message = "project secrets accounts only with external_secrets (ESO comes from the role external_secrets)"
  }
}

run "secret_create_is_allowed_on_the_project_only_for_create" {
  command = plan

  assert {
    condition     = google_project_iam_custom_role.api_secrets_create[0].permissions == toset(["secretmanager.secrets.create"])
    error_message = "the create role must hold secrets.create and nothing else"
  }
  assert {
    condition = google_project_iam_member.api_secrets_create[0].condition[0].expression == join("", [
      "resource.type == \"cloudresourcemanager.googleapis.com/Project\" || ",
      "resource.name.startsWith(\"projects/123456789/secrets/agentdev-ps-\")",
    ])
    error_message = "create must be allowed on the project (Secret Manager checks it on the parent)"
  }
  assert {
    condition     = !contains(tolist(google_project_iam_custom_role.api_secrets[0].permissions), "secretmanager.secrets.create")
    error_message = "secrets.create must not sit under the name-only condition (it never matches at create time)"
  }
  assert {
    condition     = google_project_iam_member.api_secrets[0].condition[0].expression == "resource.name.startsWith(\"projects/123456789/secrets/agentdev-ps-\")"
    error_message = "version, destroy and delete only on agentdev-ps-*"
  }
  assert {
    condition = length(setintersection(
      toset(concat(tolist(google_project_iam_custom_role.api_secrets[0].permissions), tolist(google_project_iam_custom_role.api_secrets_create[0].permissions))),
      toset(["secretmanager.versions.access", "secretmanager.secrets.list", "secretmanager.versions.list"])
    )) == 0
    error_message = "the API account must never read or list values"
  }
}

run "readers_are_disjoint" {
  command = plan
  assert {
    condition     = google_project_iam_member.project_secrets_reader[0].condition[0].expression == "resource.name.startsWith(\"projects/123456789/secrets/agentdev-ps-\")"
    error_message = "the project secrets reader reads agentdev-ps-* only"
  }
  assert {
    condition     = !strcontains(google_project_iam_member.secrets_reader["agents"].condition[0].expression, "agentdev-ps-") && !strcontains(google_project_iam_member.secrets_reader["agents"].condition[0].expression, "secrets/agentdev")
    error_message = "the platform agents reader must not reach agentdev-ps-*"
  }
  assert {
    condition     = google_service_account_iam_member.project_secrets_reader_wi[0].member == "serviceAccount:p.svc.id.goog[coding-agents/agentdev-secrets-reader]"
    error_message = "the reader is bound to the SecretStore's KSA only"
  }
  assert {
    condition     = google_service_account_iam_member.api_secrets_wi[0].member == "serviceAccount:p.svc.id.goog[agentdev-platform/agentdev-api]"
    error_message = "the writer is bound to the API's KSA only"
  }
}
