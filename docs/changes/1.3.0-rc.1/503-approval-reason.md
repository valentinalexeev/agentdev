# Approval requests explain why the agent asks (#503)

Released in 1.3.0-rc.1 (2026-11-25). Type: feature. Section: Added. Components: forge. PR: #536.

## Summary

An approval request comment now says why it exists. **Why it asks** comes from the forge sidecar (no allow rule in
`CLAUDE_TOOLS`, the protected paths of a push, the cost soft limit). **Reason given by Claude Code and the agent**
quotes the call's `description` and, when Claude Code sends them, the decision reason, its type, the blocked path and
the suggested rule. Docs: `docs/user-guide.md`, "Answer an approval request from an agent".

## Description

Both fields are also in the `AGENTDEV_APPROVAL_REQUEST` line (`guard`, `reason`); they are not part of the content
hash, so a decision still binds to the tool and its input only. The reason is untrusted: ANSI escapes and control
characters are removed, secrets are redacted, it is clipped to 1500 bytes and quoted in a code block that it cannot
close, and it never decides anything. Claude Code 2.1.x sends only `tool_name`, `input` and `tool_use_id` to the MCP
prompt tool, so today the reason is mostly the Bash `description`. The `can_use_tool` host protocol (proposal step 1
of #503) would fill the other fields; it is not part of this change.
