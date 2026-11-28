# ADR 0012: steer agents with native forge actions

- **Status:** accepted
- **Date:** 2026-11-27
- **Issues:** #619 (this decision), #340 (console scope), #237 (who gives work), #573 (assigned PRs), #578 (gated merge)
- **Owner decision:** "As proposed" on #619 (recorded 2026-11-27) for open questions 1 to 5: no new slash commands;
  `/approve <id>` and `/approve-job <id>` stay as the one exception; `hold` applies to one issue or PR; the label is
  `hold` with no prefix; triage for state actions, write for content actions, `agentdev-admins` in the console for a
  pause of an agent or of the farm.

## Context

Owners run the agent farm through task management on the forge. The console does not replace the forge or the
SDLC process (#340). Slash commands in comments (`/pause`, `/redirect`) are not native to the forge, are hard to find
and add noise to the thread. The forge already has a state change for almost every steering need, and it sends a
signed webhook for each one.

Two facts of the current design shape this decision:

- **The router only wakes.** It verifies the signature, reads routing facts and forwards a bare wake. The payload never
  reaches an agent, and an agent also works without webhooks (`POLL_SECONDS`, the router's wake poll). Each pass of
  the loop re-reads the state from the forge API (`docs/concepts.md`, "Webhook router").
- **Forge permissions are not enough on a public repository.** Labels, assignees, milestones and draft state need
  triage or write access. But any user can comment and review, and the author of an issue (an outside user too) can
  edit its body, close it and reopen it.

## Decision

### D1. Steering need to forge action

| Steering need | Native forge action | Webhook event | Minimum level |
|---|---|---|---|
| Start work | Gitea: assign the agent. GitHub: add the label `agent:<name>` (an App cannot be assigned) | `issues.assigned`, `issues.labeled` | triage |
| Stop or pause this task | Unassign the agent or remove `agent:<name>`. To keep the task but hold it, add the label `hold` | `issues.unassigned`, `issues.unlabeled`, `issues.labeled` | triage |
| Abandon | Close the issue as not planned. The agent drops the branch or PR | `issues.closed` | triage |
| Resume or retry | Assign the agent again, reopen, or remove `hold` | `issues.assigned`, `issues.reopened`, `issues.unlabeled` | triage |
| Change direction | Edit the issue body (the acceptance criterion is the spec), then add a normal comment with the reason | `issues.edited`, `issue_comment.created` | write |
| Answer a question | A normal comment. Remove `needs-decision` to signal the answer | `issue_comment.created`, `issues.unlabeled` | write |
| Rework a PR | Review with **Request changes**, or line comments | `pull_request_review.submitted`, `pull_request_review_comment.created` | write |
| Do not merge yet | Convert the PR to draft (Gitea: the `WIP:` title prefix), or add `hold` | `pull_request.converted_to_draft`, `pull_request.edited` | triage |
| Priority and order | Milestone, priority label, order on the project board | `issues.milestoned`, `issues.labeled` | triage |
| Pause one agent or the whole farm | Not a task action: the console or admin (CRD field) | none | `agentdev-admins` |

Gitea has no triage role: there the minimum for every row is write. GitHub and Gitea send all events in the table.

### D2. No new slash commands

Agents are steered with the actions of D1 only. `/approve <id>`, `/deny <id>` and `/approve-job <id>` stay: they
approve one action of an executor or one job, and no native forge state fits that. A label on the approval request
or a review of a plan PR can replace them later, after its own design. That is not part of this decision.

### D3. `hold` holds one issue or one PR

The label `hold` (no prefix) on an issue or a PR stops new work on that item: the loop starts no job on it, and does
not merge it (`on_hold`, `images/worker/agent-loop.sh`). The cursor of the item stays, so removing the label resumes the
work on the next pass. A job that runs when the label is added finishes its current run. A pause of a full agent stays
an operator action (the console **Pause**, `spec.paused`).

`hold` needs no actor check: only people with triage access or more can change labels, and the label can only stop
work. Removing it only returns to the state before.

### D4. Authority is checked where the control decision is made

The issue thread proposed a sender check in the router. The router does not decide anything: it wakes, and the loop
reads the forge state, also on the polling paths that have no webhook. A check in the router alone would not cover
them. So:

1. The loop checks the actor of each state that it acts on, as the forge records it (issue events on GitHub, the
   timeline on Gitea), never a payload field or `author_association`. It gets the level with
   `GET /repos/{owner}/{repo}/collaborators/{user}/permission` (GitHub) or the assignee list (Gitea, see
   `labeler_ok`). An unknown answer is a refusal (fail closed).
2. A refused state is a security event (`security_event`), so the console **Security** page shows it.
3. A comment or a review by a user below the level of its row is data, not a steering action. The agent can read it
   as untrusted context. It never removes `needs-decision`, never counts as an answer and never starts a rework run.
4. At the start of an issue run the agent records the issue body and its hash. An edit by a user below write is
   ignored, and the agent writes this in its status comment.
5. The router ignores the events that the agent's own forge account causes (`sender` is the agent). This saves a
   wake. It is not a security control: the loop already ignores its own comments and reviews.
6. An instance can name its owners or a team. Then only they can steer, also when the repository has more
   collaborators. This is for a farm that more than one team shares.

These rules keep reviewer checklist item 4 of `CLAUDE.md`: no text from an untrusted party, and nothing that the
model prints, reaches a control decision.

### D5. The agent shows its state on the forge

The agent shows its state with a commit status on the PR head and one edited status comment per issue. Gitea has no
checks API, so both forges use the commit status.

## Status of the implementation

| Row or rule | State |
|---|---|
| Start | Done before this ADR: the assigned issue list and `labeler_ok` (write; triage on GitHub is a follow-up of D4) |
| Stop (unassign, remove the label) | Done before this ADR: the item leaves the list of the next pass |
| `hold` (D3) | Done in #619 |
| Abandon (close as not planned) | Partly: a closed issue leaves the list. Dropping the branch or PR is open |
| Resume | `hold` and reassignment work. A reopened issue that already has a branch is not taken again |
| Change direction | Partly: each run reads the current body and the discussion. The spec freeze of D4 is open |
| Answer a question | Partly: any comment resumes the issue (`awaiting_answer`). The write check of D4 is open |
| Rework a PR | Partly: every review and comment is feedback (`new_feedback`). The write check of D4 is open |
| Do not merge yet | Done: the sidecar refuses to merge a draft (`images/forge/merge.go`); the loop does not ask to merge a held PR (D3) |
| Priority and order | Open |
| Router ignores the agent's own events (D4.5) | Open |
| Owner list (D4.6), status comment and commit status (D5) | Open |

The open rows are done in follow-up issues of #619. Each one closes a gap of D4 or a row of D1.

## Consequences

- Owners steer with the forge actions they already know, and the thread has no command noise.
- A loop pass does more forge reads for content events (the actor and its permission), cached for a short time.
- On a public repository, an outside user can still comment and review. That text reaches the agent only as
  untrusted context.
- A held item keeps its cursor, so many feedback items can wait until the hold ends. The next run gets all of them.

## Alternatives rejected

- **Keep or add slash commands** (option 1, keep as a shortcut). Two ways to do one thing, and the command text is
  written by anyone who can comment, so each command needs the same actor check anyway.
- **A prefixed label `agent:hold`.** The `agent:` prefix routes work to an agent on GitHub, and a second meaning makes
  the router and the people read it wrong.
- **Write for all actions** (open question 5, option b). Simpler, but triage users cannot hold or start work, which is
  the reason that role exists.
- **The sender check in the router only.** The router does not see the polling paths, and a wake is not a decision
  (D4).
