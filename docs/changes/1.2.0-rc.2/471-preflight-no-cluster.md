# First deploy and resume after a deleted cluster work through the deployment CI (#471)

Released in 1.2.0-rc.2 (2026-11-23). Type: fix. Section: Fixed. Components: ansible, ci.

## Summary

The example deployment CI runs the checks that need no cluster before `infra` and the CRD checks in the new job
`preflight-cluster` after `infra`, so a first install or a resume after `gcloud container clusters delete` works on
both engines. `scripts/upgrade-check.sh` skips the cluster checks with a notice when the cluster is not found and has
`--phase no-cluster|cluster`. Docs: `docs/install.md`, `docs/troubleshooting.md`.

## Upgrade

1. Copy `.github/workflows/deploy.yml` from `examples/deployment/gcp-gke` of this release into the deployment
   repository. An existing cluster is checked as before.

## Description

Before, `preflight` ran `get-credentials` and the CRD checks, and `infra` needed `preflight`, so CI could not make
a missing cluster. `deploy` (Ansible) and `render` (Flux) now need `infra` and `preflight-cluster`; `verify` still
needs `render`. Only a clear "not found" answer skips the cluster checks; any other gcloud error runs them (and fails)
as before. With an agentdev release before this change, the template falls back to the old one-job preflight.
