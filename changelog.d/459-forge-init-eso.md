---
issue: 459
type: fix
section: Fixed
status: done
breaking: false
title: forge-init reads its secrets through the platform store
components: [terraform, ansible]
pr: 460
---
## Summary

Terraform `forge_init = true` lets the platform reader read `gitea-admin-token` and `github-app-webhook-secret`, so
the ExternalSecret `forge-init` of #442 syncs. The playbook refuses `forge_init.enabled` without
`external_secrets.mode: external`. Docs: `docs/admin.md`, external Gitea.
