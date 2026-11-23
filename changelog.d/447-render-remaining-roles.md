---
issue: 447
type: feature
section: Added
status: done
breaking: false
title: Render mode covers the platform, authentik and API roles
components: [ansible]
pr: 451
---
## Summary

The roles `platform`, `authentik`, `authentik_sso` and `api` render through their `tasks/render.yml`, so
`infra/ansible/render.yml` writes the whole instance: authentik as `helm template` output, the database and
break-glass steps as the Jobs `agentdev-db-init-<hash>`, `authentik-db-init-<hash>` and `authentik-breakglass-<hash>`
of #382, Secrets by name only (`SECRETS.md`). Docs: `docs/admin.md`, "Render the manifests for a GitOps engine".

## Upgrade

Nothing to do. `deploy.yml` applies the same objects as before. To use the render mode, see #384.

## Description

#384 rendered only the agent objects. This change adds the other roles, with no cluster or Secret Manager access and
no secret values in the output.

- **platform:** Postgres, ContextForge (only with MCP projects) and `obs-mcp`. The template Secrets `platform-db` and
  `contextforge` are removed from the output. Placeholders fill their values only during the render, and a test
  checks that no placeholder remains.
- **authentik:** `helm template` of the pinned chart with `manifests/authentik-values.yaml`, plus the ConfigMap
  `authentik-blueprints`. The output has no Helm hooks and no `lookup`. The chart annotation `checksum/secret` is
  removed: it is a hash of empty chart values, and gitleaks reports it as a key.
- **database and break-glass:** the same Jobs as `deploy.yml` (#382), from `tasks/db_init_render.yml`. The passwords
  come from the Secrets when the Job runs. A changed schema makes a new Job name.
- **api:** the API, the web UI, the NotificationChannel CRD and the `webpush` channel.
- **authentik_sso:** Secret name only (`authentik-sso`).
- `SECRETS.md` lists the new Secret names (`platform-db`, `contextforge`, `obs-mcp-token`, `authentik`,
  `authentik-sso`, `agentdev-api-db`, `agentdev-api-forge`, `agentdev-api-github`, `agentdev-api-siem`,
  `agentdev-api-chat`, `agentdev-api-vapid`). After #445, ExternalSecret objects can replace that list.
- File order: namespaces, platform, authentik, the API CRD, the DB Jobs, API, UI, then the agent objects of #384.

Checks: `tests/render_mode.yml` renders the example with authentik, the API and webpush, and runs `kubeconform` and
`gitleaks`.

Limit: the output lists the Secrets by name only. ExternalSecret objects in the output come after #445.
