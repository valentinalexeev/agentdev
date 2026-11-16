# Read-only plan identity for deployment PRs (#291). Plan only, mocked providers, no credentials:
#   cd infra/terraform && terraform init -backend=false && terraform test
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

run "plan_identity_is_read_only" {
  command = plan

  assert {
    condition     = google_service_account.plan[0].account_id == "agentdev-plan"
    error_message = "the plan service account must be agentdev-plan"
  }
  # Allow-list: every project role of the plan account is a read-only role.
  assert {
    condition = alltrue([for r in keys(google_project_iam_member.plan) :
    contains(["roles/viewer", "roles/iam.securityReviewer"], r)])
    error_message = "the plan service account has a project role that is not on the read-only list"
  }
  # Deny-list: no write, admin, editor, owner, user or secret access role, on project, bucket or billing account.
  assert {
    condition = alltrue([for r in concat(
      keys(google_project_iam_member.plan),
      google_storage_bucket_iam_member.plan_state[*].role,
      google_billing_account_iam_member.plan_budgets[*].role,
    ) : !can(regex("(?i)(admin|editor|owner|writer|creator|user|manager|accessor|roles/storage\\.objectAdmin)", r))])
    error_message = "the plan service account must not get a write role"
  }
  assert {
    condition     = google_storage_bucket_iam_member.plan_state[0].role == "roles/storage.objectViewer" && google_storage_bucket_iam_member.plan_state[0].bucket == "test-project-tfstate"
    error_message = "the plan service account must read (only read) the state bucket"
  }
  assert {
    condition     = google_billing_account_iam_member.plan_budgets[0].role == "roles/billing.viewer"
    error_message = "the plan service account reads the budget with roles/billing.viewer only"
  }
}

run "plan_provider_accepts_only_same_repo_prs" {
  command = plan

  assert {
    condition     = strcontains(google_iam_workload_identity_pool_provider.github_pr[0].attribute_condition, "assertion.repository == \"example-org/example-deploy\"")
    error_message = "the PR provider must accept only the deployment repository"
  }
  assert {
    condition     = strcontains(google_iam_workload_identity_pool_provider.github_pr[0].attribute_condition, "assertion.event_name == \"pull_request\"")
    error_message = "the PR provider must accept only pull_request runs"
  }
  # The plan identity lives in its own pool, so its tokens cannot match the automation account's binding.
  assert {
    condition     = google_iam_workload_identity_pool.github_pr[0].workload_identity_pool_id != google_iam_workload_identity_pool.github.workload_identity_pool_id
    error_message = "the PR provider must be in a separate pool"
  }
  # The apply identity is unchanged: main only.
  assert {
    condition     = strcontains(google_iam_workload_identity_pool_provider.github.attribute_condition, "assertion.ref == \"refs/heads/main\"")
    error_message = "the apply provider must keep its refs/heads/main condition"
  }
}

run "no_budget_no_billing_read" {
  command = plan
  variables {
    create_budget = false
  }
  assert {
    condition     = length(google_billing_account_iam_member.plan_budgets) == 0
    error_message = "without create_budget the plan account gets no billing role"
  }
}

run "plan_identity_off" {
  command = plan
  variables {
    create_plan_identity = false
  }
  assert {
    condition     = length(google_service_account.plan) == 0 && length(google_iam_workload_identity_pool.github_pr) == 0 && length(google_project_iam_member.plan) == 0
    error_message = "create_plan_identity = false must create no plan identity"
  }
  assert {
    condition     = output.github_actions_auth.plan_service_account == null
    error_message = "plan outputs are null without the plan identity"
  }
}

run "custom_state_bucket" {
  command = plan
  variables {
    plan_state_bucket = "my-state"
  }
  assert {
    condition     = google_storage_bucket_iam_member.plan_state[0].bucket == "my-state"
    error_message = "plan_state_bucket overrides the default bucket"
  }
}
