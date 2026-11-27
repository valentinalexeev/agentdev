---
issue: 313
type: feature
section: Added
status: done
title: Stop and start a GKE instance; Terraform variable system_pool_min_nodes
components: [terraform, scripts]
pr: 320
---
## Summary

Stop and start a GKE instance (#313): `scripts/cluster-stop.sh` records the replicas and the system pool
autoscaling (after a `yes` confirmation, or `--yes`/`ASSUME_YES=1`) in the ConfigMap `kube-system/agentdev-stopped-state`, scales the operator and the router first, then
resizes the system pool to 0 with retries. `scripts/cluster-start.sh` restores exactly that state. New Terraform
variable `system_pool_min_nodes` (default 1, 0 allowed) keeps a stopped instance at 0 nodes. Procedure:
`docs/admin.md` "Stop and start an instance". Re-run: Terraform module only; no change with the default.
