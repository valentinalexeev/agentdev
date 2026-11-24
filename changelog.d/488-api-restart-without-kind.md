---
issue: 488
type: fix
section: Fixed
status: done
breaking: false
title: Redeploy on GKE no longer stops at the API restart when no Secret changed
components: [ansible]
---
## Summary

`deploy.yml` failed at `api : Restart the API after a Secret change` with `'api_load' is undefined` on every
non-kind redeploy without a Secret change, and the roles after `api` did not run. The workaround
`api_load: {changed: false}` in `instance.yml` can be removed.
