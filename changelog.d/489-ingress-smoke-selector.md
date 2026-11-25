---
issue: 489
type: fix
section: Fixed
status: done
breaking: false
title: The ingress smoke test finds the forwarder pods
components: [ansible]
---
## Summary

The post-deploy smoke test built the label selector as the literal `key=value`, so it never found the forwarder pods
of `ingress.mode: external` and failed every deploy. It now uses one `key=value` per label of
`ingress.forwarder.pod_selector`. The workaround `ingress.smoke: false` can be removed.
