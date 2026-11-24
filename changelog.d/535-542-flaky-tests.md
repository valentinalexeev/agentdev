---
issues: [535, 542]
type: task
section: Changed
status: done
breaking: false
title: Two flaky Go tests in credproxy and forge are stable
components: [credproxy, forge]
---
## Summary

`TestClassifierGzipRequest` (credproxy) reads every response before asserting on the logged events. The forge
test fixture turns off git's detached automatic maintenance, which could still write into the mirror while the
test's temporary directory was removed. Both pass 50 of 50 runs under `go test -race -count=50`.
