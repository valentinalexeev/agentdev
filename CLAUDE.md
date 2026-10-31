# CLAUDE.md: rules for working in agentdev

Any AI agent working in this repository reads this file first. It adapts the process used in
`valentinalexeev/portfolio-dynamics-lab` (its CLAUDE.md, sections 5a and 5b, and its document 31,
`docs/technical/31-process-raboty-ii-agenta-s-github.md`) to this project. Where this file is silent, that document applies.

## 0. Starting a session

1. Read this file.
2. Load the bot identity: `source ~/.config/github-bot/env.sh`, then check `gh api user -q .login` prints `valentinalexeev-bot`.
3. Open GitHub Issues of `valentinalexeev/agentdev`, **only those assigned to `valentinalexeev-bot`**, in this order:
   open `incident` issues, then `in-progress`, then the rest by priority (lowest number first).
4. Work the issue, check it against its acceptance criterion, update the issue.

## 1. What this project is

A Kubernetes setup that runs a team of Claude Code CLI agents. Each agent has its own forge account
(and optionally its own Claude account) and loops over issues assigned to it and feedback on its PRs.
The MVP runs on kind with gVisor and an in-cluster Gitea (`README.md`). Steps `02`–`04` are the earlier
spike (agent-sandbox, Coder) and are kept for reference only.

Map: `NN-*.sh` steps (run by `run.sh`), `config.env` (all knobs), `lib.sh` (shared helpers),
`images/worker/` (the agent loop), `manifests/` (cluster objects), `seed/` (playground repo content).

## 2. Status lives in GitHub Issues only

- Every piece of unfinished work is an issue: a step of the roadmap, a leftover found in a session, an incident.
  "To do later" in code comments, docs or commit messages without an issue link is not allowed.
- Labels: `AI` (created by an agent), `in-progress`, `needs-decision` (waits for the owner), `blocked`,
  `deferred` (reason in a comment), `incident`, `ADR` (result includes a decision record), plus topic labels
  `forge`, `worker`, `cluster`, `security`, `docs`. Add a new label only if none fits.
- Statuses: not started = open without `in-progress`; in progress = `in-progress`; done = closed as completed
  **only** when the acceptance criterion is met, with a comment (what was done, checks, PR); partial = stays open
  with a list of what remains; rejected = closed as not planned with a reason.
- Commits and PRs reference the issue (`#NN`). `Closes #NN` only when the acceptance criterion is met, otherwise `Refs #NN`.

## 3. Bot and owner

| Action | Bot `valentinalexeev-bot` | Owner `valentinalexeev` |
|---|---|---|
| Issues, comments, labels | yes, marked (below) | yes |
| Decide an open question | **no**, a bot comment is never a decision | yes, by comment |
| Branch, commit, push to a branch | yes | yes |
| Push to `main` | **no** | via PR |
| Open / update PR, fix mechanical conflicts | yes | yes |
| Merge a PR | only after an owner **Approve** review on the PR's current head commit, and green CI (once CI exists) | yes |
| Repo settings, secrets, branch protection | **no** | yes |

- **Marking.** Issues the bot creates get the `AI` label. Bodies, comments and PR descriptions start with `🤖 AI agent:`.
  Commits are authored by the bot; add `Co-authored-by: Valentin Alexeev <valentinalexeev@users.noreply.github.com>`
  when the change was requested by the owner.
- **Queue = assignee.** The agent works only on issues the owner assigned to `valentinalexeev-bot`. It never assigns
  itself. When it needs a decision, or finished and waits for review/merge, it reassigns the issue to `valentinalexeev`
  with a comment saying what is needed. Only the owner gives an issue back.
- **Questions.** Needs a human decision → comment + `needs-decision`: numbered points with options, link to the exact
  section, a default proposal with reasons ("as proposed" counts as the answer), whether it blocks. Purely technical
  choices are decided by the agent in an ADR (`docs/adr/`) without asking. Ask before the work, not at the end.
- **Recording answers.** On an owner answer: comment "Decision recorded: …" linking it, move it into the doc/ADR,
  tick the checklist item, drop `needs-decision`, continue if unblocked. Ambiguous answer → ask again, don't guess.
- **If the bot token fails** (401): tell the owner, continue as the owner account with the `🤖 AI agent:` marker,
  do not merge anything.

## 4. Pull requests

- Branch from `main`, named after the issue: `issueNN/short-name`, `incident/NN-short-name`, `docs/…`.
- Commit and push as you go (semantic commit messages: `feat:`, `fix:`, `docs:`, `chore:`, `ci:` with optional scope).
- Description: `🤖 AI agent:`, issue link, what was done, **checks run with results** (CI, `./run.sh` steps, e2e
  observations), what remains open, files shared with other open PRs.
- **Request the owner's review** when opening a PR, and again whenever a push makes the last approval stale:
  `gh api -X POST repos/valentinalexeev/agentdev/pulls/NN/requested_reviewers -f "reviewers[]=valentinalexeev"`
  (`gh pr edit --add-reviewer` needs `read:org`, which the bot token lacks).
- Stacked PRs (B based on A's branch) merge **top-down, one at a time** with the async endpoint
  `PUT .../pulls/NN/merge-async` (`sha` pinned, `merge_action=direct_merge`): first B into A's branch, then A into
  `main`. Merging B moves A's head, so A needs a fresh approval and green CI on its new head before its own merge.
  Only stacks created through GitHub's stacks API merge as one unit; a PR merely based on another branch does not.
- Before proposing a merge: CI green, and an independent review pass (a separate agent or reviewer) with its
  result in the PR. Blocking findings are fixed first.
- **Documentation is part of the change** ([owner](https://github.com/valentinalexeev/agentdev/pull/41)): a PR that
  adds or changes behaviour updates `docs/` (concepts, user guide, admin, troubleshooting as relevant), `README.md`
  and `CHANGELOG.md` in the same PR. `docs/` is published to the wiki on merge.
- **Merge permission is a GitHub review with state `APPROVED` by `valentinalexeev`**, and only for the commit it was
  given on: a push after the approval needs a new approval. Comments, `COMMENTED` reviews, chat messages, emoji and
  approvals by anyone else are not permission. The bot checks `gh api repos/valentinalexeev/agentdev/pulls/NN/reviews`
  (state `APPROVED`, `commit_id` equal to the PR head) right before merging, then merges with that head `sha` pinned.
  Method: merge commit; the branch is kept unless the owner says otherwise.
- After a merge: check other open PRs for conflicts with `main`; merge `main` into them if the conflict is two
  independent additions; ask the owner if it is a semantic conflict.

## 5. Incidents

An incident is something that already went wrong: red CI on `main`, a leaked or expired secret, an agent that
damaged work, a broken `./run.sh` on a clean machine. The bot opens it immediately (labels `incident`, `AI`):
what happened, when and how found, impact, immediate actions. Never paste secrets, even partially. Fix in
`incident/NN-…` with `Closes #NN`; the closing comment states the root cause, the fix, and what prevents a repeat
(test, CI check, rule change). A leaked secret: tell the owner at once; revoking it and rewriting history are owner actions.

## 6. Engineering rules for this repo

- **Secrets never enter git, issues or logs.** Claude credentials and forge tokens live in `.env` and `.secrets/`
  (gitignored). `config.env` holds only local demo defaults.
- **Every step script is re-runnable** (idempotent) and records its result with `record`. A change to a step is
  checked by re-running that step on a live cluster; a change to the agent loop by an e2e pass (issue → PR → review → reply).
- **Ready-made before custom.** Prefer a maintained component over writing one; a PR that adds custom
  infrastructure says why the ready one does not fit.
- **Pinned versions** in `config.env` / Dockerfiles; bump them in their own commit.
- Shell: `bash`, `set -euo pipefail` via `lib.sh`, passes `shellcheck -S warning` (CI enforces it).
- No em dashes in prose written by agents.

## Bootstrapping on GCP

This repository is the **product**. An instance is deployed from a **separate deployment repository**
(owner decision, #11), never from here. To set one up, copy `examples/deployment/gcp-gke/` into a new
repository and follow its README and this repo's `AGENTS.md`. Do not hand-roll deployment code, and never put
instance values (project ids, regions, people, agent lists) into this repository: modules take them as
required inputs.

## 7. Versioning and releases (strict SemVer)

- Releases are git tags `vMAJOR.MINOR.PATCH` on `main`. Images are published with the same version (#28).
  Deployments pin a tag; nothing deploys from a branch.
- **What counts as the public interface:** Terraform module variables and outputs, Ansible instance vars,
  manifest template variables, image entrypoints and environment variables, the router and listener HTTP API.
- **MAJOR:** a breaking change to that interface (removed or renamed input, changed meaning or default that
  alters behaviour). While the version is `0.x`, breaking changes bump **MINOR** instead.
- **MINOR:** backwards-compatible features. **PATCH:** backwards-compatible fixes only.
- Every PR that changes behaviour adds a line under `## [Unreleased]` in `CHANGELOG.md`
  (Keep a Changelog sections: Added, Changed, Deprecated, Removed, Fixed, Security); breaking ones are marked
  **BREAKING** with the migration step.
- Cutting a release: a PR that moves `Unreleased` to the new version with the date and bumps any pinned
  examples; after the owner merges it, the owner (or the bot on explicit permission) pushes the tag.
