---
issue: 519
type: fix
section: Changed
status: done
breaking: false
title: Faster Work board and agent drawer, the forge is read in parallel and in the background
components: [api]
---
## Summary

`GET /api/jobs` and the agent drawer's recent jobs no longer wait for the forge one repository after the other.
The API reads at most 8 repositories at the same time and keeps the forge data for 90 s (was 30 s). Older data, up
to 10 min, is sent at once and read again in the background.

## Description

Before, a cache miss called the forge twice per repository, in sequence, so 16 agents with one repository each
could mean 32 serial calls before the response. The cache TTL (30 s) was shorter than the Work board's poll (60 s),
so most polls took the slow path. Now `jobsOf` reads the repositories at the same time (bounded by a semaphore that
background refreshes share), each repository's two calls run in parallel, and a stale entry is served at once while
one background refresh per entry runs (stale-while-revalidate). The UI shows no "refreshing" mark: the data is at
most 10 min old, and in normal polling at most about 90 s.
