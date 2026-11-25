---
issue: 466
type: fix
section: Fixed
status: done
breaking: false
title: In Flux mode, Reloader watches the same namespaces as the Ansible role
components: [gitops]
pr: 546
---
## Summary

`scripts/gitops-layout.py` now sets the Reloader namespaces of `rendered/reloader` to the namespaces that have a
rendered SecretStore. This is the subset that the Ansible role `external_secrets` uses. Before, Flux mode watched
every rendered Namespace. A render without a SecretStore keeps the old scope. docs/admin.md, "Install an instance
with Flux".
