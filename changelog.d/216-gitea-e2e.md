---
issue: 216
type: task
section: Added
status: done
title: Gitea section in the security e2e; change request and new-commit check
components: [scripts]
pr: 289
---
## Summary

`scripts/e2e-gke-security.sh` has a `gitea` section (#216). It does the steps of the `github` section on a Gitea
repository: an issue assigned to the agent, the PR with the owner as reviewer and the label `agent:<name>`, a
change-request review, the agent's reply and a new commit. The `github` section now also sends a change request
and waits for a new commit, not only a comment. Use `--gitea OWNER/REPO --owner LOGIN [--gitea-url URL]` with
`E2E_GITEA_TOKEN` or `FORGE_ADMIN_USER`/`FORGE_ADMIN_PASSWORD` (`docs/e2e-security.md`). No step to re-run.
