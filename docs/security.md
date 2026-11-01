# Security risk register

Every security risk flagged on agentdev, by code scanning (CodeQL and its AI findings), independent review agents,
or the owner, with its current status. Update this file in the PR that changes a risk's status.

Status: **fixed** (code removes the risk), **mitigated** (reduced, residual risk stated), **accepted** (inherent or
by design, reason stated), **open** (known gap, tracked issue).

The security model these entries refer to is described in [concepts.md](concepts.md#security-model) and, for the
planned target, [security-model.md](security-model.md).

## Agent runtime (worker)

| ID | Risk | Source | Status | Where / how |
|----|------|--------|--------|-------------|
| R1 | Forge token in git remote URL (visible in process list, errors, `.git/config`) | CodeQL #59 | fixed | Token never in a URL; credential helper (#59) |
| R2 | Forge token stored on disk (`/state/git-credentials`) readable through `cat` | CodeQL #59 | fixed | Helper reads token from the loop's environment; old file deleted on start (49c5671) |
| R3 | Forge, wake and register tokens in Claude's environment | CodeQL #59 | fixed | Removed from Claude's environment (`env -u`) (0ff6073) |
| R4 | Claude can read the loop's environment through `/proc/<pid>/environ` with any allowed runner (same UID, same container) | CodeQL #59 on 4d6026f | fixed (pending e2e) | Forge sidecar (security-model.md phase 1, #64): FORGE_TOKEN, WAKE_TOKEN and REGISTER_TOKEN are only in the `forge` container (own UID, own process namespace); the agent container's environment and processes hold none. Sidecar push policy: `agent/<name>/*` only, no force, protected paths, size limit. Until the e2e pass on a cluster, keep issue and review writers trusted |
| R5 | `--dangerously-skip-permissions` (no tool policy) | CodeQL #59 | fixed | `--permission-mode dontAsk` with allow and deny lists (0ff6073) |
| R6 | Build and test runners can start any program, bypassing the tool lists | CodeQL #59 | accepted | Required for an autonomous developer. Boundary is the pod, not the tool list (concepts.md). Residual risk depends on R10 (R4 fixed by #64) |
| R7 | Git hooks or config-defined programs written by Claude run in the loop's git (with the token) | CodeQL #59, review | fixed (unit-tested; live e2e pending) | `lgit` (no hooks, fsmonitor, global or system config), `scrub_git_config` (4d6026f); worktree `.git` and `commondir` pinned before every loop git use, admin dir must be a single real directory, no writes through symlinks (cdef2b4, and the follow-up in this PR) |
| R8 | Credential helper answered any host (redirect or rewritten remote leaks the token) | CodeQL #59 | fixed | Answers only the forge's `scheme://host`; push to the forge URL, not `origin` (4d6026f) |
| R9 | Claude's stderr written into the worktree and committed | CodeQL #59 | fixed | Written under `/state/jobs` and deleted (4d6026f) |
| R10 | **Open egress**: anything Claude can read can be sent anywhere | review, #6 | **open** | Egress allow-list (security-model.md phase 2, #65, #6) |
| R11 | Claude login (subscription or API key) is in the agent pod and readable by Claude | design review | **open** | Credential proxy (security-model.md phase 3, #66) |
| R12 | Prompt injection through issue and review text steers edits | CodeQL #59 | mitigated | Fenced as untrusted, fixed system rule, agent pushes only to its own branch, owner review before merge |
| R13 | Loop pushes without an approval gate | CodeQL #59 | mitigated | Push only to `agent/<name>/*`; merge to main needs the owner's approval (branch protection) |
| R14 | Issue and repo text sent to the model provider | CodeQL #59 | accepted | Inherent to the product; documented. No automatic redaction (would give false assurance) |
| R15 | `Write`/`MultiEdit` not covered by path deny rules | CodeQL #59 | mitigated | Claude Code applies `Edit(...)` rules to all writes; real risk was R7 |
| R16 | A forged worktree admin dir makes pinning fail closed, so that branch stops working until fixed by hand (denial of service, no code runs) | review #59 | open | Self-heal by re-creating the worktree; with #64 |
| R17 | `WORK/.git` itself is not checked for being a symlink | review #59 | open | Same class as R4 (Claude controls the agent's state volume); closed by #64 |

## Operator and Agent API

| ID | Risk | Source | Status | Where / how |
|----|------|--------|--------|-------------|
| R20 | Agent spec can choose any container image | CodeQL #59 | fixed | No image field; image set by the operator (#59) |
| R21 | Agent spec can mount any Secret (forge or Claude credentials of another agent) | CodeQL #59 | fixed | CEL rules bind secrets to the agent name; reconciler fails closed with phase `invalid` |
| R22 | `spec.repo` points the agent at an attacker-controlled repository | CodeQL #59 | mitigated | Pattern-checked; Agents are created by cluster admins, or through the API (#57) only by `agentdev-admins` or a user with **admin** on that repository, checked live on the forge (fail closed); every attempt is logged as `AGENTDEV_AUDIT`. An admin of a repository can point an agent only at that repository |
| R23 | Creating an Agent gives a pod forge write access | CodeQL #59 | accepted | That is the product. The API Role may create/patch/delete Agents only (no Secrets): the forge token must already exist as `agent-<name>` (CEL, R21), so the API cannot mint credentials. Creation needs repo admin or `agentdev-admins` (#57); forge user = authentik username for now (`forgeUserOf`) |
| R24 | **Write credentials to infrastructure** (for example cloud admin for terraform) held by an AI agent | design #69 | **open** | Phase 5 (#69): the agent keeps read-only credentials; a deterministic executor with scoped write credentials applies only an approved plan of the merge commit |
| R25 | The plan executor runs code of an unreviewed PR (providers, modules) | review #85 | open (design) | Sandboxed like an agent, read-only cloud credentials, writes only to its own prefix in the plan bucket (phase 5) |

## Platform (ContextForge, authentik, API, UI)

| ID | Risk | Source | Status | Where / how |
|----|------|--------|--------|-------------|
| R30 | Postgres and ContextForge admin reachable from agent pods | review #50 | mitigated | NetworkPolicies (f57e518). **Not enforced on kind** (kindnet); enforced on GKE (#51) |
| R31 | MCP token scope | review #50 | fixed | Per-agent token scoped to the project's virtual server; revoke and renewal (#50) |
| R32 | In-cluster OIDC key fetch over plain http | review #53 | open | TLS inside the cluster, #51 |
| R33 | UI security headers dropped on some routes; no CSP | review #56 | fixed | Shared header snippet, strict CSP, `frame-ancestors 'none'` (4e1f9be) |
| R34 | A bypass attempt is blocked but nobody sees it, so a prompt injection can keep trying | owner, #64 | mitigated | #70: every refusal becomes a `SecurityEvent` and a row in the append-only `security_events` table; high severity or > 5 events per job pause the agent; Security page, per-agent counter, signed webhook to a SIEM. Agents have no RBAC on SecurityEvents. Source from the container (agent-container events `untrusted`), per-container rate limit with a `rate-limited` event, per-agent webhook queues, API login without DELETE/TRUNCATE (owner role, SECURITY DEFINER prune). Forge comments and notifications to project writers: #67, #57. Tamper evidence (hash chain), syslog, OTLP: #71 |
| R36 | A compromised agent forges security events or floods the collector | review of #82 | mitigated | `source` from the container, agent from the pod label, agent-container events marked `untrusted`; token bucket per pod container (10/min, burst 20), first overflow is a `rate-limited` high event that pauses only that agent; webhook queues per agent. Forged events can only pause the agent's own pod (accepted) |
| R37 | The API's database login deletes or rewrites the event history | review of #82 | mitigated | owner role `agentdev_owner`; `agentdev_api` has INSERT/SELECT and EXECUTE on the SECURITY DEFINER `security_events_prune()` only; retention in `security_settings`, owner-only. Open: a Postgres superuser can still change history; tamper evidence (hash chain, external copy) is #71. `REVOKE CREATE ON SCHEMA public FROM PUBLIC` (PG < 15) so the definer's `search_path = pg_catalog, public` cannot be shadowed. **Before release:** run `TestPgRoleSeparation` (`images/api`, skipped without `TEST_DATABASE_URL`) once against a real Postgres as a superuser, e.g. `TEST_DATABASE_URL=postgres://postgres:...@localhost/postgres go test -run TestPgRoleSeparation ./...` |
| R35 | Every signed-in user can read every agent's security events (the `action` text can quote issue content) | #70 | open | Restrict `GET /api/security-events` to projects with write access (#57) |
| R34 | Sign-out left the authentik session alive | review #56 | fixed | `signoutRedirect` (4e1f9be) |
| R35 | GitHub App manifest built by string templating (injection) | review #61 | fixed | Built with `jq --arg`, strict input validation (db8be52) |
| R42 | API maps the authentik username to the forge login: a chosen or case-variant name ("Adam") could act as another forge user | review #86 | mitigated | Only lowercase names matching `^[a-z0-9][a-z0-9._-]*$` map to a forge user; requires closed enrollment and no username changes in authentik (admin.md, "Keep the usernames safe"). Intended setup: authentik has no own user database and signs people in through the same company SSO as the forge (for example Google Workspace), so one account gives both names. Stable claim: #88 |
| R43 | API forge token user must be repo or site admin to read permissions | review #86 | accepted | Read-only scopes (`read:repository`, `read:user`); dedicated service account, admin on agent repositories only |
| R44 | The audit of agent changes is lost with the pod log, or the API rewrites it to hide a change | #91 | mitigated | Every `AGENTDEV_AUDIT` line is also in the append-only `api_audit` table: `agentdev_api` has INSERT/SELECT and EXECUTE on the SECURITY DEFINER `api_audit_prune()` only, a trigger refuses UPDATE/DELETE inside the `security_settings` retention (`TestPgAudit`). `GET /api/audit` is `agentdev-admins` only. Open: a failed insert is only logged (the stdout line stays); audit rows are not forwarded to the SIEM outputs yet; superuser tampering as R37 (#71) |

## Process

| ID | Risk | Source | Status | Where / how |
|----|------|--------|--------|-------------|
| R40 | Assistant committed under the owner's name without permission | owner | fixed | CLAUDE.md rule (#60): per-ticket explicit permission only |
| R41 | Workflow changes need a token the bot does not have | process | mitigated | Diff posted on the ticket, pushed under the owner's name only after a per-ticket yes |
