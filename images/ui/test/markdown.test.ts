// Tests of the safe Markdown rendering of the Chat page (#492). Run: npm test (node --test).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import ReactMarkdown from 'react-markdown'
import { options } from '../src/markdown.ts'

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

test('the renderer has no way to make a link, an image, raw HTML or a handler', () => {
  for (const f of ['../src/Markdown.tsx', '../src/markdown.ts']) {
    const src = readFileSync(new URL(f, import.meta.url), 'utf8')
    for (const bad of ['dangerouslySetInnerHTML', 'rehype-raw', 'skipHtml', '<a ', '<img', '<iframe', 'onClick', 'innerHTML']) {
      assert.ok(!src.includes(bad), `${f} contains ${bad}`)
    }
  }
})
