---
issue: 516
type: fix
section: Fixed
status: done
breaking: false
title: The web UI keeps the view choice of Work, Agents, Approvals and Today
components: [ui]
---
## Summary

The console keeps the last view in this browser, like the theme and the project scope: Board/Matrix or List on
**Work** (`agd-work-view`), the filter on **Agents** (`agd-agents-filter`), the tab on **Approvals**
(`agd-approvals-tab`) and the groups on **Today** (`agd-today-by`). Blocked storage falls back to the default.
Docs: `docs/user-guide.md`, "Control agents in the web UI".
