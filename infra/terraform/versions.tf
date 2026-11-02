terraform {
  required_version = ">= 1.9"
  required_providers {
    google      = { source = "hashicorp/google", version = "~> 7.0" }
    google-beta = { source = "hashicorp/google-beta", version = "~> 7.0" }
  }
  # No backend here: this directory is a reusable module. The deployment repository's root module
  # declares the backend (a GCS bucket created by bootstrap/), see examples/deployment/gcp-gke.
}

# Labels (#184): every resource with a labels field gets them. Cluster, node pools and the notification channel set
# theirs explicitly (main.tf), default_labels does not reach those fields.
locals {
  labels = merge({ service = "agentdev" }, var.labels)
}

provider "google" {
  project        = var.project_id
  region         = var.region
  default_labels = local.labels
}

provider "google-beta" {
  project        = var.project_id
  region         = var.region
  default_labels = local.labels
}
