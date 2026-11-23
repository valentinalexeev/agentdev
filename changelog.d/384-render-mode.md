---
issue: 384
type: feature
section: Added
status: done
breaking: false
title: Render mode for a GitOps engine
components: [ansible, terraform]
pr: 449
---
## Summary

`infra/ansible/render.yml` writes the manifests of one instance to a directory, with no cluster or Secret Manager
access and no secret values (`SECRETS.md` lists the Secret names). `deploy.yml` applies the same objects
(`tasks/render_objects.yml`) and runs the same value checks (`tasks/checks.yml`). New Terraform output `platform` and
instance value `platform` (`egress.fqdn`, `ingress.gateway_api`, `ingress.certificate_domains`, `kube_api_ips`) replace
the reads of the cluster. The roles `platform`, `authentik`, `authentik_sso` and `api` are not rendered yet (#447).
Docs: `docs/admin.md`, "Render the manifests for a GitOps engine".

## Upgrade

No new step to run: apply `deploy.yml` as usual. Without `platform` the behaviour is the same. To use the render mode,
re-apply `infra/terraform` and copy `terraform output platform` into `ansible/instance.yml`. The order of some tasks in
`deploy.yml` changed:
1. All reads of Secret Manager come first: platform, agent, executor, Gitea wake poll and GitHub App secrets, the
   forge proxy credential, and the Claude proxy signing key and agent keys (made once, as before).
2. A new check reads the FQDNNetworkPolicy and Gateway API CRDs, when the values need them. It stops with a clear
   message, also under `--check`.
3. The platform namespace is now made together with the agents namespace, before the CRDs (before: after the CRDs).
4. The `webhook-router` Secret is a separate task, before the router objects.

The objects themselves are the same. Re-run: `deploy.yml` only.

## Description

Owner decision on #384: option (a), a render mode in Ansible, so a GitOps engine (Flux, Argo CD) can apply the
manifests of an instance from git.

- One renderer: `tasks/render_objects.yml`. `render.yml` writes its objects to `render_dir`; `deploy.yml` applies the
  same objects. The Secrets stay in `deploy.yml`, so the rendered output never holds a secret value.
- `tasks/checks.yml` holds the value checks of `deploy.yml` (labels, executors, approvals, budget, wake source). Both
  playbooks run them.
- `deploy.yml` and the role `ingress` do not read the cluster to select a template. The FQDNNetworkPolicy objects
  follow `platform.egress.fqdn` (default from `egress_fqdn_policy`, as before), and the Gateway follows
  `platform.ingress.gateway_api` (default `true`). The preflight (`crds`) still compares them with the cluster.
- Role `ingress`: `tasks/render.yml` (render only). The certificate check uses `platform.ingress.certificate_domains`
  when set.
- Output order: numbered files, CRDs and namespaces first. No hooks, no install-time lookups.
- Fix found by the kind apply of the rendered manifests: an executor without `executors[].path` no longer renders
  `path: ""`, which the Executor CRD refuses.

Checks: `tests/render_mode.yml` (FQDN on and off, Gateway, own Claude Secret, executors, no `Secret` in the output,
refusals before any file is written; renders `examples/deployment/gcp-gke` and runs `kubeconform` and `gitleaks`).

Limits: the roles `platform`, `authentik`, `authentik_sso` and `api` are rendered by #447. An OCI artifact of the
rendered output (point 6 of #384) is not done.
