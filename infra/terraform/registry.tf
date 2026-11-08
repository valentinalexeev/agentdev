# ---------- package registry cache (#203) ----------
# Artifact Registry remote repositories cache the public npm, PyPI and Go registries. Agents never hold a credential:
# they talk to the in-cluster gateway registry-cache (Ansible role registry_cache), which reaches these repositories as
# the service account below through Workload Identity. The reader role is granted per repository, not per project.
# Read-only: no writer role exists here; publishing stays an executor job (#69).
locals {
  registry_proxy_repos = var.registry_proxy ? {
    npm  = { format = "NPM", description = "npm cache (registry.npmjs.org)" }
    pypi = { format = "PYTHON", description = "PyPI cache (pypi.org)" }
    go   = { format = "GO", description = "Go module cache (proxy.golang.org)" }
  } : {}
}

resource "google_artifact_registry_repository" "proxy" {
  for_each      = local.registry_proxy_repos
  location      = var.region
  repository_id = "agentdev-${each.key}"
  format        = each.value.format
  mode          = "REMOTE_REPOSITORY"
  description   = each.value.description
  # labels come from the provider default_labels (#184)

  remote_repository_config {
    description = each.value.description
    dynamic "npm_repository" {
      for_each = each.key == "npm" ? [1] : []
      content {
        public_repository = "NPMJS"
      }
    }
    dynamic "python_repository" {
      for_each = each.key == "pypi" ? [1] : []
      content {
        public_repository = "PYPI"
      }
    }
    dynamic "common_repository" {
      for_each = each.key == "go" ? [1] : []
      content {
        uri = "https://proxy.golang.org"
      }
    }
  }
  # a cache, not a store: drop versions nobody pulled for 30 days (storage is the only idle cost)
  cleanup_policies {
    id     = "drop-old"
    action = "DELETE"
    condition { older_than = "2592000s" }
  }
  depends_on = [google_project_service.apis]
}

resource "google_service_account" "registry_cache" {
  count        = length(local.registry_proxy_repos) > 0 ? 1 : 0
  account_id   = "agentdev-registry-cache"
  display_name = "Registry cache gateway: reads the Artifact Registry remote repositories (#203)"
}

resource "google_artifact_registry_repository_iam_member" "registry_cache" {
  for_each   = local.registry_proxy_repos
  location   = google_artifact_registry_repository.proxy[each.key].location
  repository = google_artifact_registry_repository.proxy[each.key].name
  role       = "roles/artifactregistry.reader"
  member     = "serviceAccount:${google_service_account.registry_cache[0].email}"
}

resource "google_service_account_iam_member" "registry_cache_wi" {
  count              = length(local.registry_proxy_repos) > 0 ? 1 : 0
  service_account_id = google_service_account.registry_cache[0].name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.agents_namespace}/registry-cache]"
  depends_on         = [google_container_cluster.this] # the workload pool exists with the cluster
}
