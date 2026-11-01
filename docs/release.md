# Release checklist (1.0.0)

For maintainers. This page lists the gates for cutting `v1.0.0` (milestone 1, tracking issue #125) and the steps to
cut any release. The versioning rules are in `CLAUDE.md` §7. The deployment side is in
[Private deployment](private-deployment.md).

Copy the checklist into the release PR description and tick each item with a link to the evidence (issue, PR, CI
run, e2e log). An unticked item blocks the release unless the owner records an accepted-risk or out-of-scope
decision on #125.

## 1. Milestone gates (#125 "Done when")

- [ ] **Deploy from a private repo:** a repo made from `examples/deployment/gcp-gke` runs `terraform apply`, then
      `infra/ansible/deploy.yml`, against an empty GCP project, with images at the release candidate tag. No fork, no
      manual patch, no shell step outside the documented procedure.
- [ ] **Workspace sign-in:** a user signs in to the UI with a Google Workspace account; members of the synced Workspace
      group are in `agentdev-admins` (#122, PR #134).
- [ ] **GitHub end to end:** an admin adds an agent for a GitHub repo in the UI; an issue assigned on GitHub becomes a
      PR through the GitHub App (#2, #55); a review reply gets a follow-up push; the owner merges.
- [ ] **Security model on GKE:** phases 1 to 3 and 5 pass the GKE e2e security checklist (#123): forge creds only in
      the sidecar, egress allow-list (#65), credential proxy (#66, #118), executors (#69), usage limits (#93),
      approvals on the forge (#67), security events and SIEM outputs (#70, #71).
- [ ] **Docs:** `install.md`, `admin.md`, `user-guide.md`, `troubleshooting.md` and `private-deployment.md` describe
      the GKE + GitHub + Workspace path end to end, in STE (`docs/writing.md`).
- [ ] **Workstream issues:** every issue in milestone 1 is closed, or moved out with an owner decision.

## 2. GKE e2e checklist

Run on a real GKE cluster made from the template, at the release candidate images.

- [ ] Terraform `plan` on the deployment repo shows no unexpected change after the first apply.
- [ ] `ansible-playbook <agentdev>/infra/ansible/deploy.yml -e @ansible/instance.yml --check --diff` is clean, then
      the real run succeeds; a second real run reports no changes for the secret and manifest tasks.
- [ ] `kubectl -n coding-agents get pods`: `webhook-router` and every `agent-<name>-0` are `2/2 Running`.
- [ ] `kubectl -n agentdev-platform rollout status deploy/agentdev-api` succeeds; the UI loads over HTTPS.
- [ ] `curl -sI https://<host>/` gives `200` with `strict-transport-security`; `https://<host>/status` gives `404`.
- [ ] The router `/status` lists every agent (command in `AGENTS.md` step 10).
- [ ] Issue to PR to review reply to merge, on GitHub (gate 3), with the webhook (not only polling).
- [ ] Every item of the security checklist in #123 passes, with the event visible on the Security page and at the
      SIEM output.
- [ ] Upgrade test: deploy the previous release, then upgrade to the candidate by the procedure in
      [Private deployment, "Upgrade the instance"](private-deployment.md#upgrade-the-instance). Agents keep working.
- [ ] Backup and restore test of the platform database (see
      [Private deployment, "Make a backup"](private-deployment.md#make-a-backup)).

## 3. Risk register review

- [ ] `scripts/check-risk-ids.sh` passes.
- [ ] Every risk in `docs/security.md` with severity `high` is `mitigated`, `fixed`, or has an accepted-risk decision
      by the owner (link in the register row).
- [ ] Risks added since the last release have an owner and an issue.
- [ ] `docs/security-model.md` phase status matches what #123 verified.

## 4. Images and pinning

- [ ] `images.yml` published `agentdev-worker`, `agentdev-router` and every other release image at `X.Y.Z` (no `v`),
      from the release tag commit. Nothing in the release refers to `:main` or `:latest`.
- [ ] Third-party images and charts (Postgres, authentik, ContextForge, gateway images) are pinned to a version, and
      where the manifests support it, to a digest.
- [ ] `examples/deployment/gcp-gke/terraform/main.tf` uses `ref=vX.Y.Z` and `ansible/instance.yml.example` uses
      `image_tag: "X.Y.Z"`, the same version.
- [ ] The private deployment repo used for the e2e run pins the same two values.

## 5. CHANGELOG and SemVer

- [ ] Every merged behaviour change has a line under `## [Unreleased]`; **BREAKING** entries have a migration step.
- [ ] `1.0.0` freezes the public interface (Terraform variables and outputs, Ansible instance vars, manifest
      template variables, image entrypoints and env vars, router and listener HTTP API). After it, a breaking change
      bumps MAJOR, not MINOR. Review the interface for names you do not want to keep.
- [ ] Deprecated inputs either removed in `1.0.0` (with a **BREAKING** note) or kept for all of `1.x`.

## 6. Cut the release

1. Branch `release/X.Y.Z` from `main`.
2. In `CHANGELOG.md`, move `## [Unreleased]` to `## [X.Y.Z] - YYYY-MM-DD` and add an empty `## [Unreleased]`.
3. Bump the pinned examples (`ref=vX.Y.Z`, `image_tag: "X.Y.Z"`). For `1.0.0`, also uncomment the #120 public entry
   point in `examples/deployment/gcp-gke/terraform/main.tf` (`public_hosts`, `cloud_armor` and `output "ingress"`),
   so the template matches `private-deployment.md`, which needs `public_hosts` and `terraform output ingress`.
4. Commit `chore(release): vX.Y.Z`, open the PR with this checklist, and request the owner's review.
5. After the owner merges, the owner (or the bot on explicit permission) pushes the tag:
   `git tag vX.Y.Z <merge commit> && git push origin vX.Y.Z`
6. Check that `images.yml` publishes the images at `X.Y.Z`, then make the GitHub release with the CHANGELOG section.
7. Upgrade the reference private deployment by its normal PR flow.
