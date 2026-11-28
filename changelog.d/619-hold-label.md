---
issue: 619
type: feature
section: Added
status: in-progress
title: The label hold stops new work on one issue or PR; ADR 0012 records steering with native forge actions
components: [worker, process]
---
## Summary

An issue or PR with the label `hold` gets no new agent job and is not merged by the agent, on GitHub and Gitea. Remove
the label and the agent resumes on its next pass. The default process pack creates the label. Agents are steered with
native forge actions, not slash commands ([ADR 0012](docs/adr/0012-steer-agents-with-forge-actions.md)). Docs:
`docs/user-guide.md`, "Steer an agent with forge actions".

## Description

ADR 0012 maps each steering need to a forge action and its minimum permission, and keeps `/approve`, `/deny` and
`/approve-job` as the only commands. This change implements the `hold` row. The actor checks for content events, the
spec freeze, abandoning a closed issue's branch, priority order and the status comment are open (ADR 0012,
"Status of the implementation").
