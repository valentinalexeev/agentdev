// Options of react-markdown for assistant text in the Chat page (#492). react-markdown with remark-gfm parses the
// text; no rehype plugin is used, so raw HTML is never parsed as HTML. Before rendering, safeText() turns every node that could load or open something into
// text: raw HTML shows as its source, a link as its text and its address, an image as its alt text and its address,
// and reference definitions and footnotes as their source. allowedElements is the closed list of elements the chat
// shows (anything else is unwrapped to its text), and urlTransform drops every address as a second guard. So nothing
// in model text becomes a link, a button, an image or a request of the browser. test/markdown.test.ts checks this.
import type { Options } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { Component, createElement, type ReactElement, type ReactNode } from 'react'
import ReactMarkdown from 'react-markdown'

// longer text is shown plain (Markdown.tsx)
const MAX_CHARS = 100_000

// the part of an mdast node that safeText() reads and writes
type MdNode = {
  type: string
  value?: string
  url?: string
  alt?: string | null
  children?: MdNode[]
  position?: { start: { offset?: number }; end: { offset?: number } }
}

// parents whose children are blocks: a node turned into text there needs a paragraph
const FLOW = new Set(['root', 'blockquote', 'listItem', 'footnoteDefinition'])
// nodes shown as their source text
const AS_SOURCE = new Set(['html', 'definition', 'linkReference', 'imageReference', 'footnoteReference', 'footnoteDefinition'])

function source(n: MdNode, src: string): string {
  const a = n.position?.start.offset, b = n.position?.end.offset
  return a === undefined || b === undefined ? (n.value ?? '') : src.slice(a, b)
}

function walk(parent: MdNode, src: string): void {
  if (!parent.children) return
  const flow = FLOW.has(parent.type)
  parent.children = parent.children.flatMap((n): MdNode[] => {
    if (AS_SOURCE.has(n.type)) {
      const t = { type: 'text', value: source(n, src) }
      return [flow ? { type: 'paragraph', children: [t] } : t]
    }
    if (n.type === 'image') return [{ type: 'text', value: `${n.alt ?? ''} (image: ${n.url ?? ''})` }]
    walk(n, src)
    if (n.type === 'link') {
      // an autolink (<https://…> or a bare www. address) shows as typed; [text](address) as text (address)
      const s = source(n, src)
      if (!s.startsWith('[')) return [{ type: 'text', value: s }]
      return [...(n.children ?? []), { type: 'text', value: ` (${n.url ?? ''})` }]
    }
    return [n]
  })
}

function safeText() {
  return (tree: MdNode, file: { value: unknown }) => walk(tree, String(file.value))
}

export const options: Readonly<Options> = {
  remarkPlugins: [remarkGfm, safeText],
  allowedElements: [
    'p', 'br', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'strong', 'em', 'code', 'pre', 'blockquote', 'ul', 'ol', 'li',
    'table', 'thead', 'tbody', 'tr', 'th', 'td', 'hr',
  ],
  unwrapDisallowed: true,
  urlTransform: () => '',
  // a heading in a message ranks below the page and thread headings
  components: { h1: 'h3', h2: 'h4', h3: 'h5', h4: 'h6', h5: 'h6', h6: 'h6' },
}

// Deeply nested text (about 1,000 levels of `>` or list markers, a few thousand characters) overflows the stack of the
// react-markdown pipeline and a 5,000 level list takes seconds first (#494). tooDeep() is a cheap linear check before
// parsing: per line it counts the leading quote and list markers and the indentation before them (one level per 2
// spaces, a tab is 4 spaces). A line above MAX_DEPTH levels shows the message as plain text (Markdown.tsx).
export const MAX_DEPTH = 32

// a space, a tab, a line end or the end of the text (NaN)
const blank = (c: number) => c === 32 || c === 9 || c === 10 || c === 13 || Number.isNaN(c)

export function tooDeep(text: string): boolean {
  let depth = 0, indent = 0, start = true
  for (let i = 0; i < text.length; i++) {
    const c = text.charCodeAt(i)
    if (c === 10) { depth = 0; indent = 0; start = true; continue }
    if (!start) continue
    // indentation counts before the first marker; spaces after a marker only separate it from the next one
    if (c === 32) indent += depth ? 0 : 1
    else if (c === 9) indent += depth ? 0 : 4
    else if (c === 62) depth++ // >
    else if ((c === 45 || c === 42 || c === 43) && blank(text.charCodeAt(i + 1))) depth++ // - * + then a blank
    else if (c >= 48 && c <= 57) {
      // an ordered list marker: digits then . or )
      let j = i
      while (j < text.length && text.charCodeAt(j) >= 48 && text.charCodeAt(j) <= 57) j++
      const d = text.charCodeAt(j)
      if ((d === 46 || d === 41) && blank(text.charCodeAt(j + 1))) { depth++; i = j } else start = false
    } else start = false
    if (depth + (indent >> 1) > MAX_DEPTH) return true
  }
  return false
}

type BoundaryProps = { text: string; children?: ReactNode }

// Error boundary around the Markdown renderer (#494): if rendering throws anyway, the message shows as plain text
// instead of React unmounting the whole console.
export class PlainOnError extends Component<BoundaryProps, { failed: boolean }> {
  state = { failed: false }
  static getDerivedStateFromError(): { failed: boolean } { return { failed: true } }
  render(): ReactNode {
    return this.state.failed ? plain(this.props.text) : this.props.children
  }
}

const plain = (text: string) => createElement('div', { className: 'agd-msg-text agd-plain' }, text)

// The element of one assistant message (Markdown.tsx): plain text when the text is too long or too deeply nested,
// otherwise react-markdown with the options above inside PlainOnError.
export function markdownElement(text: string): ReactElement {
  if (text.length > MAX_CHARS || tooDeep(text)) return plain(text)
  return createElement(PlainOnError, { text },
    createElement('div', { className: 'agd-msg-text agd-md' }, createElement(ReactMarkdown, options, text)))
}
