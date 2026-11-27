---
issue: 602
type: feature
section: Added
status: done
breaking: false
title: Security page event details drawer; the refused host in the egress event log
components: [ui, operator]
pr: 587
---
## Summary

On the Security page, click an event's time to open a drawer with all its fields: time (local and UTC), agent, severity,
source, rule, the refused host for egress events, the job (with a link), schema and the action as untrusted plain text.
The operator's `security event` log line now adds `host=` for egress events.
