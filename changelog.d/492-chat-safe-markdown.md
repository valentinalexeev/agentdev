---
issue: 492
type: feature
section: Added
status: done
breaking: false
title: Chat answers as safe Markdown
components: [ui]
---
## Summary

The console Chat page shows the answers of Claude as formatted Markdown: headings, lists, GFM tables, bold, italic,
code, quotes and rules. Raw HTML shows as text. Images and links show their text and address as text, so the
browser loads and opens nothing from model text. A **Plain text** toggle on each answer shows the exact text. Tool
input and output, permission requests and drafts stay plain text. Docs: `docs/user-guide.md`, risk R121 in
`docs/security.md`.

## Description

The parser (`images/ui/src/markdown.ts`) has no dependencies. Its tree has node types for the allowed elements only,
so a link, an image or HTML cannot appear in the output, also not by a later library update or option. A library
such as `react-markdown` makes these nodes and needs a filter that must stay correct. Unit tests:
`images/ui/test/markdown.test.ts`, run with `npm test` in `images/ui` (`node --test`).
