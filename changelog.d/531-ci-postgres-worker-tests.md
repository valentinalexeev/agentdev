---
issue: 531
type: task
section: Changed
status: done
breaking: false
title: CI runs the Postgres API tests and every worker test file
components: [ci]
---
## Summary

CI starts a Postgres 16 service for the API job, so the `TestPg*` store tests run (they skipped before), and it
fails when one of them is skipped. The worker image job runs every `images/worker/test/*.test.sh` instead of a fixed
list. `runlog.test.sh` ran for the first time and needed a fix for jq 1.6, which is in the worker image.
