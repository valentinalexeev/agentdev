# One-time bootstrap, run by the owner (#11 decision 4): creates the project, links billing and creates the
# state bucket the main configuration uses. Local state on purpose: this runs once and changes rarely.
#
#   cd infra/terraform/bootstrap
#   terraform init && terraform apply -var-file=../../../path/to/bootstrap.tfvars   (see example.tfvars)
terraform {
  required_version = ">= 1.9"
  required_providers {
    google = { source = "hashicorp/google", version = "~> 7.0" }
  }
}

variable "project_id" {
  description = "GCP project id of this agentdev instance: created here, or an existing one with create_project = false."
  type        = string
}
variable "create_project" {
  description = "true: create the project and link billing. false: use the existing project project_id (billing must already be linked); only the APIs and the state bucket are created."
  type        = bool
  default     = true
}
variable "project_name" {
  type    = string
  default = "agentdev"
}
variable "billing_account_id" {
  description = "Billing account to link; required only when create_project is true."
  type        = string
  default     = ""
  validation {
    condition     = !var.create_project || var.billing_account_id != ""
    error_message = "billing_account_id is required when create_project is true."
  }
}
variable "org_id" {
  description = "Organization id, if the project belongs to one. Leave empty for a project without an organization."
  type        = string
  default     = ""
}
variable "region" {
  description = "Region for the state bucket, e.g. europe-west4."
  type        = string
}

provider "google" {
  region = var.region
}

resource "google_project" "this" {
  count           = var.create_project ? 1 : 0
  project_id      = var.project_id
  name            = var.project_name
  billing_account = var.billing_account_id
  org_id          = var.org_id == "" ? null : var.org_id
  deletion_policy = "PREVENT"
}

locals {
  project_id = var.project_id
}

resource "google_project_service" "bootstrap" {
  for_each           = toset(["storage.googleapis.com", "serviceusage.googleapis.com", "cloudresourcemanager.googleapis.com", "cloudbilling.googleapis.com", "billingbudgets.googleapis.com"])
  project            = local.project_id
  service            = each.value
  disable_on_destroy = false
  depends_on         = [google_project.this]
}

resource "google_storage_bucket" "tfstate" {
  project                     = local.project_id
  name                        = "${var.project_id}-tfstate"
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  versioning { enabled = true }
  lifecycle_rule {
    condition { num_newer_versions = 20 }
    action { type = "Delete" }
  }
  depends_on = [google_project_service.bootstrap, google_project.this]
}

output "next" {
  value = "terraform init -backend-config=\"bucket=${google_storage_bucket.tfstate.name}\" in the main configuration (or the deployment repo root module)"
}
