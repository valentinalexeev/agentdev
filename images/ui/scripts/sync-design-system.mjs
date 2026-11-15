#!/usr/bin/env node
// Copies the design system (docs/design-system: tokens.css, components.css, console.css, favicon.svg) into the UI
// build (#341). The UI has no colour values of its own: every colour comes from tokens.css.
// The Docker build context is images/ui only, so the copies are committed; `prebuild` checks that they match the
// source, and fails the build (and CI) if somebody edited one side only.
// Usage: node scripts/sync-design-system.mjs [--check]
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

const ui = fileURLToPath(new URL('..', import.meta.url))
const src = `${ui}../../docs/design-system/`
const files = [
  ['tokens.css', 'src/design-system/tokens.css'],
  ['components.css', 'src/design-system/components.css'],
  ['console.css', 'src/design-system/console.css'],
  ['favicon.svg', 'public/favicon.svg'],
]
const check = process.argv.includes('--check')
if (!existsSync(src)) {
  // Docker build: only images/ui is in the context; CI checked the copies before.
  console.log('sync-design-system: docs/design-system not found, using the committed copies')
  process.exit(0)
}
let stale = 0
for (const [from, to] of files) {
  const want = readFileSync(src + from, 'utf8')
  const have = existsSync(ui + to) ? readFileSync(ui + to, 'utf8') : ''
  if (want === have) continue
  if (check) { console.error(`${to} differs from docs/design-system/${from}: run npm run sync-design-system`); stale++; continue }
  mkdirSync(`${ui}${to.slice(0, to.lastIndexOf('/'))}`, { recursive: true })
  writeFileSync(ui + to, want)
  console.log(`copied docs/design-system/${from} to ${to}`)
}
process.exit(stale ? 1 : 0)
