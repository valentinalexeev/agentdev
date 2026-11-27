---
issue: 621
type: fix
section: Fixed
status: done
breaking: false
title: Worker tests that run inside an agent pod no longer push to the forge sidecar or kill the agent's Claude run
components: [worker]
---
## Summary

The worker tests no longer send requests to the forge sidecar of the agent pod that runs them. Before, `drain.test.sh` pushed its
fixture branch `agent/alice/wip/issue-7` to the sidecar, which caused a high security event. It also stopped the
agent's own Claude run with exit code 137. Each worker test now sets `FORGE_API` to a closed port. The new check
`images/worker/test/isolation.test.sh` makes sure that every worker test does this.

## Description

`drain.test.sh` runs the SIGTERM handler `on_term` of `agent-loop.sh`. It did not set `FORGE_API` and had no curl
stub, so inside an agent pod it used the pod's `FORGE_API=http://127.0.0.1:7070`. The handler pushed the saved work
in progress (#593) to the real sidecar, which refused the branch as `push-foreign-branch` (high severity), and the
operator deleted the pod. The same handler runs `pkill -KILL -x claude`, which killed the agent's Claude process:
the run ended with exit 137 and the reason `unknown`, because no worker code had killed it. In CI nothing listens
on that port and no `claude` process runs, so the tests passed. `stopterm.test.sh` had a curl stub but the same
`pkill`.

Now `drain.test.sh` and `stopterm.test.sh` stub `curl` and `pkill` in the loop process. They also check that the push and the kill reach
the stubs. Every worker test sets `FORGE_API` to `http://127.0.0.1:1` (or unsets it), also the tests whose curl
is a stub on `PATH` (`chat-console.test.sh`, `costlimit.test.sh`). `isolation.test.sh` fails when a test does not
set `FORGE_API`, sets it to port 7070, or runs `on_term` without the two stubs. The exit-137 runs on #589, #590 and
#593 probably had the same cause.
