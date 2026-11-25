---
issue: 499
type: feature
section: Added
status: done
breaking: false
title: Toolchain setup egress, chat Job policy and settings for per-agent toolchains
components: [ansible, terraform, manifests, docs]
---
## Summary

Set a toolchain per agent with `agents[].toolchain` (GKE) or `kind_agents_toolchain` (kind), for example
`{presets: {go: "1.27.4"}}`. See docs/admin.md "Give an agent a toolchain", with the volume size guidance.

## Description

The egress proxy allows the preset download hosts (`setup_egress_hosts`, `SETUP_EGRESS_HOSTS`) only with the new
credential `egress-setup-proxy`, which the operator gives only to the `toolchain` init container. The playbook generates
it in Secret Manager (playbook mode), Terraform generates it with `external_secrets`, and kind keeps it in a file. The
chat Job admission policy allows the `toolchain` init container with the agent's own setup key, and requires
`/state/toolchain` read-only in chat and forge. docs/security.md R121 to R124.
