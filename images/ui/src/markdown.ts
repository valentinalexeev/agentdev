// Options of react-markdown for assistant text in the Chat page (#492). react-markdown with remark-gfm parses the
// text; no rehype plugin is used, so raw HTML is never parsed as HTML. Before rendering, safeText() turns every node that could load or open something into
// text: raw HTML shows as its source, a link as its text and its address, an image as its alt text and its address,
// and reference definitions and footnotes as their source. allowedElements is the closed list of elements the chat
// shows (anything else is unwrapped to its text), and urlTransform drops every address as a second guard. So nothing
// in model text becomes a link, a button, an image or a request of the browser. test/markdown.test.ts checks this.
import type { Options } from 'react-markdown'
import remarkGfm from 'remark-gfm'

// longer text is shown plain (Markdown.tsx)
export const MAX_CHARS = 100_000

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
