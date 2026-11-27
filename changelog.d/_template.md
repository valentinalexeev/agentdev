---
issue: 0                  # required: the issue number; for several, use "issues: [382, 388]" instead
type: feature             # required: feature | fix | task | security | docs | deprecation | removal
section: Added            # required: Added | Changed | Deprecated | Removed | Fixed | Security
status: done              # required: planned | in-progress | done | dropped
breaking: false           # optional: true needs a "## Upgrade" section
title: One line that names the change
components: [operator]    # optional
pr: 0                     # optional: remove the line if you do not know the PR number yet
---
## Summary

The changelog text: 1 to 6 lines. What changed for the user or operator, the setting names, the doc page.

## Upgrade

1. Optional. Numbered steps for the operator. Required when `breaking: true`.

## Description

Optional. The full description: motivation, design, limits. It does not go into CHANGELOG.md. At release it moves
to docs/changes/<version>/<this file name>.
