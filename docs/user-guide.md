# User guide

For people who give agents work and review what they produce.

## Give an agent work

1. Open an issue with the **Task** template.
   - **What:** the change, in plain words.
   - **Acceptance criterion:** an observable check that decides "done" (a test passes, a page shows X).
   - **Open decisions:** anything the agent must not guess.
2. **Assign it to an agent account** (for example `alice`). Assigning is what queues the work.
3. Within seconds the agent comments that it picked the issue up. It then works on a branch
   `agent/<agent>/issue-<n>` and opens a pull request that requests your review.

Small, well-scoped issues work best. One issue becomes one PR.

## Review an agent's pull request

- **Comment** or **Request changes**: the agent reads all new feedback, pushes a fix to the same PR and replies.
  Several comments at once are handled together.
- **Approve and merge** when you are satisfied. Agents never merge. A push after your approval dismisses it, so
  you always approve the code you merge.
- To stop work on a PR, close it.

## When the agent needs a decision

If an issue is ambiguous, an agent is instructed (by `AGENTS.md` from the process pack) to ask in the issue and
add the `needs-decision` label instead of guessing. Answer in a comment. If it could not produce a change at all,
it says so in the issue ("could not produce a change"); open a new issue with more detail.

## What to expect

| Situation | What happens |
|---|---|
| Two issues assigned at once | both are worked on in parallel (up to the agent's limit), in separate PRs |
| Agent is scaled to zero | assigning or commenting starts it, usually within seconds |
| Feedback while a fix is running | picked up in the next pass, nothing is lost |
| Webhooks are down | agents still poll, just more slowly |

## What agents do not do

- merge, push to the default branch, change branch protection or settings;
- work on issues not assigned to them;
- commit secrets, build output or caches.

Project-specific instructions for agents (how to build, test, code style) go in `AGENTS.md` in your repository,
below the line.
