# Optional diagnostics for killed agent jobs (exit code 137): log-based metrics and two alerts.
# Off by default: enable_diagnostic_alerts = true in instance.tfvars turns it on; false removes everything here.
#
# Needs two roles the module does not give the automation account: logging.configWriter (log-based metrics) and
# monitoring.alertPolicyEditor (alert policies). They are granted below, only while the switch is on.
variable "enable_diagnostic_alerts" {
  description = "Log-based metrics for security auto-pauses, Spot preemptions, autoscaler scale-downs and Pending agent pods, plus e-mail alerts for the first two (to budget_alert_emails)."
  type        = bool
  default     = false
}

locals {
  diag = var.enable_diagnostic_alerts
  diag_filters = {
    # the operator pauses an agent after too many security events; running jobs die with exit 137
    security_pause = "resource.type=\"k8s_container\" AND resource.labels.namespace_name=\"agentdev-platform\" AND jsonPayload.msg=\"agent paused by security event\""
    # a Spot VM was reclaimed by Compute Engine
    node_preempted = "logName=\"projects/${var.project_id}/logs/cloudaudit.googleapis.com%2Fsystem_event\" AND protoPayload.methodName=\"compute.instances.preempted\""
    # the cluster autoscaler removes a node; pods on it are evicted
    node_scale_down = "resource.type=\"k8s_node\" AND jsonPayload.reason=\"ScaleDown\""
    # an agent pod cannot be scheduled (no agent node, Spot capacity)
    agent_pod_pending = "resource.type=\"k8s_pod\" AND resource.labels.namespace_name=\"coding-agents\" AND jsonPayload.reason=\"FailedScheduling\""
  }
  diag_alerts = {
    security_pause = "agentdev: an agent was paused by a security event"
    node_preempted = "agentdev: a Spot node was preempted"
  }
}

resource "google_project_iam_member" "diag_log_config" {
  count   = local.diag ? 1 : 0
  project = var.project_id
  role    = "roles/logging.configWriter"
  member  = "serviceAccount:${module.agentdev.automation_service_account}"
}

resource "google_project_iam_member" "diag_alert_editor" {
  count   = local.diag ? 1 : 0
  project = var.project_id
  role    = "roles/monitoring.alertPolicyEditor"
  member  = "serviceAccount:${module.agentdev.automation_service_account}"
}

resource "google_logging_metric" "diag" {
  for_each   = local.diag ? local.diag_filters : {}
  project    = var.project_id
  name       = "agentdev_${each.key}"
  filter     = each.value
  depends_on = [google_project_iam_member.diag_log_config]
  metric_descriptor {
    metric_kind = "DELTA"
    value_type  = "INT64"
  }
}

resource "google_monitoring_notification_channel" "diag" {
  for_each     = local.diag ? toset(var.budget_alert_emails) : toset([])
  project      = var.project_id
  display_name = "agentdev diagnostics ${each.value}"
  type         = "email"
  labels       = { email_address = each.value }
}

resource "google_monitoring_alert_policy" "diag" {
  for_each     = local.diag ? local.diag_alerts : {}
  project      = var.project_id
  display_name = each.value
  combiner     = "OR"
  depends_on   = [google_project_iam_member.diag_alert_editor]

  conditions {
    display_name = each.value
    condition_matched_log { filter = local.diag_filters[each.key] }
  }
  alert_strategy {
    notification_rate_limit { period = "3600s" }
    auto_close = "1800s"
  }
  notification_channels = [for c in google_monitoring_notification_channel.diag : c.id]
  user_labels           = var.labels
}
