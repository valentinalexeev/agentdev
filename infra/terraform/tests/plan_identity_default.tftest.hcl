# The plan identity (#291) is opt-in (#421): with the default, an upgrade from 1.0.x needs no new permissions for
# the apply account. Plan only, mocked providers: cd infra/terraform && terraform init -backend=false && terraform test
mock_provider "google" {
  mock_data "google_project" {
    defaults = { number = "123456789012" }
  }
}
mock_provider "google-beta" {}

variables {
  project_id         = "test-project"
  region             = "europe-west4"
  zone               = "europe-west4-a"
  billing_account_id = "000000-000000-000000"
  github_repository  = "example-org/example-deploy"
}

run "plan_identity_default_off" {
  command = plan
  assert {
    condition     = length(google_service_account.plan) == 0 && length(google_iam_workload_identity_pool.github_pr) == 0 && length(google_storage_bucket_iam_member.plan_state) == 0 && length(google_billing_account_iam_member.plan_budgets) == 0 && length(google_service_account_iam_member.github_pr_wif) == 0
    error_message = "the default must create no plan identity (#421)"
  }
  assert {
    condition     = output.github_actions_auth.plan_workload_identity_provider == null && output.github_actions_auth.plan_service_account == null
    error_message = "plan outputs are null by default"
  }
}
