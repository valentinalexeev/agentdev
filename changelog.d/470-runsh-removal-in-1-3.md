---
issues: [470, 476]
type: docs
section: Deprecated
status: done
breaking: false
title: run.sh and the kind step wrappers are now removed in 1.3.0, not 1.2.0
components: [kind, docs]
---
## Summary

`run.sh`, `04-coder.sh`, the numbered step wrappers and `99-teardown.sh` stay deprecated in 1.2.0 and are removed
in 1.3.0 (#470), not in 1.2.0 as the 1.1.0-rc notes said. Use `ansible-playbook infra/ansible/kind.yml --tags <tag>`.
`AGENTS.md` step 5 now matches `docs/install.md`: the owner applies only the CI identity, and the CI creates the
cluster (#471, #476).
