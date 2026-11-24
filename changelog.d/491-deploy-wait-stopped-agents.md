---
issue: 491
type: fix
section: Fixed
status: done
title: The GKE deploy no longer waits 600 s for agents that are stopped
components: [ansible]
---
## Summary

`deploy.yml` waits only for the pods of agents whose StatefulSet has replicas above 0. When all agents are stopped
(scale to zero), the wait is skipped and the deploy goes on at once. Before, the task waited its full 600 s timeout
and then passed without a check. The router wait does not change.
