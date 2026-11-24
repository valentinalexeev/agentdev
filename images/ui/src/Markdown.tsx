// Assistant text of the Chat page as formatted text (#492), rendered by react-markdown with the options of markdown.ts.
// No raw HTML, link target, image source or handler comes from the text. test/markdown.test.ts checks the output and
// fails if this file gets one of those.
import ReactMarkdown from 'react-markdown'
// full file names: markdown.ts and Markdown.tsx differ only in case on case-insensitive file systems
import { MAX_CHARS, options } from './markdown.ts'

export function Markdown({ text }: { text: string }) {
  if (text.length > MAX_CHARS) return <div className="agd-msg-text agd-plain">{text}</div>
  return <div className="agd-msg-text agd-md"><ReactMarkdown {...options}>{text}</ReactMarkdown></div>
}
