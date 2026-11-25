---
issue: 478
type: docs
section: Changed
status: done
breaking: false
title: docs/admin.md gives the 1.2.0 state of scripts/mcp-sync.sh
components: [docs]
---
## Summary

`docs/admin.md` no longer says that `scripts/mcp-sync.sh` works "until 1.2.0 ships". Since 1.2.0 the operator does the
MCP sync (#383). The script is deprecated and stays only for `scripts/bootstrap-project.sh`. Its removal is #547.
