// Assistant text of the Chat page as formatted text (#492). Each node type of markdown.ts maps to one fixed element
// below. No link target, image source, event handler or raw HTML comes from the text: React escapes every string.
// test/markdown.test.ts fails if this file gets one of those attributes.
import { Fragment, type ReactNode } from 'react'
// full file names: markdown.ts and Markdown.tsx differ only in case on case-insensitive file systems
import { MAX_CHARS, parse, type Block, type Inline } from './markdown.ts'

// a heading in a message ranks below the page and thread headings
const HEADINGS = ['h3', 'h4', 'h5', 'h6', 'h6', 'h6'] as const

function inl(c: Inline[]): ReactNode[] {
  return c.map((n, i) => {
    switch (n.t) {
      case 'text': return <Fragment key={i}>{n.v}</Fragment>
      case 'code': return <code key={i}>{n.v}</code>
      case 'strong': return <strong key={i}>{inl(n.c)}</strong>
      case 'em': return <em key={i}>{inl(n.c)}</em>
    }
  })
}

function blk(b: Block[]): ReactNode[] {
  return b.map((n, i) => {
    switch (n.t) {
      case 'p': return <p key={i}>{inl(n.c)}</p>
      case 'h': { const H = HEADINGS[n.level - 1] ?? 'h6'; return <H key={i}>{inl(n.c)}</H> }
      case 'pre': return <pre key={i}><code>{n.v}</code></pre>
      case 'quote': return <blockquote key={i}>{blk(n.c)}</blockquote>
      case 'hr': return <hr key={i} />
      case 'list': {
        // a tight item (one paragraph) shows its text without a paragraph margin
        const items = n.items.map((it, k) => <li key={k}>{it.length === 1 && it[0].t === 'p' ? inl(it[0].c) : blk(it)}</li>)
        return n.ordered ? <ol key={i} start={n.start}>{items}</ol> : <ul key={i}>{items}</ul>
      }
      case 'table': return (
        <div key={i} className="agd-md-table">
          <table>
            <thead><tr>{n.head.map((c, k) => <th key={k} style={{ textAlign: n.align[k] || undefined }}>{inl(c)}</th>)}</tr></thead>
            <tbody>{n.rows.map((r, j) => <tr key={j}>{r.map((c, k) => <td key={k} style={{ textAlign: n.align[k] || undefined }}>{inl(c)}</td>)}</tr>)}</tbody>
          </table>
        </div>
      )
    }
  })
}

export function Markdown({ text }: { text: string }) {
  if (text.length > MAX_CHARS) return <div className="agd-msg-text agd-plain">{text}</div>
  return <div className="agd-msg-text agd-md">{blk(parse(text))}</div>
}
