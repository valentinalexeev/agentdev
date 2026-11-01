# Public HTTPS entry point (#120): the global static IP (main.tf, google_compute_global_address.webhook), a
# Google-managed certificate for the public host names and, optionally, a Cloud Armor policy. The Ansible role
# `ingress` makes the GKE Gateway that uses them by name (outputs ingress_*). Without public_hosts nothing here is
# created and the instance has no public HTTPS entry point (polling still works).

# Google-managed certificate: Google gets and renews it when the DNS A records of all domains point at webhook_ip.
# A changed host list makes a new certificate (create_before_destroy), so the Gateway never has none.
resource "google_compute_managed_ssl_certificate" "public" {
  count = length(var.public_hosts) > 0 ? 1 : 0
  name  = "agentdev-public-${substr(sha256(join(",", sort(var.public_hosts))), 0, 8)}"
  managed {
    domains = sort(var.public_hosts)
  }
  lifecycle {
    create_before_destroy = true
  }
  depends_on = [google_project_service.apis]
}

# Modern TLS only (TLS 1.2+, no legacy ciphers); the Gateway refers to it by name (GCPGatewayPolicy).
resource "google_compute_ssl_policy" "public" {
  count           = length(var.public_hosts) > 0 ? 1 : 0
  name            = "agentdev-public"
  profile         = "MODERN"
  min_tls_version = "TLS_1_2"
  depends_on      = [google_project_service.apis]
}

# Optional Cloud Armor (cost: about $5/month for the policy plus $1/month per rule, plus requests). Rules, in order:
# deny bodies larger than public_max_request_bytes (by Content-Length), throttle per client IP, allow the rest.
resource "google_compute_security_policy" "public" {
  count       = length(var.public_hosts) > 0 && var.cloud_armor ? 1 : 0
  name        = "agentdev-public"
  description = "agentdev public entry point (#120): size limit and per-IP rate limit"

  rule {
    action      = "deny(413)"
    priority    = 1000
    description = "request body larger than public_max_request_bytes"
    match {
      expr {
        expression = "has(request.headers['content-length']) && int(request.headers['content-length']) > ${var.public_max_request_bytes}"
      }
    }
  }

  rule {
    action      = "throttle"
    priority    = 2000
    description = "per client IP rate limit"
    match {
      versioned_expr = "SRC_IPS_V1"
      config {
        src_ip_ranges = ["*"]
      }
    }
    rate_limit_options {
      conform_action = "allow"
      exceed_action  = "deny(429)"
      enforce_on_key = "IP"
      rate_limit_threshold {
        count        = var.cloud_armor_requests_per_minute
        interval_sec = 60
      }
    }
  }

  rule {
    action      = "allow"
    priority    = 2147483647
    description = "default"
    match {
      versioned_expr = "SRC_IPS_V1"
      config {
        src_ip_ranges = ["*"]
      }
    }
  }
  depends_on = [google_project_service.apis]
}
