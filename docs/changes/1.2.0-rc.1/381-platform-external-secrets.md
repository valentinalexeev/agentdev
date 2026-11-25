# External secrets mode on GKE (#381)

Released in 1.2.0-rc.1 (2026-11-23). Type: feature. Section: Added. Components: terraform, ansible, operator, claude-proxy. PR: #445.

## Summary

GKE: `external_secrets.mode: external`. Terraform generates the platform secrets once in Secret Manager, and the
External Secrets Operator 2.14.0 makes the Kubernetes Secrets with unchanged names and keys. Stakater Reloader
v2.0.0-beta.2 (chart 3.0.0-beta.5, scoped) restarts annotated workloads. The operator writes the `agent-<n>-proxy`
keys. The Claude proxy refuses a bad `claude-shared` at start. The default `playbook` mode is unchanged, and Terraform
makes nothing new unless `external_secrets = true`.

## Upgrade

Nothing to do in the default `playbook` mode. To use external mode on an instance from 1.1.x, import the generated
secrets first and list them in `keep_existing_secret_values` (`docs/admin.md`,
`examples/deployment/gcp-gke/terraform/imports.tf.example`). Otherwise Terraform writes new database passwords. Then
set `external_secrets = true` in Terraform and `external_secrets.mode: external` in `ansible/instance.yml`, and
re-apply `infra/terraform` and `deploy.yml`.

## Description

With `external_secrets.mode: external` on GKE, no secret value passes through the operator machine or Ansible
(umbrella #388).

- **Terraform** (`infra/terraform/secrets.tf`) generates the random secrets once in Secret Manager: `platform-*`,
  `egress-forge-proxy`, `claude-proxy-signing`, `agentdev-api-chat-token-key` and `agent-<n>-wake-token`. It adds one
  Google service account per namespace (`agentdev-sm-*`) with IAM conditions on secret names, bound by Workload
  Identity to the Kubernetes service account `agentdev-platform-secrets`. The generated values are also in the
  Terraform state: protect the state bucket like the secrets (`docs/security.md` R119).
- **Ansible** role `external_secrets`: the ESO chart, Reloader, one SecretStore per namespace, and the CEL policies
  (an ExternalSecret in the agents namespace must use the platform store, may not read `platform-*` keys or use
  `dataFrom`). Each `gcloud` read is replaced by one ExternalSecret with the same name and keys.
- **Operator** writes `agent-<n>-proxy` (an HMAC of the agent name with `claude-proxy-signing`, the same value as
  before) by server-side apply, and sets the Reloader annotation on agent StatefulSets. In the agents namespace it has
  get on `claude-proxy-signing` by name only, and one rule create, patch, delete on Secrets (with #452); the policy
  `agentdev-operator-secrets` limits writes to `agent-*-proxy` and `agent-*-mcp` and deletes to `agent-*-mcp`.
- **Claude proxy** refuses an invalid `claude-shared` at start and reports an invalid `claude-<agent>`.

Stable Secret names, also for #382, #383 and #442: store `agentdev-platform-secrets` for other ExternalSecrets.

Why the Reloader beta: owner decision on #381, Reloader v2 pinned. It runs scoped, acts only on workloads with an
explicit `secret.reloader.stakater.com/reload` annotation, and ignores ConfigMaps. Not annotated: `platform-db` (needs
`ALTER ROLE`, #382) and `executor-egress-proxy`. Agents restart through OnDelete and the drain.

Limits: no database password rotation in Postgres (#382); Flux is #388. Acceptance 3 is checked by
`scripts/e2e-secrets.sh` on kind (by hand, not in CI).
