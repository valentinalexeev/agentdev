terraform {
  required_version = ">= 1.9"
  required_providers {
    google      = { source = "hashicorp/google", version = "~> 7.0" }
    google-beta = { source = "hashicorp/google-beta", version = "~> 7.0" }
  }
  # No backend here: this directory is a reusable module. The deployment repository's root module
  # declares the backend (a GCS bucket created by bootstrap/), see examples/deployment/gcp-gke.
}

provider "google" {
  project = var.project_id
  region  = var.region
}

provider "google-beta" {
  project = var.project_id
  region  = var.region
}
