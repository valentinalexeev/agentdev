---
issue: 556
type: task
section: Changed
status: done
breaking: false
title: The compat e2e runs the API and chat-policy denial checks nightly and on demand
components: [ci]
---
## Summary

The nightly compat run and manual runs (input `api`, on by default) now deploy the PR-head API with `COMPAT_API=1`
and run the console chat NetworkPolicy and admission denial checks (`e2e-gke-security.sh --kind --only chat`),
including the #549 token-policy cases. A manual run with `real_model` and `api` runs the API checks in the real-model
pass. A failed API pass no longer skips the nightly real-model run. Per-PR compat runs are unchanged.
