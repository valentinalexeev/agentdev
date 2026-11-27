---
issue: 202
type: feature
section: Added
status: done
breaking: false
title: Project secrets part 2 and 3, credential broker, mode env delivery and a write-only secrets API
components: [credproxy, operator, api, ui, forge, ansible]
pr: 272
---
## Summary

Project secrets reach agents (#202 parts 2 and 3). Mode `broker`: the credproxy image with `MODE=broker`
(Deployment `credential-broker`) adds the secret to requests for its https upstream (`$AGENTDEV_BROKER_URL/s/<secret>/`);
the agent never holds the value and authenticates with a projected token (audience `credential-broker`), not a key.
Mode `env` (opt-in, `riskAccepted`) puts the value into the agent container, never under a variable agentdev sets.
Write-only API (`/api/secrets`) and a **Secrets** page. kind: `kind.yml --tags secrets`. Docs: `docs/admin.md`, R128, R129.

## Upgrade

1. kind: run `--tags secrets` again after `--tags agents`, so the role `secrets_store` deploys the broker and the
   write-only API RBAC. GKE: nothing (the broker on GKE is #202 part 4).

## Description

- The broker has `tokenreviews create` only and no signing key. The chat Job policy admits the `broker-token`
  projection only with audience `credential-broker`, 600 s, path `token`, and only in the chat container.
- The broker checks the scope (project or agent, matched with the forge and the agent's repository) on each request,
  removes the agent's credential headers, redacts the value from response bodies (also binary), refuses encodings it
  did not ask for and dials public addresses only.
- Reserved variable names for mode `env` cover every rendered variable and loader and trust hooks; one expression is
  shared by the API and the CRD (`TestReservedEnvMatchesCRD`). The operator drops a shadowing secret and records
  `secret-env-reserved`. The forge sidecar removes mode `env` values from approval text.
- The API can create, update and delete value Secrets but never read them; it audits each change without the value.
- `scripts/check-tracked-artifacts.sh` also fails on tracked compiled executables (ELF, Mach-O, PE).
