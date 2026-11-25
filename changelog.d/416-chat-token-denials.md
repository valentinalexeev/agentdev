---
issue: 416
type: task
section: Changed
status: done
breaking: false
title: The security e2e checks more denials for the chat Job token
components: [e2e]
---
## Summary

`scripts/e2e-gke-security.sh --only chat` now checks that `agentdev-api-chat-jobs` denies a chat Job whose projected
`claude-proxy` token lives other than 600 seconds, or is mounted in `forge` or the `state-init` clone step. A control
case checks that the right token shape is allowed. No policy change. docs/e2e-security.md.
