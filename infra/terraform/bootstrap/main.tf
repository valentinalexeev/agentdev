# One-time bootstrap, run by the owner (#11 decision 4): creates the project, links billing and creates the
# state bucket the main configuration uses. Local state on purpose: this runs once and changes rarely.
#
#   cd infra/terraform/bootstrap
#   terraform init && terraform apply -var billing_account_id=XXXXXX-XXXXXX-XXXXXX
terraform {
  required_version = ">= 1.9"
  required_providers {
    google = { source = "hashicorp/google", version = "~> 7.0" }
  }
}

variable "project_id" {
  type    = string
  default = "valabs-agentdev"
}
variable "project_name" {
  type    = string
  default = "agentdev"
}
variable "billing_account_id" {
  type = string
}
variable "org_id" {
  description = "Organization id, if the project belongs to one. Leave empty for a project without an organization."
  type        = string
  default     = ""
}
variable "region" {
  type    = string
  default = "europe-west4"
}

provider "google" {
  region = var.region
}

resource "google_project" "this" {
  project_id      = var.project_id
  name            = var.project_name
  billing_account = var.billing_account_id
  org_id          = var.org_id == "" ? null : var.org_id
  deletion_policy = "PREVENT"
}

resource "google_project_service" "bootstrap" {
  for_each           = toset(["storage.googleapis.com", "serviceusage.googleapis.com", "cloudresourcemanager.googleapis.com", "cloudbilling.googleapis.com", "billingbudgets.googleapis.com"])
  project            = google_project.this.project_id
  service            = each.value
  disable_on_destroy = false
}

resource "google_storage_bucket" "tfstate" {
  project                     = google_project.this.project_id
  name                        = "${var.project_id}-tfstate"
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  versioning { enabled = true }
  lifecycle_rule {
    condition { num_newer_versions = 20 }
    action { type = "Delete" }
  }
  depends_on = [google_project_service.bootstrap]
}

output "next" {
  value = "cd .. && terraform init -backend-config=\"bucket=${google_storage_bucket.tfstate.name}\" && terraform apply -var billing_account_id=${var.billing_account_id}"
}
