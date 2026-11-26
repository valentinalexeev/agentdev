---
issue: 569
type: fix
section: Fixed
status: done
title: e2e-compat.sh no longer fails on an empty SSO argument list under bash 3.2
components: [scripts]
pr: 584
---
## Summary

`scripts/e2e-compat.sh` expands an empty `sso_args` array safely under `set -u`, so the compat run works with the
bash 3.2 of macOS when no SSO arguments are set.
