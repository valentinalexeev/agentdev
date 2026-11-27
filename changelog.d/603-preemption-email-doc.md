---
issue: 603
type: docs
section: Added
status: done
breaking: false
title: "docs/admin.md: the task \"Get an e-mail for each preemption\" is back"
components: [docs]
---
## Summary

docs/admin.md, "Spot VMs (GKE)": the task "Get an e-mail for each preemption" is back. It uses
`examples/deployment/gcp-gke/terraform/monitoring.tf` (#589): `enable_diagnostic_alerts = true` and
`budget_alert_emails` give at most one e-mail per hour for Spot preemptions and security auto-pauses.
