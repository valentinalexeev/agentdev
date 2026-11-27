---
issue: 578
type: feature
section: Added
status: done
breaking: false
title: Opt-in merge of an agent's own pull request after an owner approved its exact head commit
components: [api, forge, operator, worker]
pr: 580
---
## Summary

New field `spec.merge` on the `Agent` (`mode: off | gated`, `approvers: [logins]`).
The default is `off`: the agent never merges, as before. With `mode: gated` (GitHub only) the forge sidecar merges the
agent's own PR when an owner listed in `approvers` approved the exact head commit, every check passed, no change is
requested and protected paths have an approval. It always makes a merge commit (no squash or rebase). Each merge is an `agent-merge` audit entry; a refusal is a security
event. See `docs/admin.md`, "Gated merge of the agent's own PRs".

## Upgrade

1. Nothing to do to keep the old behaviour: agents without `spec.merge` (or with `mode: off`) render the same pod.
2. To turn it on for one agent, apply the new CRD (part of the release manifests), then set
   `spec.merge: {mode: gated, approvers: [<owner login>]}` on that `Agent`. `approvers` is required with `gated`;
   without it the operator marks the agent `invalid`.
   The repository must allow merge commits.
3. Keep branch protection on the default branch (required owner review, dismiss stale approvals, required checks).
   The sidecar checks the same facts itself, but the forge's rules stay the last gate.
4. On Gitea `gated` has no effect: the sidecar logs that the merge endpoint stays off.

## Description

The sidecar reads every fact from the forge with its own credential at the moment of the merge, never from the agent
(`images/forge/merge.go`): the PR is open, from `agent/<name>/*` of this repository, opened by the agent, into the
default branch; the latest deciding review of an `approvers` login is `APPROVED` with `commit_id` equal to the head
(a dismissed approval, an approval of an earlier commit, or one by a non-owner, a bot or the agent itself never
counts); every check run and commit status on the head passed and the forge reports the PR mergeable. The merge call
pins the head (`sha`) and always uses `merge_method: merge` (a `MERGE_METHOD` other than merge stops the sidecar at start); the request carries only the PR number, so the agent
cannot choose the approver or the method. A PR that is not ready (a check pending) gets HTTP 409 without an event;
the loop asks again on its next pass. On Gitea the sidecar cannot tell a person from another agent's account, so it
refuses explicitly: no endpoint. A console chat pod never merges: the API drops `MERGE_*` from its sidecar, and the
sidecar refuses `POST /merge` with `CHAT_SESSION` set before any forge call (`chat-label-not-asked`).
