---
issue: 547
type: removal
section: Removed
status: done
breaking: false
title: scripts/mcp-sync.sh is removed; the bootstrap applies an McpProject
components: [scripts]
---
## Summary

`scripts/mcp-sync.sh` is removed (deprecated since 1.2.0, #383). `scripts/bootstrap-project.sh` now applies an
`McpProject` for a specification with `mcp:` and waits until the operator reports it `Ready` before it deploys the
agents. A specification without `mcp:` removes the `McpProject` that an earlier bootstrap made. The allow-list is the
operator's `mcp_allowed_hosts`. Docs: `docs/admin.md`, "Add MCP servers to a project"; `docs/troubleshooting.md`, MCP tools.
