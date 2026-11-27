---
issue: 178
type: docs
section: Added
status: done
title: ADR 0006, compatibility rule for mixed versions during a rollout
pr: 254
---
## Summary

ADR 0006 (proposed, #178): compatibility rule for mixed versions during a rollout. The internal interface (agent pod
to platform calls, pod template, CRD fields, state volume, Postgres schema) accepts the previous MINOR release and
changes additively; a breaking internal change takes two releases. `docs/admin.md` tells operators to upgrade one
MINOR step at a time. The `compat` CI job checks the rule.
