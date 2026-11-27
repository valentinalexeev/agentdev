---
issue: 203
type: feature
section: Added
status: done
title: Registry cache for npm, PyPI and Go
components: [ansible, terraform, operator]
pr: 249
---
## Summary

Registry cache for npm, PyPI and Go, one setting `registry_proxy` (`off` by default, `cluster`, `artifact_registry`).
GKE: Terraform `registry_proxy = true` makes Artifact Registry remote repositories `agentdev-npm`, `agentdev-pypi`,
`agentdev-go` (reader IAM per repository for a gateway service account through Workload Identity, 30-day cleanup) and
output `registry_proxy`. kind and GKE without Artifact Registry: Ansible role `registry_cache` with Verdaccio, devpi
and Athens. Agents reach a read-only gateway through the ConfigMap `registry-proxy`; with the cache on, the public
registry hosts leave the agents' egress allow-list. Docs: `docs/admin.md` "Registry cache".

## Description

The gateway accepts GET and HEAD only. Agents get URLs, never a credential. Private registries per project: #248.
On kind set `registry_proxy: cluster` in `kind.vars.yml` and run the tags `registry-cache`, `operator` and `agents`.
