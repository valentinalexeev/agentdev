# Per-agent tool allow-list additions (spec.tools.allow) (#496)

Released in 1.3.0-rc.1 (2026-11-25). Type: feature. Section: Added. Components: operator, worker, ansible.

## Summary

`Agent` has `spec.tools.allow`: Claude Code rules added to the worker's tool allow-list for that agent only, for
example `Bash(npm ci:*)`. Set it with `agents[].tools.allow` (GKE) or `kind_agents_tools` (kind).

## Description

The operator passes the entries comma-joined as `CLAUDE_TOOLS_EXTRA` to the agent container only, and the worker
appends them to `CLAUDE_TOOLS`, the default list or the instance's (ADR 0010 D1). There is no replace: an agent's list
cannot be shrunk or swapped, and the deny-list stays. The operator refuses entries that are empty, longer than 200
characters, have a comma or a control character, or more than 50 entries (phase `invalid`). An agent without the field
gets no new environment, so its pod is not restarted on upgrade.

Not yet done: a security event for a call allowed only by an added entry (depends on #506), and the entries in the
console (#505).
