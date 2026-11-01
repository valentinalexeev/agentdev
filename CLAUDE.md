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
  `roadmap` (the umbrella issue with the status comment), `deferred` (reason in a comment), `incident`, `ADR` (result
  includes a decision record), plus topic labels `forge`, `worker`, `cluster`, `security`, `docs`. Add a new label
  only if none fits.
- Statuses: not started = open without `in-progress`; in progress = `in-progress`; done = closed as completed
  **only** when the acceptance criterion is met, with a comment (what was done, checks, PR); partial = stays open
  with a list of what remains; rejected = closed as not planned with a reason.
- Commits and PRs reference the issue (`#NN`). `Closes #NN` only when the acceptance criterion is met, otherwise `Refs #NN`.
- **Every question to the owner goes on an issue** (open one if none fits), in the form of §3 *Questions*, even when
  it is also asked in chat. The owner must see all open decisions in `label:needs-decision` and the next steps in the
  status comment of the umbrella issue (the open issue labelled `roadmap`), without reading the chat.

## 3. Bot and owner

| Action | Bot `valentinalexeev-bot` | Owner `valentinalexeev` |
|---|---|---|
| Issues, comments, labels | yes, marked (below) | yes |
| Decide an open question | **no**, a bot comment is never a decision | yes, by comment |
| Branch, commit, push to a branch | yes, always as the bot | yes |
| Act, commit or push **as the owner** (owner git identity, owner credentials, owner `gh` login) | **no**, unless the owner gave explicit permission **in that ticket, for that change** | yes |
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
- **Never as the owner.** Every commit, push, comment, review and API call is made as `valentinalexeev-bot`. The
  agent never sets the owner's name or email as git author/committer and never uses the owner's credentials
  (keychain, `gh` login, tokens), with exactly one exception: the owner wrote explicit permission **in the issue or
  PR of that change**, naming the change. Permission is per change: an earlier "yes" on another ticket is not
  permission. When the bot cannot do something itself (e.g. its token lacks `workflow` scope for `.github/workflows/`),
  it posts the exact diff on the ticket, asks, and waits; the owner applies it or grants permission there.
- **Check the identity before every `gh` or `git` write** ([#108](https://github.com/valentinalexeev/agentdev/issues/108)).
  The default `gh` login and git config on the owner's machine are the owner's. Every shell first runs
  `source ~/.config/github-bot/env.sh && [ "$(gh api user -q .login)" = valentinalexeev-bot ] || exit 1`, and
  every clone sets `git config user.name valentinalexeev-bot` and
  `git config user.email valentinalexeev-bot@users.noreply.github.com`. Agents work in their own clone, never in
  the owner's checkout.
- **If the bot token fails** (401): tell the owner and stop the affected work; do not fall back to the owner
  account, do not merge anything.

## 4. Pull requests

- **Branch names follow gitflow prefixes**, lower-case, with the issue number:

  | Prefix | For | Example |
  |---|---|---|
  | `feature/` | new behaviour | `feature/57-agents-api` |
  | `bugfix/` | a fix to something not yet released | `bugfix/59-operator-replicas` |
  | `hotfix/` | an urgent fix to a released version, incidents | `hotfix/63-router-crash` |
  | `release/` | cutting a release (`release/0.2.0`) | `release/0.2.0` |
  | `docs/` | documentation only | `docs/60-owner-credentials` |
  | `chore/`, `ci/` | tooling, dependencies, workflows | `ci/64-api-image` |

  Branches start from `main` and PRs target `main` (gitflow names on a single integration branch; a separate
  `develop` branch is not used unless the owner decides otherwise).
- **Commit messages follow [Conventional Commits 1.0.0](https://www.conventionalcommits.org/en/v1.0.0/):**
  `<type>[(scope)][!]: <description>`, imperative, lower-case, no trailing period, then an optional body and footers.
  Types: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`, `revert`. Scope is the
  component (`api`, `ui`, `operator`, `router`, `worker`, `ansible`, `terraform`, `platform`, `mcp`, ...). A breaking
  change (§7) uses `!` after the type/scope **and** a `BREAKING CHANGE: <what and how to migrate>` footer. Footers
  also carry `Refs #NN` / `Closes #NN` and `Co-authored-by:`. The PR title follows the same format, because the
  squash commit takes it.
- Commit and push as you go.
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
  result in the PR. Every finding, nits included, is fixed in the same PR or, for a security advisory, gets a
  recorded outcome (below). A review counts only for the head it names; a push after it needs a new review.
- **Security advisories get an outcome in their own thread** (owner, recorded in
  [#110](https://github.com/valentinalexeev/agentdev/issues/110)). Each `github-advanced-security[bot]` line
  comment, and each security finding on a code line, gets a reply in that thread:
  `gh api -X POST repos/valentinalexeev/agentdev/pulls/{pr}/comments/{comment_id}/replies -f body='...'`
  (list the ids with `gh api repos/valentinalexeev/agentdev/pulls/{pr}/comments`). An advisory posted as a
  top-level PR comment (no thread) gets a quote-reply with `gh pr comment {pr}`. The reply has one outcome:
  **Fixed in `<sha>`** (what changed, file:line, the covering test), **Accepted risk** (why, and the R-number in
  `docs/security.md` or a follow-up issue), or **False positive** (the concrete reason). The independent reviewer
  replies in the same thread with **Verified: fixed / not fixed / outcome disagreed** and one line why, then the
  thread is resolved. A thread without an author outcome and a reviewer verification blocks the review OK and
  the merge. The review summary lists every thread id with its outcome.
- **kind and GKE stay at parity** ([owner](https://github.com/valentinalexeev/agentdev/pull/95#issuecomment-6555379750)): a feature that
  is deployed on kind is also deployable through `infra/ansible` / `infra/terraform`, with templates, instance
  examples and docs in sync, in the same PR; if not, the PR links a follow-up issue.
- **Documentation is part of the change** ([owner](https://github.com/valentinalexeev/agentdev/pull/41)): a PR that
  adds or changes behaviour updates `docs/` (concepts, user guide, admin, troubleshooting as relevant), `README.md`
  and `CHANGELOG.md` in the same PR. `docs/` is published to the wiki on merge.
- **Merge permission is a GitHub review with state `APPROVED` by `valentinalexeev`**, and only for the commit it was
  given on: a push after the approval needs a new approval. Comments, `COMMENTED` reviews, chat messages, emoji and
  approvals by anyone else are not permission. The bot checks `gh api repos/valentinalexeev/agentdev/pulls/NN/reviews`
  (state `APPROVED`, `commit_id` equal to the PR head) right before merging, then merges with that head `sha` pinned.
  Method: merge commit; the branch is kept unless the owner says otherwise.
- **A review names the head it checked** ([#110](https://github.com/valentinalexeev/agentdev/issues/110)): the
  independent reviewer ends the result with `Reviewer: OK at <sha>` (full head sha) or `Reviewer: changes needed at
  <sha>`. A push after it makes the review stale, and the next head needs a new review line.
- **Read the current approval state at each check** (#110): for every open PR, read the reviews and the head now
  (`gh api repos/valentinalexeev/agentdev/pulls/NN/reviews`, `gh pr view NN --json headRefOid`). An `APPROVED`
  review on the current head means the PR waits for merge, even if the approval is older than the last check. Do
  not filter on reviews that are new since the last check.
- After a merge: check other open PRs for conflicts with `main`; merge `main` into them if the conflict is two
  independent additions; ask the owner if it is a semantic conflict.

## 5. Incidents

An incident is something that already went wrong: red CI on `main`, a leaked or expired secret, an agent that
damaged work, a broken `./run.sh` on a clean machine. The bot opens it immediately (labels `incident`, `AI`):
what happened, when and how found, impact, immediate actions. Never paste secrets, even partially. Fix in
`hotfix/NN-…` with `Closes #NN`; the closing comment states the root cause, the fix, and what prevents a repeat
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
- **Manuals use ASD-STE100 Simplified Technical English** (`user-guide.md`, `install.md`, `admin.md`,
  `troubleshooting.md`): task procedures with numbered one-instruction steps, max. 20 words per procedure sentence
  and 25 per description sentence, WARNING/CAUTION before the step, approved words plus the project's technical
  names and verbs. Rules and word list: [`docs/writing.md`](docs/writing.md). Add a new technical word to that list
  in the same PR. Design pages (`concepts.md`, `security*.md`, `adr/`) use normal technical English. The independent
  reviewer checks STE on every PR that changes a manual.

- **Each BREAKING entry names every step or role to re-run** (#110): a **BREAKING** line in `CHANGELOG.md` lists
  every `./run.sh` step and every Ansible role or Terraform module the operator must apply again, not only the main
  one. The author checks that list by applying exactly those steps on a live cluster and says so in the PR.

## Reviewer checklist

The independent reviewer checks these items on every PR, and lists the result per item in the review:

1. **Identity:** commits, pushes and comments are by `valentinalexeev-bot`, never by the owner.
2. **Security advisories:** every GHAS thread has an author outcome and a reviewer verification (§4).
3. **Parity:** kind and GKE (`infra/ansible`, `infra/terraform`) change together, or a follow-up issue is linked.
4. **Untrusted output:** nothing the model can print (logs, stdout, files in the worktree) reaches a control
   decision (limits, pause, reset, credentials). Telemetry comes through a channel the model cannot write.
5. **Numbers:** every value from outside the script is checked against `^[0-9]+$` (or a stricter pattern) before
   shell arithmetic, `[[ -lt ]]` comparisons or `sleep`. Bash arithmetic evaluates expressions and can run commands.
6. **Docs:** `docs/`, `README.md` and `CHANGELOG.md` change with the behaviour; manuals follow STE (`docs/writing.md`).

## Coordination and resource limits

Applies when one coordinator session runs several worker agents (#110).

- **At most 3 active implementation tasks** at the same time. Further work waits in the queue.
- **Workers stop near the limits.** Near the Claude usage limit (5-hour or weekly), or with less than 10 GB free
  disk, a worker commits and pushes its work in progress, writes its state on the PR or issue, and stops.
- **The coordinator watches the disk.** Below 20 GB free it prunes the build cache (`docker builder prune -f`,
  old kind images) before it starts new work.
- **Commit and push work in progress often**, at least after each meaningful step, so a vanished worker loses
  little. A WIP commit uses a normal Conventional Commits message.
- **At each check the coordinator** resumes workers that vanished (from their clone and branch), and stops workers
  that finished and reported.
- **Questions from workers:** when a worker raises a question for the owner, the coordinator puts it on the issue
  in the §3 *Questions* form and adds `needs-decision`. The worker does not wait silently in chat.

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
- Cutting a release: a PR from `release/X.Y.Z` (commit `chore(release): vX.Y.Z`) that moves `Unreleased` to the new version with the date and bumps any pinned
  examples; after the owner merges it, the owner (or the bot on explicit permission) pushes the tag.
