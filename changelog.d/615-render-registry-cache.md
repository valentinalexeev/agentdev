---
issue: 615
type: feature
section: Added
status: done
title: render.yml renders the registry cache for GitOps instances
components: [ansible, terraform]
---
## Summary

`render.yml` renders the registry cache when `registry_proxy` is `cluster` or `artifact_registry`. Before, it refused
these values. The file `registry-cache.yaml` has the gateway, the caches, the NetworkPolicies, the ConfigMap
`registry-proxy` and, with `platform.egress.fqdn: gke`, the FQDNNetworkPolicy `registry-cache`. With
`external_secrets.mode: external`, the devpi root password comes from the new Secret Manager entry
`registry-cache-pypi`, which `infra/terraform` makes. Docs: `docs/admin.md`, "Render the manifests for a GitOps engine".

## Upgrade

With `external_secrets = true`, the next `terraform apply` adds the generated secret `registry-cache-pypi` (one
`random_id`, one secret, one version). Apply it before you render with `registry_proxy: cluster`.

## Description

The role `registry_cache` has a `tasks/render.yml`, the same pattern as the roles of #447. Its value check is now
`tasks/checks.yml`, and the two egress policies are templates, so `deploy.yml` and `render.yml` give the same objects.
`deploy.yml` still makes the devpi password Secret in the cluster. The CEL policies of the agents namespace refuse
an ESO generator, so render mode reads the password from Secret Manager.
