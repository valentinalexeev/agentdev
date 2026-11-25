---
issue: 250
type: security
section: Security
status: done
breaking: false
title: obs-mcp refuses an unbalanced Cloud Logging filter
components: [operator]
pr: 268
---
## Summary

obs-mcp `cloud_logging_query` refuses a filter with unbalanced parentheses, an unterminated string, a newline in a
string, a line end other than LF or CRLF, or a trailing backslash before the API call (#250). Such a filter could
close the tool's `(<filter>) AND timestamp>=...` wrapper and drop the time bound.

## Description

The check skips parentheses inside `"..."` strings and honours backslash escapes inside and outside strings, as in
the Logging query language. A `--` comment outside a string runs to the end of the line and does not count. The
filter is wrapped as `(\n<filter>\n) AND timestamp>=...`, so a trailing comment cannot hide the time bound. The
Logging docs do not define which characters end a line, so a lone CR, VT, FF, U+0085, U+2028 and U+2029 are refused
anywhere, and only LF (or CRLF) ends a comment. Nested parentheses are counted to any depth; a `)` before its `(`
is refused at once.
