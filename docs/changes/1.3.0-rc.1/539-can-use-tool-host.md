# Approval prompts use Claude Code's can_use_tool host protocol (#539)

Released in 1.3.0-rc.1 (2026-11-25). Type: feature. Section: Added. Components: worker. PR: #540.

## Summary

With approvals on, the worker runs Claude Code through `permission-host`, which answers Claude Code's `can_use_tool`
control requests by asking the forge sidecar. The approval request now gets Claude Code's decision reason and its
type, the blocked path, the suggested rule and the call's description (#503). `APPROVAL_HOST=off` keeps the MCP
prompt tool. Docs: `docs/admin.md`, "Turn on approvals".

## Description

`permission-host` (Node, in the worker image) starts Claude Code with `-p --input-format stream-json
--permission-prompt-tool stdio`, writes the prompt as the first user message, passes stdout through unchanged and
closes stdin after the first `result` event; the exit code is Claude's. Each `can_use_tool` request goes to the same
sidecar endpoint as before (`POST /mcp`, tool `approve`, job header), so the decision rules do not change. Any error
or unreadable answer is a deny; other control requests get an error response. Verified against the
control_request/control_response schema of Claude Code 2.1.328 and of 2.1.312, the version in the image (same fields).
