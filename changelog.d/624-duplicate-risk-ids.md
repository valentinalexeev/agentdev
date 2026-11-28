---
issue: 624
type: fix
section: Fixed
status: done
breaking: false
title: Duplicate risk ids R121 and R126 in docs/security.md
components: [docs]
---
## Summary

`docs/security.md` had risk ids R121 and R126 twice, so `scripts/check-risk-ids.sh` failed. The classifier risk
(#506) keeps R121, and the read-only toolchain mount risk (#499) is now R138. The forge interface risk (#236) keeps
R126, and the custom toolchain risk (#598) is now R137.

## Description

In each pair, the risk that came first keeps its id, and the newer row gets a new id (rule in `docs/security.md`).
R137 and R138 are the next free ids from the coordination list. The released note for #499 now says "R138 and R122
to R124 (R138 was first numbered R121)".
