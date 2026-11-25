---
issue: 507
type: task
section: Changed
status: done
breaking: false
title: The worker guardrail test no longer depends on APPROVALS in the environment
components: [worker]
---
## Summary

`images/worker/test/guard.test.sh` passes with `APPROVALS=true` and with `APPROVALS` unset. It now unsets at the start
every variable the loop code under test reads, and each check sets the ones it needs.
