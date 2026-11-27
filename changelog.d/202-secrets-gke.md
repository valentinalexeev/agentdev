---
issue: 202
type: feature
section: Added
status: done
breaking: false
title: Project secrets on GKE (Secret Manager store, write-only API account, credential broker)
components: [terraform, ansible, api]
pr: 274
---
## Summary

Project secrets on GKE (#202 part 4), on the platform secrets setup of #381. Terraform `project_secrets` (default
`true` with `external_secrets`) adds two accounts limited by IAM condition to `agentdev-ps-*`: the ESO reader
`agentdev-sm-projsecrets` and the API's write-only `agentdev-api-secrets`. With `project_secrets` in `instance.yml`
(from `terraform output project_secrets`) the playbook adds the store `agentdev-secrets` and the credential broker.
Docs: `docs/admin.md`, "Set up the secret store (GKE)"; R113.

## Upgrade

1. Use `external_secrets.mode: external` (#381) first. Then `terraform apply` (the automation account gets
   `roles/iam.roleAdmin` for the custom roles), copy `terraform output project_secrets` into `instance.yml` as
   `project_secrets`, and run the playbook.

## Description

- The API writes values with custom roles without `secretmanager.versions.access`. Create is a separate role whose
  condition also admits the project, because Secret Manager checks create on the parent. Rotation destroys the
  previous version.
- An admission policy refuses any workload that runs as `agentdev-secrets-reader`.
- ESO and the CEL policies come from the role `external_secrets`; #274's own ESO install is dropped.
