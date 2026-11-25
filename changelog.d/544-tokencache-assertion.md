---
issue: 544
type: task
section: Fixed
status: done
title: credproxy token cache test checks the cache hit again
components: [credproxy]
---
## Summary

The credproxy positive token cache test no longer has a vacuous assertion (staticcheck SA4000). It now fails if a repeated verify skips the cache.
