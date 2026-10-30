# AGENTS.md: operating this agentdev instance

This repository is the **deployment** of one agentdev instance. The product lives in
`valentinalexeev/agentdev`; do not copy its code here, consume it by release tag.

- Every change is a PR. `main` is protected; merging applies it (`.github/workflows/deploy.yml`).
- Instance values only: `terraform/instance.tfvars`, `ansible/instance.yml`. No secrets in git:
  values live in Secret Manager.
- Upgrades: bump `ref=` in `terraform/main.tf` and `image_tag` in `ansible/instance.yml` to the same
  agentdev release in one PR; quote the relevant CHANGELOG entries in the PR description.
- Spending or irreversible actions (new projects, billing, deleting clusters or disks) need the owner's
  explicit approval in the PR. Never run `terraform destroy`.
- Product bugs go to agentdev's issue tracker; issues here are about running this instance
  (capacity, outages, secret rotation, upgrades).
