---
issue: 515
type: fix
section: Fixed
status: done
breaking: false
title: More space in the chat transcript and around the message box
components: [ui]
---
## Summary

The chat transcript (`/chat`) has more space. Items have a gap of `space-5` (24px), not `space-4` (16px). The
untrusted text block (`agd-untrusted`) has a left padding of `space-5`, which keeps 18px between the hatched edge and
the text. The message box (`agd-composer`) has more padding: `space-4` above and `space-5` below it, and `space-3`
around its button bar. On phones, the message box has `space-3` above and below and 16px at the sides, and it keeps
clear of the bottom safe area. All values come from the spacing scale in `tokens.css`.
