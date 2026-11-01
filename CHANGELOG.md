# Changelog

All notable changes to agentdev. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versioning: [Semantic Versioning](https://semver.org/spec/v2.0.0.html) (rules in `CLAUDE.md` §7).
While the version is `0.x`, breaking changes bump MINOR.

## [Unreleased]

### Added
- Audit trail in Postgres (#91): every `AGENTDEV_AUDIT` line of the write API is also stored in the append-only
  `api_audit` table (same owner/API role split, trigger and `security_settings` retention as `security_events`; the
  API login has INSERT/SELECT and EXECUTE on `api_audit_prune()` only). The stdout line stays. New
  `GET /api/audit` (filters `agent`, `result`, `since`, `limit`, `cursor`), readable only by `agentdev-admins`.
- Subscription usage limits (#93, incident #92): after each Claude run the worker prints an `AGENTDEV_USAGE {json}`
  line (tokens, 5-hour and weekly rate-limit windows from the CLI's `rate_limit_event`; the worker now uses
  `--output-format stream-json`). The operator sums them per Claude credential Secret and, when a window reaches its
  threshold (defaults 80 % 5-hour, 90 % weekly), pauses **new** runs of every agent on that subscription until the
  window resets (running jobs finish; automatic resume, also without the operator). A run refused by Claude's rate
  limit pauses that agent at once (reset time from the CLI, else 1 h) and keeps the work for a retry instead of a
  "could not produce a change" comment. Each pause is a `SecurityEvent` (source `operator`, rule
  `usage-five-hour|usage-weekly|usage-rate-limited`, severity low). State in ConfigMap `agentdev-usage` (mounted
  read-only into agent pods), thresholds in `agentdev-usage-settings`. API: `GET /api/usage`,
  `PUT /api/usage/global` (agentdev admins), `PUT /api/usage/projects/{owner}/{repo}` (write access; only lower than
  global, clamped again by the operator); every attempt audited as `set-limits`. Web UI: **Usage** page. RBAC: the
  operator gets get/update on `agentdev-usage` and get on `agentdev-usage-settings`; the API gets configmap create
  and get/update on `agentdev-usage-settings`, get on `agentdev-usage`.
- Web UI controls for agents (#57 step 4): per agent **Pause**/**Resume**, **Edit** (`maxConcurrent`, `scaleToZero`)
  and **Delete** with a confirm dialog (native `<dialog>`, keyboard usable, names the agent); **Add agent** form
  (name, repo, forge, maxConcurrent, scaleToZero) with client-side checks that mirror the API patterns. Buttons are
  always shown; the API decides: 403 shows the API message, 503 shows "permission check unavailable". The list
  reloads after each change. Phases `paused` and `invalid` have styles. No new dependencies; CSP unchanged.
- API writes to Agents (#57 step 3): `PATCH /api/agents/{name}` (JSON merge patch of `paused`, `maxConcurrent`,
  `scaleToZero`), `POST /api/agents` and `DELETE /api/agents/{name}` (volumes always kept). Permission is checked
  live on Gitea (`/collaborators/{user}/permission`): write on the repository to pause, resume or edit; admin to
  create or delete; the authentik group `agentdev-admins` may always pause, create and delete. The forge user is the
  authentik username (pluggable `forgeUserOf`). If the forge cannot be reached the change is refused (503). Every
  attempt prints an `AGENTDEV_AUDIT {json}` line (who, action, agent, before, after, result). Writes require a bearer
  token (no cookies, CSRF). The API Role gains `patch`, `create`, `delete` on `agents` only. New optional Secret
  `agentdev-api-forge` (`FORGE_API_TOKEN`, scopes `read:repository`, `read:user`; the token user must be admin of
  the agent repositories, Gitea requires it) and env `FORGE_URL`, `ADMIN_GROUP`. Review of #86: only lowercase
  usernames map to a forge user (R42); DELETE uses UID and resourceVersion preconditions; repositories with `.`/`..`
  segments are refused; refused logins (401) on write routes are audited; GitHub agents give 403 for repository
  users until #87.

### Security
- Forge sidecar (#64, security model phase 1, fixes R4 pending e2e): every agent pod gets a `forge` container
  (`images/forge`, image `ghcr.io/valentinalexeev/agentdev-forge`, UID 1001, own process namespace) that alone holds
  `FORGE_TOKEN`, `WAKE_TOKEN` and `REGISTER_TOKEN` and does every forge operation for the loop over localhost:
  allow-listed REST calls, sync and push as git bundles. Pushes only to `agent/<name>/*`, no force, protected paths
  (`.github/workflows/` and friends, `PROTECTED_PATHS`) refused, size limit (`MAX_PUSH_MB`). The webhook listener
  moved into the sidecar. **BREAKING** for deployments: the operator needs `FORGE_IMAGE` (Ansible `forge_image`,
  derived from `image_tag`; kind builds it in `./run.sh 06`); the worker image no longer runs without the sidecar.
- Security events (#64): every refusal by the forge sidecar (foreign branch, protected path, oversize, API call
  outside the allow-list or for another repo, non-loopback caller, bad webhook token) and by the loop (redirected or
  forged worktree, removed git config keys, planted hooks) prints an `AGENTDEV_SECURITY_EVENT {json}` line to stdout.
- Security events are collected, stored and reported (#70): the operator turns `AGENTDEV_SECURITY_EVENT` log lines of
  every agent container into `SecurityEvent` objects (new CRD `securityevents.agentdev.dev`, schema
  `agentdev.security/v1`, kept 30 days or 500 per agent) and pauses an agent on a high-severity event or more than
  `SECURITY_PAUSE_AFTER` (5) events in one job (condition `SecurityPaused`). The API appends them to the append-only
  `security_events` table in the platform Postgres (database `agentdev`, kept 1 year), serves
  `GET /api/security-events` (filters, cursor paging) and sends new events to an optional HMAC-SHA256 signed webhook.
  The web UI has a **Security** page and a counter per agent. Deployments: install the new CRD (`./run.sh 12`,
  Ansible does it); the operator needs `pods`, `pods/log`, `configmaps` and `securityevents` in its Role (in the
  manifest); `./run.sh 10` creates the API's database login.
- Security events hardening (review of #82): `source` comes from the container, not the line, and events from the
  agent container carry `untrusted: true`; per pod container token bucket (`SECURITY_RATE_PER_MIN` 10,
  `SECURITY_RATE_BURST` 20), the first overflow becomes one `rate-limited` high event, the rest is dropped and
  counted; webhook queues per agent (round robin, 4 workers, drops counted per agent); `job` must match
  `^(issue|pr)-[0-9]+$` (else empty) and the CRD limits field lengths; a failed record no longer advances the log
  position; text is clipped on UTF-8 boundaries; `TooManyEvents` counts per pod UID and job, including events without
  a job and the event just recorded; operator RBAC on ConfigMaps narrowed to `agentdev-security-positions` for
  get/update. Postgres: new owner role `agentdev_owner`; the API login `agentdev_api` gets INSERT/SELECT and the
  SECURITY DEFINER `security_events_prune()` only; the retention lives in `security_settings` and the API's
  `SECURITY_RETENTION_DAYS` env is gone. **BREAKING** for deployments: re-run `./run.sh 06 10 12` (10 moves
  ownership and grants, 12 installs the `SecurityEvent` CRD, 06 loads the forge image);
  the API no longer creates its schema unless `DATABASE_MIGRATE=true`.
- The sidecar picks the default branch for the protected-path check itself and checks merge commits too, so a
  missing or wrong `base` cannot let a workflow change through (review of #68).

### Added
- Security model phase 5 (#69, design): deterministic executors apply changes that an AI agent designed with
  read-only credentials, after the merge and an explicit `/approve`; trusted plan required, one executor per
  environment.
- Manuals follow ASD-STE100 Simplified Technical English (#75): rules and the project word list in `docs/writing.md`,
  CLAUDE.md rule for agents and reviewers; the admin guide's *Web UI* section is the first converted page.
- MCP tools per project through ContextForge (#14): `./run.sh 08` deploys `agentdev-platform` (Postgres,
  ContextForge); `mcp:` in the project spec plus `scripts/mcp-sync.sh` give each agent a token scoped to its
  project's virtual server only; the readiness report counts the tools.
- authentik as the instance identity provider (#45): `./run.sh 09` installs the Helm chart on the platform
  Postgres (database `authentik`, no Redis); the owner is the first admin.
- Management API skeleton (#46): Go service `agentdev-api` (`./run.sh 10`, image `ghcr.io/valentinalexeev/agentdev-api`),
  authentik OIDC required (401 without a valid token), read-only `/api/me` and `/api/agents`; the `agentdev` OIDC
  application comes from an authentik blueprint.
- Web UI shell (#48): React app `agentdev-ui` (`./run.sh 11`, image `ghcr.io/valentinalexeev/agentdev-ui`): sign in
  with authentik (code + PKCE), agent overview from the API; nginx serves runtime config and proxies `/api`.
- Agent operator (#47): `Agent` CRD (`agentdev.dev/v1alpha1`) and a controller-runtime operator
  (`./run.sh 12`, image `ghcr.io/valentinalexeev/agentdev-operator`); creating, pausing or deleting an Agent
  creates, stops or removes its StatefulSet; honours `agentdev/blocked` and router scale-to-zero. An Agent can
  reference only its own Secrets (`agent-<name>`, `claude-<name>` or `claude-shared`).
- GitHub App manifest template and `scripts/github-app.sh` (render, registration page, code conversion) with the
  deployment's URLs from `instance.yml` `github_app:` (#2).

### Changed
- The manuals (`user-guide.md`, `install.md`, `admin.md`, `troubleshooting.md`) use ASD-STE100 Simplified Technical
  English: task procedures with numbered steps, safety notes before their steps, a reference part per section, and
  troubleshooting as symptom / cause / procedure tables (#75). New technical words are in `docs/writing.md`.
- `GET /api/agents` reads Agent objects (#57): adds `managed`, `paused`, `maxConcurrent`, `scaleToZero`; agents still
  deployed as plain StatefulSets are listed with `managed: false`. The API's Role can read Agents.
- Image builds no longer emulate arm64 for compiling: the api, operator and forge Dockerfiles cross-compile on the builder's
  platform, and the UI's static build runs once; vet, tests and lint stay in the CI `lint` job. Multi-arch image
  builds take minutes instead of up to 30 minutes on a cold cache.
- Readiness report state is keyed by repository and issue, so an agent re-bootstrapped into another project reports
  there too.
- `06-agents.sh`, `scripts/bootstrap-project.sh` and the Ansible playbook create `Agent` objects; the operator
  renders the StatefulSets and adopts existing ones in place (no restart). `06` installs the operator when missing.
  **Removed** `manifests/agent.yaml.tmpl` (#57).
- **BREAKING** `infra/ansible`: per-agent `agents[].idle_seconds` is no longer applied; the idle time is
  cluster-wide (`idle_seconds`, set on the operator) until the Agent spec has it (#57).

### Security
- Agent runs are guarded (#59, CodeQL): Claude gets no forge or router tokens, works with an allow-list of tools
  (`--permission-mode dontAsk`; file tools, git without push/remote/config, build and test runners; replace with
  `CLAUDE_TOOLS`) instead of `--dangerously-skip-permissions`, and receives issue and review text fenced as untrusted
  data under a fixed system rule. The forge token is in no git URL and on no disk: a credential helper reads it from
  the loop's environment and answers only for the forge host. The loop's own git runs no hooks, fsmonitor or
  config-defined drivers, pushes to the forge URL rather than `origin`, and keeps Claude's stderr out of the worktree.
  The security model is documented in `docs/concepts.md`; egress control remains open (#6).
  **Known gap:** a build or test command Claude runs can read the loop's environment, including the forge token
  (R4). Until the forge sidecar lands, only trusted people should write issues and reviews for an agent.
- Security risk register `docs/security.md` (every flagged risk with its status) and the target security model
  `docs/security-model.md` (forge sidecar, egress allow-list, Claude credential proxy, approvals in the UI).
  Approvals belong to the project: anyone with write access to the agent's repository decides, in the UI or with
  `/approve` on the forge (#67). Every guardrail refusal is reported as a security event, with persistence and SIEM
  integration (#70, #71).
- The loop's git ignores global and system git config and pins each worktree's `.git` to the shared clone.

## [0.1.0] - 2026-10-31

### Added
- Coding agents on Kubernetes: one Claude Code worker per forge account, polling assigned issues and PR
  feedback, under gVisor (kind locally).
- Webhook router with agent registration; agents wake on signed forge webhooks, polling as fallback (#3).
- Owner approval required to merge agent PRs in the forge (#7); exactly one Claude credential per agent (#5).
- GCP infrastructure as a reusable Terraform module (`infra/terraform`, plus `bootstrap/`) and an Ansible
  playbook (`infra/ansible`) (#11).
- `examples/deployment/gcp-gke`: template for a separate deployment repository (#11).
- Router published as its own image; deployments pull `ghcr.io/valentinalexeev/agentdev-{worker,router}` (#28).
- ADR 0001: ContextForge as the MCP gateway per instance (#14).
- Agents work on several items in parallel (`MAX_CONCURRENT`, default 2), each in its own git worktree, with
  backoff on failures (#20).
- `scripts/bootstrap-project.sh <project.yaml>`: connects an existing repo from one spec. It sets up forge accounts,
  branch protection and labels, proposes the process pack (`process/default`) by PR, deploys the agents and
  checks that each agent reports ready on a forge issue (#31).
- Scale to zero, opt-in (`SCALE_TO_ZERO` / `agents[].scale_to_zero`): an agent idle for `IDLE_SECONDS` (900) asks
  the router to stop it; a webhook for a stopped agent makes the router start it again (#35). The router gets a
  namespaced Role limited to reading agent StatefulSets and patching their scale.
- Interactive sessions (`scripts/agent-chat.sh start|attach|list|stop`): a chat with Claude Code in a project, as a
  short-lived gVisor Job with a fresh clone and a read + issues/comments forge account (no push). Re-attachable from
  any terminal with cluster access; ends after 30 min with nobody attached; optional Claude app access (#38, #26).
- Agents report their work to [botboard](https://github.com/valentinalexeev/botboard) (`./run.sh 07`, `BOTBOARD_URL`):
  one card per issue through plan, build, review, done/blocked, linking to the issue or PR (#26).
- Resource control without the UI (#27): `limits:` in the project spec becomes a `ResourceQuota`;
  `scripts/agent-block.sh` stops an agent and keeps it stopped (router and redeploys honour `agentdev/blocked`).

### Changed
- **BREAKING** `infra/terraform`: `project_id`, `region`, `zone` and `github_repository` are required (no
  instance defaults); the module no longer declares a backend; the deployment's root module does (#11).
- **BREAKING** `infra/ansible`: instance values (`gcp_project`, `gcp_zone`, `image_tag`, `reviewer`, `agents`)
  must come from the deployment (`-e @instance.yml`); the playbook stops if they're missing (#11).
- **BREAKING** `manifests/router.yaml.tmpl`: takes `ROUTER_IMAGE` and `IMAGE_PULL_POLICY` instead of
  embedded code (`ROUTER_JS`, `ROUTER_HASH`) (#28).

[Unreleased]: https://github.com/valentinalexeev/agentdev/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/valentinalexeev/agentdev/releases/tag/v0.1.0
