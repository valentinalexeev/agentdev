---
issue: 613
type: task
section: Changed
status: done
title: The pod template compatibility test covers optional features and the state volume claim
components: [operator]
---
## Summary

`TestPodTemplateIsAdditive` has a second fixture agent with a GitHub forge, the egress proxy, the Claude credential proxy, approvals, agent egress hosts and the Go and Rust toolchains (golden `pod-contract-v1.3.0-full.txt`). The pod contract now also lists the names of the volume claim templates, so the test fails when a newer operator removes or renames the `state` claim.
