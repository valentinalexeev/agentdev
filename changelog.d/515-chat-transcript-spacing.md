---
issue: 515
type: fix
section: Fixed
status: done
breaking: false
title: More space in the chat transcript
components: [ui]
---
## Summary

The chat transcript (`/chat`) has more space. Items have a gap of `space-5` (24px), not `space-4` (16px). The
untrusted text block (`agd-untrusted`) has a left padding of `space-5`, which keeps 18px between the hatched edge and
the text. Both values come from the spacing scale in `tokens.css`.
