terraform {
  required_version = ">= 1.9"
  required_providers {
    google      = { source = "hashicorp/google", version = "~> 7.0" }
    google-beta = { source = "hashicorp/google-beta", version = "~> 7.0" }
  }
  # State lives in a GCS bucket created by bootstrap/ (see README). Configure with:
  #   terraform init -backend-config="bucket=<project_id>-tfstate"
  backend "gcs" {
    prefix = "agentdev"
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}

provider "google-beta" {
  project = var.project_id
  region  = var.region
}
