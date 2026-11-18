# Platform secrets (#381): generated values, one reader per namespace, the agents reader never gets platform-*.
# Plan only, mocked providers: cd infra/terraform && terraform init -backend=false && terraform test
mock_provider "google" {
  mock_data "google_project" {
    defaults = { number = "123456789012" }
  }
}
mock_provider "google-beta" {}
mock_provider "random" {}

variables {
  project_id         = "test-project"
  region             = "europe-west4"
  zone               = "europe-west4-a"
  billing_account_id = "000000-000000-000000"
  github_repository  = "example-org/example-deploy"
  agent_names        = ["alice", "bob"]
  external_secrets   = true
}

# Default (playbook mode, an upgrade from 1.1.x with no new inputs): no secret, account or grant from secrets.tf
run "off_by_default" {
  command = plan
  variables {
    external_secrets = false
  }
  assert {
    condition = (length(random_id.generated) + length(google_secret_manager_secret.generated)
      + length(google_secret_manager_secret_version.generated) + length(google_service_account.secrets_reader)
    + length(google_project_iam_member.secrets_reader) + length(google_service_account_iam_member.secrets_reader_wi)) == 0
    error_message = "secrets.tf must make no resource unless external_secrets = true"
  }
}

run "generated_and_scoped" {
  command = plan
  assert {
    condition     = contains(keys(google_secret_manager_secret.generated), "platform-pg-admin") && contains(keys(google_secret_manager_secret.generated), "agent-bob-wake-token") && contains(keys(google_secret_manager_secret.generated), "claude-proxy-signing")
    error_message = "platform, per-agent wake and proxy signing secrets are generated"
  }
  assert {
    condition     = length(google_secret_manager_secret_version.generated) == length(google_secret_manager_secret.generated)
    error_message = "a fresh instance seeds every generated secret"
  }
  assert {
    condition     = !strcontains(google_project_iam_member.secrets_reader["agents"].condition[0].expression, "/secrets/platform-")
    error_message = "the agents reader must never read platform-* (#381 acceptance 3)"
  }
  assert {
    condition     = strcontains(google_project_iam_member.secrets_reader["platform"].condition[0].expression, "startsWith(\"projects/123456789012/secrets/platform-\")")
    error_message = "the platform reader reads platform-*"
  }
  assert {
    condition     = !strcontains(google_project_iam_member.secrets_reader["platform"].condition[0].expression, "claude-shared") && !strcontains(google_project_iam_member.secrets_reader["platform"].condition[0].expression, "agentdev-ps-")
    error_message = "the platform reader never reads Claude credentials or project secrets"
  }
  assert {
    condition     = google_service_account_iam_member.secrets_reader_wi["agents"].member == "serviceAccount:test-project.svc.id.goog[coding-agents/agentdev-platform-secrets]"
    error_message = "Workload Identity binds the store's service account in its own namespace only"
  }
}

run "upgrade_keeps_values" {
  command = plan
  variables {
    keep_existing_secret_values = ["platform-pg-admin", "agent-alice-wake-token"]
  }
  assert {
    condition     = !contains(keys(google_secret_manager_secret_version.generated), "platform-pg-admin") && !contains(keys(google_secret_manager_secret_version.generated), "agent-alice-wake-token") && contains(keys(google_secret_manager_secret_version.generated), "agent-bob-wake-token")
    error_message = "kept secrets get no new version"
  }
}
