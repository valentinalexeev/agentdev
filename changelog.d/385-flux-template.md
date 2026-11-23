---
issue: 385
type: feature
section: Added
status: done
breaking: false
title: Install an instance with Flux
components: [ansible, deployment]
---
## Summary

The GKE deployment template has a Flux tree (`examples/deployment/gcp-gke/gitops/flux/`): a GitRepository and three
layers (infrastructure with ESO 2.14.0 and Reloader v2.0.0-beta.2, platform, agents) with `dependsOn`, prune and
drift correction, Flux on the system pool, and a fourth layer for Reloader after the platform layer, which makes
the namespaces that Reloader watches. With `DEPLOY_ENGINE=flux` the template workflow renders, pushes the branch
`gitops`, waits for Flux Ready (`scripts/gitops-status.sh`) and runs the smoke test (`infra/ansible/smoke.yml`).
Docs: `docs/admin.md`, "Install an instance with Flux".

## Upgrade

No change for an Ansible deployment: without the repository variable `DEPLOY_ENGINE`, the workflow runs `deploy.yml` as
before. To change to Flux, copy the new `.github/workflows/deploy.yml` and `gitops/`, then follow the procedure.

## Description

Owner decision on #385: Flux only for 1.2.0; Argo CD is a separate issue. The status adapter
(`scripts/gitops-status.sh --engine`) has one interface for both engines, so Argo CD adds one reader function.
`scripts/gitops-layout.py` splits the `render.yml` output into `rendered/platform` and `rendered/agents`, removes
StatefulSet `replicas`, merges objects that the render writes twice and refuses a Secret. Checks:
`infra/ansible/tests/gitops_flux.yml` (renders the example, `scripts/check-flux-tree.sh`: kustomize build and kubeconform
with the Flux CRD schemas, system pool, prune, dependsOn, pinned charts) and `scripts/test/gitops-test.sh`.
