# Infracost configuration

`/infracost.yml` pins the Terraform projects that the Infracost Cloud GitHub App and the `infracost` CLI estimate
(#183). The App runs on every pull request. It skips PRs that change no file in a pinned project.

| Project | Path | Inputs |
|---|---|---|
| `agentdev-infra` | `infra/terraform` | `example.tfvars`, `usage-2-3-agents.yml` |
| `agentdev-bootstrap` | `infra/terraform/bootstrap` | `example.tfvars`, `usage-2-3-agents.yml` |

## Usage profile

`usage-2-3-agents.yml` matches *Cost (rough guide)* in `infra/terraform/README.md`: one always-on `system` node,
one Spot `agents` node, Cloud NAT egress, an almost empty registry, the secrets and the state bucket.

Limits of the estimate:

- Infracost has no hours setting for GKE node pools. The profile counts the Spot agent node for the full month
  (upper bound). With agents active about 180 hours a month, the real cost of that node is about a quarter.
- The GKE cluster management fee (about $73) is shown, but the free tier credit covers one zonal cluster.
- `example.tfvars` has no `public_hosts`, so the static IP, load balancer and Cloud Armor are not counted.

Reading the result: Infracost shows about $200/month for `agentdev-infra` (CLI v2.18, europe-west4). Subtract the
cluster fee ($73) and about three quarters of the agent node ($66 to about $16): about $80, inside the $40 to $90
range of the cost guide.

## Run locally

```bash
infracost auth login     # once; the key stays in your home directory, never in git
infracost scan           # reads infracost.yml
infracost inspect --group-by resource
```
