---
issue: 557
type: fix
section: Fixed
status: done
breaking: false
title: The kind SSO check applies the SSO blueprint on the restarted authentik worker
components: [ansible]
pr: 562
---
## Summary

`kind.yml --tags authentik` (role `authentik_kind`) applied the SSO blueprint in the first Running worker pod after the
restart, which could be the old pod still terminating, without the `AGENTDEV_SSO_*` env. The blueprint then created no
source and the sign-in check failed with "source did not redirect to the stand-in (HTTP 404)". The role now waits for a
worker pod that is not being deleted and has the SSO env, and fails at the apply step when the source `agentdev-sso`
does not exist.
