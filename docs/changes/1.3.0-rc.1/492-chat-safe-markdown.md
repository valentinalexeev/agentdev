# Chat answers as safe Markdown (#492)

Released in 1.3.0-rc.1 (2026-11-25). Type: feature. Section: Added. Components: ui.

## Summary

The console Chat page shows the answers of Claude as formatted Markdown: headings, lists, GFM tables, bold, italic,
code, quotes and rules. Raw HTML shows as text. Images and links show their text and address as text, so the
browser loads and opens nothing from model text. A **Plain text** toggle on each answer shows the exact text. Tool
input and output, permission requests and drafts stay plain text. Docs: `docs/user-guide.md`, risk R125 in
`docs/security.md`.

## Description

The console uses `react-markdown` 10.1.0 with `remark-gfm` 4.0.1 (pinned, no `rehype-raw`). A remark step in
`images/ui/src/markdown.ts` turns raw HTML, links, images, reference definitions and footnotes into text. A closed
`allowedElements` list and a `urlTransform` that drops every address are a second guard. Unit tests:
`images/ui/test/markdown.test.ts`, run with `npm test` in `images/ui` (`node --test`).
