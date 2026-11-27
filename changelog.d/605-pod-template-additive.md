---
issue: 605
type: task
section: Added
status: done
title: Unit test that a newer operator only adds to the agent pod template
components: [operator]
pr: 612
---
## Summary

The operator has a new unit test, `TestPodTemplateIsAdditive`, for the mixed-version rule (ADR #178). It compares the rendered agent pod with a golden from v1.3.0 and fails when a container, env var, volume or mount is removed or renamed. To regenerate the golden, run `go test -update` (steps in `images/operator/compat_test.go`).
