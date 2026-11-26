---
issue: 578
type: feature
section: Added
status: done
title: Opt-in merge of an agent's own pull request after a person approved its exact head commit
components: [forge, operator, worker]
pr: 580
---
## Summary

New field `spec.merge` on the `Agent` (`mode: off | gated`, `method: merge | squash | rebase`). The default is `off`:
the agent never merges, as before. With `mode: gated` (GitHub only), the forge sidecar merges the agent's own PR
(head `agent/<name>/*`) through `POST /merge` only when a person with write access approved the exact head commit,
all checks passed, no change is requested, no approval request of the agent is open, and protected paths have an
approval. The merge pins the head commit and writes an audit entry; a refusal is a security event.

## Description

The sidecar reads every fact from the forge with its own credential at the moment of the merge, never from the agent
(`images/forge/merge.go`). A PR that is not ready yet (a check is pending, an approval request is open) gets HTTP 409
and no security event; the loop asks again on its next pass. The operator passes `MERGE_MODE` and `MERGE_METHOD` to
the sidecar and `MERGE_MODE` to the worker only when the mode is `gated`, so the pod template of an agent without the
field does not change (no restart on upgrade). On Gitea the sidecar cannot tell a person from another agent's
account, so the endpoint stays off there.
