---
issue: 457
type: feature
section: Added
status: done
breaking: false
title: Render mode writes the ExternalSecrets of secrets_mode external
components: [ansible]
---
## Summary

With `external_secrets.mode: external`, `render.yml` writes the CEL policies, the SecretStores
`agentdev-platform-secrets` and the ExternalSecrets of `deploy.yml` and its roles (names and Secret Manager ids
only), and the forge-init objects with `forge_init.enabled`. `SECRETS.md` stays for playbook mode. The ESO and
Reloader charts are prerequisites of the GitOps engine. Docs: `docs/admin.md`, "Render the manifests for a GitOps engine".

## Description

The roles platform, authentik, authentik_sso, api and ingress set their ExternalSecret specs in `tasks/render.yml`
from the same templates as `tasks/main.yml`; the new `roles/external_secrets/tasks/render.yml` renders the stores and
policies. Two render fixes found by the new kubeconform and gitleaks checks: the CEL variable `n` of
`manifests/secrets-policy.yaml.tmpl` is now `psname` (a bare `n` is a YAML 1.1 boolean for Go parsers), and the
authentik render now really drops the chart's `checksum/secret` annotation (the old regex did not match). The ingress
checks accept `ingress.forwarder.deploy` (#316), which they refused before.
