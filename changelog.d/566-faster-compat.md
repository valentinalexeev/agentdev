---
issue: 566
type: task
section: Changed
status: done
title: Shorter compat e2e (authentik in the background, 15 s idle timeout with the stub)
components: [scripts]
---
## Summary

`scripts/e2e-compat.sh` with `COMPAT_API=1` deploys Postgres and authentik in the background while the agents are
deployed and checked, and joins them before `--tags api`. The stubbed run uses a 15 s idle timeout (45 s with
`CLAUDE_REAL=1`, `IDLE_SECONDS` overrides). `COMPAT_SSO=0` skips the SSO stand-in and its sign-in check.
