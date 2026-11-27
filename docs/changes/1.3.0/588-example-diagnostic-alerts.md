# Deployment example: optional log-based metrics and alerts for killed agent jobs (#588)

Released in 1.3.0 (2026-11-27). Type: feature. Section: Added. Components: terraform, docs.

## Summary

`examples/deployment/gcp-gke/terraform/monitoring.tf` adds, behind `enable_diagnostic_alerts` (default `false`), log-based
metrics for security auto-pauses, Spot preemptions, autoscaler scale-downs and Pending agent pods, and e-mail alerts for the
first two. `monitoring_components` with `POD` and `STATEFULSET` adds kube-state-metrics (last terminated reason, exit code).

## Upgrade

1. Optional. Copy `terraform/monitoring.tf` into the deployment repository and set `enable_diagnostic_alerts = true`.
   The automation account gets `roles/logging.configWriter` and `roles/monitoring.alertPolicyEditor` while it is on.

## Description

Tried on a live instance after four jobs ended with exit code 137 (#573, #570, #577, #578). The product side (record why
a job was killed) is #588; this part only gives an operator the platform signals.
