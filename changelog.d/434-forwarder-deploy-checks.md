---
issue: 434
type: fix
section: Fixed
status: done
breaking: false
title: The ingress checks accept ingress.forwarder.deploy
components: [ansible]
pr: 493
---
## Summary

The Ansible role `ingress` no longer refuses `ingress.forwarder.deploy` (the deployed forwarder, #316, docs/admin.md
"Deployed forwarder"). The fix is in `roles/ingress/tasks/checks.yml`; `tests/ingress_external.yml` now covers it.
