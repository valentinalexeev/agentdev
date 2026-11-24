---
issue: 506
type: security
section: Security
status: done
breaking: false
title: Security event for every auto mode classifier allow
components: [credproxy]
---
## Summary

`claude-proxy` emits one low-severity security event `classifier-allow` for every tool call that Claude Code's auto
mode classifier allows (`spec.permission.mode: auto`, #500), with the agent, the tool and the input (redacted,
clipped). The agent cannot suppress it; it can only create false events about itself. Docs: `docs/security.md` R121.

## Description

Hooks and the approval MCP server never see a classifier allow, so the proxy is the only source outside the agent
container (option A of #506). The proxy detects the classifier request by its system prompt (Claude Code 2.1.328)
and reads the verdict from the upstream reply, streaming or not. The agent name comes from the proxy key, never
from the request body. Requests and replies are forwarded unchanged; an unknown shape is logged once and gives no
event. Recheck the detection when the Claude Code version in the worker image changes.
