#!/usr/bin/env node
// Fails if a CSS class looks like an ad to content blockers (#423): cosmetic filters hide elements whose class
// starts with `ad-`, `ads-` or `advert`, which blanked the whole console. The UI prefix is `agd-`.
// Checks class selectors in the CSS under src/ and, with --dist, the built CSS in dist/, plus class tokens in
// className/class attributes, classList calls and querySelector strings in src/.
// Usage: node scripts/check-class-prefix.mjs [--dist]
import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

const ui = fileURLToPath(new URL('..', import.meta.url))
const bad = /^(ad-|ads-|advert)/i
const walk = (dir) => readdirSync(dir, { withFileTypes: true }).flatMap((e) =>
  e.isDirectory() ? walk(join(dir, e.name)) : [join(dir, e.name)])

const roots = [join(ui, 'src')]
if (process.argv.includes('--dist')) {
  if (!existsSync(join(ui, 'dist'))) { console.error('check-class-prefix: dist/ not found, build first'); process.exit(1) }
  roots.push(join(ui, 'dist'))
}
const hits = []
for (const file of roots.flatMap(walk)) {
  const text = readFileSync(file, 'utf8')
  const report = (cls) => { if (bad.test(cls)) hits.push(`${file.slice(ui.length)}: ${cls}`) }
  if (file.endsWith('.css')) {
    // Class selectors: a dot followed by an identifier, outside of numbers such as 0.5em.
    for (const m of text.matchAll(/(?<![\w-])\.(-?[A-Za-z_][\w-]*)/g)) report(m[1])
  } else if (/\.(tsx?|jsx?|mjs|html)$/.test(file)) {
    // Every string literal in a class context: className=, class=, classList.*(...), querySelector*(...).
    for (const m of text.matchAll(/(?:className|class)\s*=\s*\{?\s*(["'`])([^"'`]*)\1/g)) m[2].split(/\s+/).forEach(report)
    for (const m of text.matchAll(/classList\.\w+\(([^)]*)\)/g)) for (const s of m[1].matchAll(/["'`]([^"'`]*)["'`]/g)) report(s[1])
    for (const m of text.matchAll(/querySelector(?:All)?\(\s*(["'`])([^"'`]*)\1/g)) for (const c of m[2].matchAll(/\.([\w-]+)/g)) report(c[1])
    // Any quoted token that starts with the prefix (class names built in helpers and template literals).
    for (const m of text.matchAll(/["'`\s{]((?:ad-|ads-|advert)[\w-]*)/gi)) report(m[1])
  }
}
if (hits.length) {
  console.error(`check-class-prefix: ${hits.length} class name(s) that ad blockers hide (#423), use the agd- prefix:`)
  for (const h of hits) console.error(`  ${h}`)
  process.exit(1)
}
console.log('check-class-prefix: no ad-like class names')
