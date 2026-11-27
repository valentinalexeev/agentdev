# 0009: Lean cluster defaults ship in 1.1.0 (SemVer exception)

Status: accepted (owner, "accept (b)", 2026-11-16, on #392)

## Context

agentdev follows strict SemVer (`CLAUDE.md` §7). The Terraform module (`infra/terraform`) is public interface frozen
at 1.0.0, so a changed default that alters the behaviour of an existing instance needs a MAJOR release.

#208 (PR #210, `feat(terraform)!: lean cluster add-ons by default`) merged into `main` after `v1.0.0` and is in the
1.1 line. It changes four defaults of the module:

| Input | 1.0 | 1.1 |
|---|---|---|
| `enable_managed_prometheus` | on | `false` |
| `enable_filestore_csi` | on | `false` |
| `monitoring_components` | GKE default | `["SYSTEM_COMPONENTS"]` (no kube-state-metrics) |
| load balancer and Gateway API (`gateway_enabled`) | always on | `null`: on only with `public_hosts` |

A 1.0.x instance that pins `v1.1.x` and runs `terraform apply` without new inputs gets these add-ons removed in place
(no node pool is replaced). The independent review of the release PR #419 flagged this as a MAJOR change in a minor.

## Decision

The owner accepts this as a recorded exception: 1.1.0 ships with the lean defaults. It is **not** a precedent; every
other change to the frozen interface still follows `CLAUDE.md` §7.

Conditions, all in the 1.1.0 release notes:

1. The `### Upgrade` part of the 1.1.0 CHANGELOG section marks the change **BREAKING** and lists the four inputs to
   keep the 1.0 cluster: `enable_managed_prometheus = true`, `enable_filestore_csi = true`,
   `monitoring_components = null`, `gateway_enabled = true`.
2. The GitHub release of every 1.1.x tag says this at the top.
3. The upgrade step tells the operator to read `terraform plan` before `terraform apply`.

## Consequences

- An operator who reads the upgrade notes keeps the old cluster with four lines in `instance.tfvars`.
- An operator who does not read them and relies on Managed Prometheus, kube-state-metrics, a Filestore PVC or a
  Gateway published without `public_hosts` loses it on the next apply. `terraform plan` shows the in-place updates.
- New instances get the smaller, cheaper cluster by default (the goal of #208).

## Alternatives rejected

- Restore the 1.0 defaults on `release/1.1` and move the lean defaults to 2.0.0 (option (a) on #392, proposed by the
  agent). The owner chose the exception instead.
