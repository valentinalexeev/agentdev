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

obs-mcp `cloud_logging_query` refuses a filter with unbalanced parentheses, an unterminated string or a trailing
backslash before the API call (#250). Such a filter could close the tool's `(<filter>) AND timestamp>=...` wrapper
and drop the time bound.

## Description

The check skips parentheses inside `"..."` strings and honours backslash escapes inside and outside strings, as in
the Logging query language. Nested parentheses are counted to any depth; a `)` before its `(` is refused at once.
