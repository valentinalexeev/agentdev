---
issue: 251
type: fix
section: Fixed
status: done
breaking: false
title: The operator refuses FORGE_EPHEMERAL_LIMIT below FORGE_MIRROR_LIMIT
components: [operator]
---
## Summary

The operator no longer starts when `FORGE_EPHEMERAL_LIMIT` is less than `FORGE_MIRROR_LIMIT` (Ansible `forge_ephemeral_limit`,
`forge_mirror_limit`). It logs an error that names both values. Before, such a setting got agent pods evicted and
recreated over and over once the git mirror grew past the forge sidecar's limit.
