// A safe Markdown subset for assistant text in the Chat page (#492). parse() returns a plain tree whose node types are
// the allowed elements only: paragraph, heading, bold, italic, inline code, code block, block quote, list, GFM table
// and horizontal rule. There is no node type for HTML, links or images: raw HTML stays text, and a link or an image
// becomes its text and its address as text. Markdown.tsx maps each node type to one fixed React element, so nothing
// in model text becomes a link, a button, an element the parser did not make, or a request of the browser.
// No library: a dependency would bring HTML, link and image nodes that we must then switch off again (and keep off).

export type Inline =
  | { t: 'text'; v: string }
  | { t: 'code'; v: string }
  | { t: 'strong'; c: Inline[] }
  | { t: 'em'; c: Inline[] }

export type Align = 'left' | 'center' | 'right' | ''

export type Block =
  | { t: 'p'; c: Inline[] }
  | { t: 'h'; level: number; c: Inline[] }
  | { t: 'pre'; lang: string; v: string }
  | { t: 'quote'; c: Block[] }
  | { t: 'list'; ordered: boolean; start: number; items: Block[][] }
  | { t: 'table'; align: Align[]; head: Inline[][]; rows: Inline[][][] }
  | { t: 'hr' }

// longer text is shown plain (Markdown.tsx): the parser stays fast on any input
export const MAX_CHARS = 100_000
// deeper nesting of quotes, lists and emphasis stays text
const MAX_DEPTH = 8
// a link text or address longer than this is not a link, so a text full of brackets stays linear
const MAX_LINK = 2000

const FENCE = /^( {0,3})(`{3,}|~{3,})(.*)$/
const FENCE_END = /^ {0,3}(`{3,}|~{3,})[ \t]*$/
const ATX = /^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$/
const SETEXT = /^ {0,3}(=+|-+)[ \t]*$/
const HR = /^ {0,3}([-*_])(?:[ \t]*\1){2,}[ \t]*$/
const QUOTE = /^ {0,3}> ?(.*)$/
const ITEM = /^( {0,3})([-*+]|\d{1,9}[.)])(?:([ \t]+)(.*))?$/
const DELIM = /^ {0,3}\|?[ \t]*:?-+:?[ \t]*(?:\|[ \t]*:?-+:?[ \t]*)*\|?[ \t]*$/
const PUNCT = /[!-/:-@[-`{-~]/
const SPACE = /\s/
const WORD = /[\p{L}\p{N}]/u

export function parse(text: string): Block[] {
  return blocks(text.replace(/\r\n?/g, '\n').split('\n'), 0)
}

const blank = (l: string) => l.trim() === ''

// the indent of a line in columns (a tab goes to the next multiple of 4)
function ind(l: string): number {
  let n = 0
  for (const ch of l) {
    if (ch === ' ') n++
    else if (ch === '\t') n += 4 - (n % 4)
    else break
  }
  return n
}

// removes up to n columns of indent
function dedent(l: string, n: number): string {
  let col = 0, i = 0
  while (i < l.length && col < n) {
    if (l[i] === ' ') col++
    else if (l[i] === '\t') col += 4 - (col % 4)
    else break
    i++
  }
  return (col > n ? ' '.repeat(col - n) : '') + l.slice(i)
}

function fence(l: string): RegExpMatchArray | null {
  const m = l.match(FENCE)
  // a backtick fence has no backtick in its info string
  return m && !(m[2][0] === '`' && m[3].includes('`')) ? m : null
}

function cells(l: string): string[] {
  let s = l.trim()
  if (s.startsWith('|')) s = s.slice(1)
  if (s.endsWith('|') && !s.endsWith('\\|')) s = s.slice(0, -1)
  const out: string[] = []
  let cur = ''
  for (let i = 0; i < s.length; i++) {
    if (s[i] === '\\' && s[i + 1] === '|') { cur += '|'; i++ } else if (s[i] === '|') { out.push(cur.trim()); cur = '' } else cur += s[i]
  }
  out.push(cur.trim())
  return out
}

const tableAt = (lines: string[], i: number) =>
  lines[i].includes('|') && i + 1 < lines.length && DELIM.test(lines[i + 1]) && cells(lines[i + 1]).length === cells(lines[i]).length

const ordered = (marker: string) => marker[0] >= '0' && marker[0] <= '9'

// true if line i starts a block that ends a paragraph
function interrupts(lines: string[], i: number): boolean {
  const l = lines[i]
  if (fence(l) || ATX.test(l) || HR.test(l) || QUOTE.test(l) || tableAt(lines, i)) return true
  const m = l.match(ITEM)
  // an empty item, or an ordered item that does not start at 1, does not end a paragraph
  return !!m && !!m[4]?.trim() && (!ordered(m[2]) || parseInt(m[2], 10) === 1)
}

function blocks(lines: string[], depth: number): Block[] {
  const out: Block[] = []
  let i = 0
  while (i < lines.length) {
    const l = lines[i]
    if (blank(l)) { i++; continue }
    let m: RegExpMatchArray | null

    if ((m = fence(l))) {
      const pad = m[1].length, mark = m[2], body: string[] = []
      i++
      while (i < lines.length) {
        const e = lines[i].match(FENCE_END)
        i++
        if (e && e[1][0] === mark[0] && e[1].length >= mark.length) break
        body.push(dedent(lines[i - 1], pad))
      }
      out.push({ t: 'pre', lang: m[3].trim().split(/\s+/)[0], v: body.join('\n') })
      continue
    }

    if ((m = l.match(ATX))) { out.push({ t: 'h', level: m[1].length, c: inline(m[2] ?? '') }); i++; continue }
    if (HR.test(l)) { out.push({ t: 'hr' }); i++; continue }

    if (depth < MAX_DEPTH && QUOTE.test(l)) {
      const body: string[] = []
      while (i < lines.length && (m = lines[i].match(QUOTE))) { body.push(m[1]); i++ }
      out.push({ t: 'quote', c: blocks(body, depth + 1) })
      continue
    }

    if (depth < MAX_DEPTH && (m = l.match(ITEM))) {
      const isOrdered = ordered(m[2]), delim = m[2].slice(-1)
      const same = (x: string) => {
        const im = x.match(ITEM)
        return !!im && !HR.test(x) && ordered(im[2]) === isOrdered && im[2].slice(-1) === delim
      }
      const items: Block[][] = []
      while (i < lines.length && same(lines[i])) {
        const im = lines[i].match(ITEM) as RegExpMatchArray
        const sp = im[3] ? ind(im[3]) : 1
        // the content column; 5 or more spaces after the marker are part of the content
        const w = im[1].length + im[2].length + (sp > 4 ? 1 : sp)
        const body = [sp > 4 ? ' '.repeat(sp - 1) + im[4] : im[4] ?? '']
        i++
        while (i < lines.length) {
          const c = lines[i]
          if (blank(c)) {
            // blank lines stay in the item when the next text is indented to its content
            let j = i
            while (j < lines.length && blank(lines[j])) j++
            if (j < lines.length && ind(lines[j]) >= w) { for (; i < j; i++) body.push(''); continue }
            break
          }
          if (ind(c) >= w) { body.push(dedent(c, w)); i++; continue }
          // a lazy continuation line of the item's paragraph
          if (!blank(body[body.length - 1]) && !interrupts(lines, i) && !ITEM.test(c)) { body.push(c.trim()); i++; continue }
          break
        }
        items.push(blocks(body, depth + 1))
        let j = i
        while (j < lines.length && blank(lines[j])) j++
        if (j < lines.length && same(lines[j])) i = j
        else break
      }
      out.push({ t: 'list', ordered: isOrdered, start: isOrdered ? parseInt(m[2], 10) : 1, items })
      continue
    }

    if (tableAt(lines, i)) {
      const head = cells(l)
      const align = cells(lines[i + 1]).map((c): Align =>
        c.startsWith(':') ? (c.endsWith(':') ? 'center' : 'left') : c.endsWith(':') ? 'right' : '')
      const rows: Inline[][][] = []
      i += 2
      while (i < lines.length && !blank(lines[i]) && !interrupts(lines, i)) {
        const r = cells(lines[i])
        rows.push(head.map((_, k) => inline(r[k] ?? '')))
        i++
      }
      out.push({ t: 'table', align, head: head.map((h) => inline(h)), rows })
      continue
    }

    // a paragraph: always takes its first line, so the loop moves on
    const para = [l.trim()]
    let heading = 0
    i++
    while (i < lines.length && !blank(lines[i])) {
      const s = lines[i].match(SETEXT)
      if (s) { heading = s[1][0] === '=' ? 1 : 2; i++; break }
      if (interrupts(lines, i)) break
      para.push(lines[i].trim())
      i++
    }
    const c = inline(para.join('\n'))
    out.push(heading ? { t: 'h', level: heading, c } : { t: 'p', c })
  }
  return out
}

// the start of the closing run of k delimiters ch after from, or -1
function closeAt(s: string, from: number, ch: string, k: number): number {
  for (let j = s.indexOf(ch, from); j !== -1; j = s.indexOf(ch, j)) {
    let e = j
    while (s[e] === ch) e++
    // a single closer is a run of 1, or of 3 and more (the end of bold italic)
    const fits = k === 2 ? e - j >= 2 : e - j !== 2
    if (fits && !SPACE.test(s[j - 1]) && !(ch === '_' && WORD.test(s[e] ?? ''))) return e - k
    j = e
  }
  return -1
}

// [text](address) at i, with s[i] === '['
function linkAt(s: string, i: number): { text: string; url: string; end: number } | null {
  let depth = 0, j = i
  for (; j < s.length && j - i <= MAX_LINK; j++) {
    if (s[j] === '\\') j++
    else if (s[j] === '[') depth++
    else if (s[j] === ']' && --depth === 0) break
  }
  if (depth !== 0 || s[j] !== ']' || s[j + 1] !== '(') return null
  let p = 0, k = j + 1
  for (; k < s.length && k - j <= MAX_LINK; k++) {
    if (s[k] === '\\') k++
    else if (s[k] === '(') p++
    else if (s[k] === ')' && --p === 0) break
    else if (s[k] === '\n') return null
  }
  if (p !== 0 || s[k] !== ')') return null
  const dest = s.slice(j + 2, k).trim()
  const angle = dest.match(/^<([^>]*)>/)
  return { text: s.slice(i + 1, j), url: angle ? angle[1] : dest.split(/\s+/)[0], end: k + 1 }
}

function inline(s: string, depth = 0): Inline[] {
  const out: Inline[] = []
  let buf = ''
  const flush = () => { if (buf) { out.push({ t: 'text', v: buf }); buf = '' } }
  const text = (v: string) => { buf += v }
  // a delimiter with no closer after one place has no closer after any later place either
  const failed = new Set<string>()
  let i = 0
  while (i < s.length) {
    const ch = s[i]
    if (ch === '\\' && i + 1 < s.length && PUNCT.test(s[i + 1])) { text(s[i + 1]); i += 2; continue }

    if (ch === '`') {
      let n = 1
      while (s[i + n] === '`') n++
      let end = -1
      if (!failed.has('`' + n)) {
        for (let j = s.indexOf('`', i + n); j !== -1;) {
          let e = j
          while (s[e] === '`') e++
          if (e - j === n) { end = j; break }
          j = s.indexOf('`', e)
        }
        if (end === -1) failed.add('`' + n)
      }
      if (end === -1) { text('`'.repeat(n)); i += n; continue }
      let v = s.slice(i + n, end).replace(/\n/g, ' ')
      if (v.length > 2 && v.startsWith(' ') && v.endsWith(' ') && v.trim()) v = v.slice(1, -1)
      flush()
      out.push({ t: 'code', v })
      i = end + n
      continue
    }

    if (ch === '[' || (ch === '!' && s[i + 1] === '[')) {
      const img = ch === '!'
      const r = linkAt(s, img ? i + 1 : i)
      if (r) {
        // a link or an image is its text and its address, as text: never an element that makes a request
        if (img || depth >= MAX_DEPTH) text(`${r.text}${r.url ? ` (${img ? 'image: ' : ''}${r.url})` : ''}`)
        else {
          flush()
          out.push(...inline(r.text, depth + 1))
          if (r.url) text(` (${r.url})`)
        }
        i = r.end
        continue
      }
      text(ch)
      i++
      continue
    }

    if (ch === '*' || ch === '_') {
      let n = 1
      while (s[i + n] === ch) n++
      const k = n >= 2 ? 2 : 1
      const canOpen = depth < MAX_DEPTH && !SPACE.test(s[i + n] ?? ' ') && !(ch === '_' && WORD.test(s[i - 1] ?? ''))
      const key = ch + k
      const end = canOpen && !failed.has(key) ? closeAt(s, i + k + 1, ch, k) : -1
      if (canOpen && end === -1) failed.add(key)
      if (end === -1) { text(ch); i++; continue }
      flush()
      const c = inline(s.slice(i + k, end), depth + 1)
      out.push(k === 2 ? { t: 'strong', c } : { t: 'em', c })
      i = end + k
      continue
    }

    text(ch)
    i++
  }
  flush()
  return out
}
