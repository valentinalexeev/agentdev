# System pool minimum (#313). Plan only, mocked providers, no credentials:
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

run "system_pool_min_default_is_1" {
  command = plan
  assert {
    condition     = google_container_node_pool.system.autoscaling[0].min_node_count == 1
    error_message = "the system pool minimum must be 1 by default"
  }
}

run "system_pool_min_0_for_a_stopped_instance" {
  command = plan
  variables { system_pool_min_nodes = 0 }
  assert {
    condition     = google_container_node_pool.system.autoscaling[0].min_node_count == 0
    error_message = "system_pool_min_nodes = 0 must give min_node_count 0"
  }
}

run "system_pool_min_refuses_negative" {
  command = plan
  variables { system_pool_min_nodes = -1 }
  expect_failures = [var.system_pool_min_nodes]
}

run "system_pool_min_refuses_above_max" {
  command = plan
  variables { system_pool_min_nodes = 3 }
  expect_failures = [var.system_pool_min_nodes]
}
