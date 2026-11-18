# Platform secrets for the External Secrets Operator (#381, phase 1 of #388).
#
# 1. Generated secrets: Terraform makes each value once (random_id, hex so it is safe in SQL, env and URLs) and stores
#    it as a Secret Manager version. The value is then also in the Terraform state: protect the state bucket like the
#    secrets (docs/security.md R110). Upgrade from 1.1.x: the playbook made these secrets already. Import the
#    containers and list them in keep_existing_secret_values (examples/deployment/gcp-gke/terraform/imports.tf.example),
#    so Terraform keeps the existing values and adds no version.
# 2. Readers: one Google service account per namespace that consumes secrets, each bound by Workload Identity to the
#    SecretStore's Kubernetes service account in that namespace, each with an IAM condition that names only its own
#    secrets. The agents namespace can never read a platform-* secret (acceptance 3 of #381): its account has no
#    grant on them, and manifests/secrets-policy.yaml.tmpl pins its ExternalSecrets.
#    The project secrets store of #202 (agentdev-ps-*) is separate and unchanged.

locals {
  # name => random bytes (hex length = 2 x bytes); the same lengths as the playbook used (openssl rand -hex N)
  generated_secrets = merge(
    { for k in ["pg-admin", "pg-contextforge", "pg-agentdev", "pg-authentik", "jwt", "enc", "cf-admin",
    "cf-default-user", "cf-basic-auth", "authentik-secret-key", "authentik-admin"] : "platform-${k}" => 48 },
    {
      "platform-pg-agentdev-api"    = 24
      "egress-forge-proxy"          = 32
      "claude-proxy-signing"        = 32
      "agentdev-api-chat-token-key" = 32
    },
    { for a in var.agent_names : "agent-${a}-wake-token" => 32 },
  )

  sm_prefix = "projects/${data.google_project.this.number}/secrets/"

  # who reads what (exact secret ids; a trailing * is a prefix). Keep in step with the ExternalSecrets of the Ansible
  # role external_secrets (infra/ansible/roles/external_secrets/defaults/main.yml).
  secret_readers = merge({
    platform = {
      namespace = var.platform_namespace
      ids = concat(["platform-*", "agentdev-api-forge-token", "agentdev-api-chat-token-key", "agentdev-chat-issue-token",
        "security-siem", "github-app-private-key", "authentik-sso-*"],
      var.platform_extra_secret_ids)
    }
    agents = {
      namespace = var.agents_namespace
      ids = ["claude-shared", "router-register-token", "github-app-webhook-secret", "github-app-private-key",
      "router-poll-forge-token", "egress-forge-proxy", "claude-proxy-signing", "agent-*"]
    }
    executors = {
      namespace = var.executors_namespace
      ids       = ["exec-*"]
    }
    }, var.forwarder_namespace == "" ? {} : {
    # deployed forwarder (#316): only its pre-auth key, in its own namespace
    forwarder = {
      namespace = var.forwarder_namespace
      ids       = [var.forwarder_authkey_secret]
    }
  })
  # IAM condition per reader: the secret itself and its versions
  secret_conditions = {
    for r, v in local.secret_readers : r => join(" || ", [
      for id in v.ids : endswith(id, "*")
      ? "resource.name.startsWith(\"${local.sm_prefix}${trimsuffix(id, "*")}\")"
      : "resource.name == \"${local.sm_prefix}${id}\" || resource.name.startsWith(\"${local.sm_prefix}${id}/\")"
    ])
  }
}

resource "random_id" "generated" {
  for_each    = local.generated_secrets
  byte_length = each.value
}

resource "google_secret_manager_secret" "generated" {
  for_each  = local.generated_secrets
  secret_id = each.key
  replication {
    auto {}
  }
  depends_on = [google_project_service.apis]
}

resource "google_secret_manager_secret_version" "generated" {
  for_each    = { for k, v in local.generated_secrets : k => v if !contains(var.keep_existing_secret_values, k) }
  secret      = google_secret_manager_secret.generated[each.key].id
  secret_data = random_id.generated[each.key].hex
}

resource "google_service_account" "secrets_reader" {
  for_each     = local.secret_readers
  account_id   = "agentdev-sm-${each.key}"
  display_name = "agentdev ${each.key} secrets (External Secrets Operator, #381)"
}

resource "google_project_iam_member" "secrets_reader" {
  for_each = local.secret_readers
  project  = var.project_id
  role     = "roles/secretmanager.secretAccessor"
  member   = "serviceAccount:${google_service_account.secrets_reader[each.key].email}"
  condition {
    title      = "agentdev-${each.key}-secrets"
    expression = local.secret_conditions[each.key]
  }
}

# The SecretStore's Kubernetes service account agentdev-platform-secrets in each namespace (role external_secrets)
resource "google_service_account_iam_member" "secrets_reader_wi" {
  for_each           = local.secret_readers
  service_account_id = google_service_account.secrets_reader[each.key].name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${each.value.namespace}/agentdev-platform-secrets]"
  depends_on         = [google_container_cluster.this]
}
