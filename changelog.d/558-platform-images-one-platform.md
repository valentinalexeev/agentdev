---
issue: 558
type: fix
section: Fixed
status: done
breaking: false
title: The kind install of the platform images no longer fails on multi-arch images
components: [ansible]
---
## Summary

On kind with Docker's containerd image store (GitHub runners included), `kind.yml --tags mcp` failed at "Load the
platform images into kind" for `postgres:17.11` with `content digest ... not found`. The role `platform` now pulls
its images for the engine's platform and re-tags them through a one-line build, as the role `kind_agents` already did
(#378). Both roles share `infra/ansible/tasks/kind_pull_platform.yml` and `kind_pull_image.yml`.
