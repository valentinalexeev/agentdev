#!/usr/bin/env node
// Builds docs/design-system/tokens.css from docs/design-system/tokens.json (the single source of the tokens).
// Usage: node scripts/design-tokens.mjs [--check]   (--check fails if tokens.css is out of date)
import { readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const dir = fileURLToPath(new URL('../docs/design-system/', import.meta.url));
const t = JSON.parse(readFileSync(dir + 'tokens.json', 'utf8'));
const val = (v) => (v.startsWith('{') ? `var(--${v.slice(1, -1)})` : v);
const [first, ...rest] = t.color.themes.map((x) => x.id);
const themed = [...t.color.tokens, ...(t.shadow?.tokens ?? [])];
const decl = (theme) => themed.map((x) => {
  const v = typeof x.value === 'string' ? x.value : (x.value[theme] ?? x.value[first]);
  return `  --${x.name}: ${val(v)};`;
}).join('\n');
const plain = [...t.spacing.tokens, ...t.radius.tokens].map((x) => `  --${x.name}: ${x.value};`);
const fams = Object.entries(t.type.families).map(([k, v]) => `  --font-${k}: ${v};`);
const styles = t.type.groups.flatMap((g) => g.styles.map((s) => {
  const fam = s.family ?? g.family;
  return `.${s.name} { font-family: var(--font-${fam}); font-size: ${s.fontSize}; line-height: ${s.lineHeight}; font-weight: ${s.fontWeight};`
    + (s.letterSpacing ? ` letter-spacing: ${s.letterSpacing};` : '') + ' }';
}));
let css = `/* agentdev design system: generated from tokens.json by scripts/design-tokens.mjs. Do not edit. */\n`
  + `:root {\n${decl(first)}\n${plain.join('\n')}\n${fams.join('\n')}\n}\n`;
for (const th of rest) {
  css += `@media (prefers-color-scheme: ${th}) {\n  :root:not([data-theme="${first}"]) {\n${decl(th).replace(/^/gm, '  ')}\n    color-scheme: ${th};\n  }\n}\n`
    + `:root[data-theme="${th}"] {\n${decl(th)}\n  color-scheme: ${th};\n}\n`;
}
css += styles.join('\n') + '\n';
if (process.argv.includes('--check')) {
  if (readFileSync(dir + 'tokens.css', 'utf8') !== css) { console.error('tokens.css is out of date: run node scripts/design-tokens.mjs'); process.exit(1); }
} else writeFileSync(dir + 'tokens.css', css);
