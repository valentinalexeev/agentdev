// Assistant text of the Chat page as formatted text (#492), rendered by react-markdown with the options of markdown.ts.
// No raw HTML, link target, image source or handler comes from the text. test/markdown.test.ts checks the output and
// fails if this file gets one of those. Very long or deeply nested text stays plain, and an error boundary shows
// plain text if rendering throws anyway (#494); markdownElement() in markdown.ts decides.
// full file names: markdown.ts and Markdown.tsx differ only in case on case-insensitive file systems
import { markdownElement } from './markdown.ts'

export function Markdown({ text }: { text: string }) {
  return markdownElement(text)
}
