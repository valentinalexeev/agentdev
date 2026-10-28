# Coder workspace template for the spike, derived from coder/coder examples/templates/kubernetes.
# Differences: gVisor RuntimeClass, sandbox worker pinning, arch from the provisioner
# (works on arm64 kind), Claude Code installed at start, no CPU/RAM parameters.
terraform {
  required_providers {
    coder      = { source = "coder/coder" }
    kubernetes = { source = "hashicorp/kubernetes" }
  }
}

variable "namespace" {
  type    = string
  default = "coder"
}

provider "coder" {}
provider "kubernetes" {} # in-cluster service account of the Coder pod

data "coder_provisioner" "me" {}
data "coder_workspace" "me" {}
data "coder_workspace_owner" "me" {}

locals {
  id     = data.coder_workspace.me.id
  labels = {
    "app.kubernetes.io/name"   = "coder-workspace"
    "app.kubernetes.io/part-of" = "coder"
    "com.coder.workspace.id"   = data.coder_workspace.me.id
    "com.coder.workspace.name" = data.coder_workspace.me.name
    "com.coder.user.username"  = data.coder_workspace_owner.me.name
  }
}

resource "coder_agent" "main" {
  os   = "linux"
  arch = data.coder_provisioner.me.arch
  startup_script = <<-EOT
    set -e
    # Claude Code (native installer) on the persistent home volume, so it survives stop/start.
    command -v claude >/dev/null || [ -x "$HOME/.local/bin/claude" ] || curl -fsSL https://claude.ai/install.sh | bash
    echo 'export PATH="$HOME/.local/bin:$PATH"' >> "$HOME/.profile"
  EOT

  metadata {
    display_name = "Kernel"
    key          = "kernel"
    script       = "dmesg 2>/dev/null | head -1 || uname -r"
    interval     = 600
    timeout      = 2
  }
}

# Home survives stop: this PVC is not tied to start_count.
resource "kubernetes_persistent_volume_claim_v1" "home" {
  metadata {
    name      = "coder-${local.id}-home"
    namespace = var.namespace
    labels    = local.labels
  }
  wait_until_bound = false
  spec {
    access_modes = ["ReadWriteOnce"]
    resources { requests = { storage = "5Gi" } }
  }
}

# Compute only exists while the workspace is started (start_count = 0 on stop → scale to zero).
resource "kubernetes_deployment_v1" "main" {
  count            = data.coder_workspace.me.start_count
  depends_on       = [kubernetes_persistent_volume_claim_v1.home]
  wait_for_rollout = false
  metadata {
    name      = "coder-${local.id}"
    namespace = var.namespace
    labels    = local.labels
  }
  spec {
    replicas = 1
    selector { match_labels = local.labels }
    strategy { type = "Recreate" }
    template {
      metadata { labels = local.labels }
      spec {
        runtime_class_name = "gvisor"
        node_selector      = { "agentdev/pool" = "sandboxes" }
        security_context {
          run_as_user     = 1000
          fs_group        = 1000
          run_as_non_root = true
        }
        container {
          name    = "dev"
          image   = "codercom/example-base:ubuntu"
          command = ["sh", "-c", coder_agent.main.init_script]
          env {
            name  = "CODER_AGENT_TOKEN"
            value = coder_agent.main.token
          }
          resources {
            requests = { cpu = "250m", memory = "512Mi" }
            limits   = { cpu = "2", memory = "2Gi" }
          }
          volume_mount {
            name       = "home"
            mount_path = "/home/coder"
          }
        }
        volume {
          name = "home"
          persistent_volume_claim {
            claim_name = kubernetes_persistent_volume_claim_v1.home.metadata[0].name
          }
        }
      }
    }
  }
}
