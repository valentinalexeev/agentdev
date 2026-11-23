---
issue: 454
type: fix
section: Fixed
status: done
breaking: false
title: Duplicate risk id R110 in docs/security.md
components: [docs]
pr: 455
---
## Summary

`docs/security.md` had risk id R110 twice, so `scripts/check-risk-ids.sh` failed. The job header risk (#196) keeps
R110. The notification action risk (#347) is now R120.

## Description

Two risks had the same id R110: the job header risk (#196) and the notification action risk (#347). The check
`scripts/check-risk-ids.sh` fails on duplicate ids. The job header risk keeps R110, because other entries (for
example R112) refer to it. The notification action risk gets the next free id, R120. R119 is kept for #445.
The released `CHANGELOG.md` line for #347 now says "R108, R109 and R120 (#347; first numbered R110)".
