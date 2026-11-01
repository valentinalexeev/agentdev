# Working with agentdev agents

This repository is worked on by agentdev coding agents. Each agent has its own forge account.

## How work flows

1. **Issue:** describe the change and an observable acceptance criterion (template: Task). Assign it to an agent
   account to queue it.
2. **PR:** the agent comments on the issue, works on a branch `agent/<agent>/issue-<n>` and opens a PR that
   requests the owner's review.
3. **Review:** comment or "Request changes" on the PR. The agent pushes fixes and replies. Only an approval by the
   owner lets the PR merge (branch protection). A new push dismisses an earlier approval.
4. **Merge:** done by the owner.

## Rules for agents

- Never push to the default branch. Never merge. One issue, one branch, one PR.
- Keep the PR description current: what was done, which checks ran with what result, what is still open.
- If the request is ambiguous, ask in the issue and add `needs-decision` instead of guessing.
- Never commit secrets, build output or caches.
- **Security advisories:** reply in each code-scanning or security advisory thread on your PR with one outcome:
  *Fixed in `<sha>`* (what changed), *Accepted risk* (why, and where it is tracked) or *False positive* (the concrete
  reason). List each thread in the PR section *Security findings*. Never resolve a thread you did not fix; the
  reviewer resolves it after checking.
- **Reviews count for one commit:** a review names the commit it checked (*Reviewed at `<sha>`*). A push after it
  needs a new review.
- **Keep every deployment target in sync:** a change that affects one target (for example a local cluster and a
  cloud deployment) updates all of them in the same PR, or links a follow-up issue.
- **Never print secrets.** Never let output of the model (logs, generated text, files it wrote) decide a control
  action such as limits, pausing or credentials, and never pass it to shell arithmetic or `eval`.

Project-specific instructions (build, test, style) go below this line.

---
