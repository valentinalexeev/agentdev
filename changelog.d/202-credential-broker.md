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
(Deployment `credential-broker`) adds the secret to requests for its https upstream
(`$AGENTDEV_BROKER_URL/s/<secret>/`), so the agent never holds the value; uses and refusals are SecurityEvents
(source `credential-broker`). Mode `env` (opt-in, `riskAccepted`) puts the value into the agent container, never
under a variable agentdev sets. Write-only API (`/api/secrets`) and a **Secrets** page in the console. kind:
`ansible-playbook infra/ansible/kind.yml -e @kind.vars.yml --tags secrets`. Docs: `docs/admin.md`, risks R128, R129.

## Upgrade

1. kind: run `--tags secrets` again after `--tags agents`, so the role `secrets_store` deploys the broker and the
   write-only API RBAC. GKE: nothing (the broker on GKE is #202 part 4).

## Description

- The broker checks the scope (project or agent, matched with the forge and the agent's repository) on each request,
  removes the agent's credential headers, redacts the value from response bodies (also binary), refuses encodings it
  did not ask for and dials public addresses only.
- Reserved variable names for mode `env` cover every rendered variable and loader and trust hooks; one expression is
  shared by the API and the CRD (`TestReservedEnvMatchesCRD`). The operator drops a shadowing secret and records
  `secret-env-reserved`. The forge sidecar removes mode `env` values from approval text.
- The API can create, update and delete value Secrets but never read them; it audits each change without the value.
- `scripts/check-tracked-artifacts.sh` also fails on tracked compiled executables (ELF, Mach-O, PE).
