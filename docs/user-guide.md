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

## Chat with the project before filing work

To explore an idea, ask questions about the code or draft an issue together, open an interactive session:

```bash
scripts/agent-chat.sh start my-project.yaml      # starts a session and attaches your terminal
```

- Talk to Claude as in any Claude Code session. It has a fresh clone of the project and can read and run code.
- **Detach** with `Ctrl-b d`; the session keeps running. **Re-attach** later, from this or another machine with
  cluster access: `scripts/agent-chat.sh attach` (the latest session) or `attach <session>`.
- `scripts/agent-chat.sh list` shows running sessions; `stop <session>` ends one.
- The session **cannot push**. When an idea is ready, ask it to file an issue (it uses the Task format and leaves it
  unassigned); then assign the issue to an agent as usual.
- It ends by itself after 30 minutes with nobody attached (`IDLE_SECONDS` changes that).

## What to expect

If your instance runs botboard, open it to see every agent's current work at a glance: one card per issue, its phase
(plan, build, review, done, blocked) and a link to the issue or PR.

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
