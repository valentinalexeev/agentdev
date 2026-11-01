# Changelog

All notable changes to agentdev. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versioning: [Semantic Versioning](https://semver.org/spec/v2.0.0.html) (rules in `CLAUDE.md` §7).
While the version is `0.x`, breaking changes bump MINOR.

## [Unreleased]

### Added
- GKE public HTTPS entry point (#120): Terraform `public_hosts` makes a Google-managed certificate and a TLS 1.2+
  SSL policy, optional `cloud_armor` a Cloud Armor policy (size limit, per-IP rate limit); the cluster turns on the
  Gateway API. New Ansible role `ingress` (runs when `instance.yml` sets `ingress.host`, values from
  `terraform output ingress`) applies a Gateway on the existing static IP: `https://<webhook_host>/hook` to the
  router, `https://<host>/` to the web UI, HTTP to HTTPS redirect, HSTS. Nothing else is public. The role `api` now
  also deploys the web UI (`manifests/ui.yaml.tmpl`; `api.ui: false` skips it). UI nginx limits bodies to 1 MB.
  Docs: `docs/admin.md` "Public HTTPS entry point (GKE)", kind parity with port-forward. New risks R66 to R68.

### Changed
- GitHub App webhook URL is `<public_url>/hook` (was `<public_url>/`), because the Gateway routes only `/hook` to the
  router (#120). **BREAKING** for an App registered before: change the webhook URL on github.com to
  `https://<host>/hook`. To apply #120: Terraform `infra/terraform` (gateway_api_config is an in-place cluster update),
  then the Ansible playbook `deploy.yml` (roles `api` and `ingress`). The router itself still accepts `/`.

### Changed
- API forge identity (#88): the forge user comes from the new token claim `forge_login` (authentik scope `forge`,
  scope mapping in the agentdev blueprint, from the user attribute `forge_login`), not from the authentik username.
  The web UI requests the scope `forge`. **Breaking:** set `forge_login` for each person (source property mapping of
  the company SSO, or the user attributes; `docs/admin.md`, "Map people to forge users"); without it a person is
  read-only. Risk R42 updated.

### Added
- Company SSO (#122): sign-in to authentik through Google Workspace (SAML) or another OIDC provider, configured by the
  new blueprint `manifests/authentik-blueprint-sso.yaml` from the optional Secret `authentik-sso` (GKE: new role
  `authentik_sso`, `instance.yml` `authentik.sso`, Secret Manager `authentik-sso-idp-cert`; kind: a local OIDC
  stand-in in `./run.sh 09`). One Workspace group syncs to `agentdev-admins-sso`, a child of `agentdev-admins`: added
  when the directory lists the person, removed only when it no longer does, each change audited as
  `custom_agentdev_admin_sync`; members of `agentdev-admins` are never changed (#103). `forge_login` comes from a
  Workspace custom attribute that only directory admins can edit. The scope `forge` now also sends the `groups` claim
  with inherited groups. Only e-mail addresses of `authentik.sso.allowed_domains` (and a matching Google `hd`) sign
  in; the source has its own enrollment flow. New risks R69 to R74.
- Claude credential proxy (#66, security model phase 3, fixes R11): new image `agentdev-credproxy` and
  `manifests/claude-proxy.yaml.tmpl`, deployed by `./run.sh 06` and the playbook. Only the proxy mounts
  `claude-shared` / `claude-<agent>`; agent pods get `ANTHROPIC_BASE_URL` and a per-agent proxy key
  (`agent-<name>-proxy`). Per-agent token budget for 24 h (`CLAUDE_BUDGET_TOKENS` / `claude_budget_tokens`, default 0 =
  none; per agent in ConfigMap `claude-proxy-budgets`), shown in the UI column **Claude tokens (24 h)** through
  `GET /api/agents` (`budgetUsed`, `budgetLimit`). The proxy writes `AGENTDEV_USAGE` lines from the response
  headers; with the proxy on, the operator takes usage only from the proxy pods, the forge sidecar refuses
  `POST /usage` and the loop posts nothing (closes R47). The loop removes an old Claude login from the `claude` volume
  at start. On GKE an FQDNNetworkPolicy limits the proxy to `api.anthropic.com` when the cluster serves the type.
  New risks R60 to R65.
- Forge sidecar: forge drivers (#2, step 1). The API location, the per-request credential, the git credential and
  the REST allow-list are now one `driver` per `FORGE_KIND`; only `gitea` exists so far and behaves as before. An
  unknown `FORGE_KIND` stops the sidecar at start. Prepares the GitHub App driver.
- GKE: the Ansible role `api` deploys the management API (#104) when `instance.yml` sets `api.oidc_issuer` (new
  optional instance vars `api.{oidc_issuer, oidc_discovery_url, oidc_audience, forge_url, db_retention_days}`). Same
  steps as `./run.sh 10`: roles `agentdev_owner`/`agentdev_api`, `schema.sql`, retention and `grants.sql`; Secrets
  `agentdev-api-db` (password generated once in Secret Manager `platform-pg-agentdev-api`), `agentdev-api-forge`
  (from the optional Secret Manager `agentdev-api-forge-token`) and `agentdev-api-siem` (from `security.siem.*` and
  the optional `security-siem`, the keys of #71); then `manifests/api.yaml.tmpl`. All secret tasks have `no_log`.
  Secret lookup is one task file per source so the kind path can reuse the role (#106). The API restarts on every
  Secret change; an optional secret without an enabled version counts as absent. authentik on GKE: #114.
- Forge sidecar: GitHub App driver (#2, step 2), `FORGE_KIND=github`. Inputs `GITHUB_APP_ID`,
  `GITHUB_APP_PRIVATE_KEY`, optional `GITHUB_INSTALLATION_ID` and `FORGE_API_URL` (GHES), forge container only.
  The sidecar signs the App JWT (RS256, standard library), mints installation tokens narrowed to the agent's repos
  and to contents/issues/pull requests write and metadata/checks/statuses read, caches them in memory and renews
  them 10 minutes before expiry. git gets the token through the credential helper. Every comment, PR body and
  review reply gets a fixed signature line with the agent's name. `GET /identity` gives the loop the commit author
  (`<agent> (agentdev)` with the App bot's noreply address). Allowed calls add review threads (`.../replies`) and
  the check runs and status of a commit; merging, labels, issue edits and GraphQL are refused. Review of #128:
  `FORGE_API_URL` only `https://` on the forge's own API host; rate limits honoured (`Retry-After`, rate-limit reset,
  passed through to the loop, back-off on token minting); a 401 drops the cached token; review requests only for 1
  to 5 user logins (no teams); `per_page`/`page` 1 to 100; numbers in paths bounded; key preferably from
  `GITHUB_APP_PRIVATE_KEY_FILE` (never in an environment).
- Agent loop on GitHub (#2, step 3). The loop reads the forge kind and its commit author from the sidecar
  (`GET /identity`, validated). On GitHub: work is the open issues labelled `agent:<name>` (pull requests dropped),
  the agent's PRs are those of the App bot on `agent/<name>/*`, links use `/pull/`. Code scanning findings
  (`github-advanced-security[bot]` review threads) are answered in their thread with one outcome: **Fixed in
  `<sha>`** only when the pushed change touches the finding's file, else *Not fixed yet*; *Accepted risk* and *False
  positive* are marked as proposals. The model only proposes the outcome word for thread ids the loop asked about; the
  loop never resolves a thread. Failing check runs of the PR head are given to the model as feedback. Gitea unchanged. Review of
  #130: the label alone does not give work. Before each start the loop reads who added `agent:<name>` last
  (`issues/{n}/events`) and requires write, maintain or admin for that person, checked live
  (`collaborators/{user}/permission`, both new on the sidecar allow-list); otherwise it skips the issue and raises
  `label-not-writer` or `label-unverified`.
  loop never resolves a thread. Failing check runs of the PR head are given to the model as feedback. Gitea unchanged.
- GitHub routing and deployment (#2, step 4). The router routes GitHub events by the label `agent:<name>` and the
  branch `agent/<name>/...` (also on check runs), never by login (all agents share the App bot). The operator gives a
  `forge: github` agent `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY` and optional `GITHUB_INSTALLATION_ID` from its own
  Secret `agent-<name>` (forge container only, no `FORGE_TOKEN`; the key as a read-only file, `GITHUB_APP_PRIVATE_KEY_FILE`) and `FORGE_URL` from the new operator setting
  `GITHUB_URL` (default `https://github.com`). GKE: `instance.yml` gets `github_app.app_id` (and optional
  `installation_id`), the key comes from the Secret Manager secret `github-app-private-key`; the playbook checks the
  ids and agent names. kind: `FORGE_KIND=github` in `config.env`, `./run.sh 06` reads `.secrets/github-app/`.
  Docs: admin.md (connect agents, agent labels, reference), user guide, security model, risks R48 to R50.
  **Re-run:** GKE: the playbook (operator, agents); kind: `./run.sh 12` and `./run.sh 06` for GitHub agents. Gitea
  agents need no change.
- Process pack `default` (#110): `AGENTS.md` rules for security advisory threads (reply with Fixed / Accepted risk /
  False positive, never resolve a thread the agent did not fix), deployment targets in sync, no secrets printed and
  no model output in control paths; the PR template gets *Security findings* (thread, outcome, sha) and *Reviewed
  at* sections; new label `security-finding`. Applies to new bootstraps; existing repos keep their files.
- SIEM outputs (#71): syslog over TLS (RFC 5424 messages, RFC 5425 octet-counted framing, facility 13, structured
  data `agentdev@32473`) and OTLP logs (OTLP/HTTP JSON to `/v1/logs`, ECS-style attributes, optional headers). Both
  reuse the webhook's per-agent fair queues and retries and can run together with the webhook. TLS 1.2+, certificates
  always verified, optional CA and client certificate; no plain-text fallback (`http://` OTLP refused). Settings
  `SECURITY_SYSLOG_*`, `SECURITY_OTLP_*` in the Secret `agentdev-api-siem` (instance secrets only); on GCP the playbook fills it from
  `instance.yml security.siem.*` and the optional Secret Manager secret `security-siem`. The webhook URL must now be
  `https://` too. Syslog retries are capped (4 tries, 30 s) because one worker serves all agents.
- Per-agent hash chain for security events (#71): new fields `prev` and `chain` (`sha256(prev + "\n" + canonical
  event)`) in the table, the API, the webhook body, syslog and OTLP; computed at insert under a per-agent advisory
  lock. A SIEM detects gaps and edits (`docs/admin.md`, "Do a check of the hash chain"). Schema stays
  `agentdev.security/v1` (fields added); `schema.sql` adds the columns.
- GKE: the Ansible playbook deploys the platform namespace (Postgres + ContextForge, #51) from
  `manifests/platform.yaml.tmpl`, with passwords generated once into Secret Manager (`platform-<key>`), and runs
  `scripts/mcp-sync.sh` for each spec in the new instance var `mcp_projects`. `config.env` `KUBECTL` and the new
  `CF_ADMIN_FILE` of `mcp-sync.sh` can be overridden. `docs/admin.md` shows how to check that Dataplane V2 enforces
  the platform NetworkPolicies.
- MCP server allow-list (GHAS on #99): `scripts/mcp-sync.sh` registers only servers whose host is in
  `MCP_ALLOWED_HOSTS` (`config.env`, kind) or `mcp_allowed_hosts` (GKE `instance.yml`). **Breaking for specs with
  `mcp:`**: the list is empty by default, so add your trusted hosts. Risk entries R46 (MCP prompt injection) and R47
  (token leaks in logs) in `docs/security.md`; the secret and sync tasks of the platform role have `no_log`.
- Audit trail in Postgres (#91): every `AGENTDEV_AUDIT` line of the write API is also stored in the append-only
  `api_audit` table (same owner/API role split, trigger and `security_settings` retention as `security_events`; the
  API login has INSERT/SELECT and EXECUTE on `api_audit_prune()` only). The stdout line stays. New
  `GET /api/audit` (filters `agent`, `result`, `since`, `limit`, `cursor`), readable only by `agentdev-admins`.
- Subscription usage limits (#93, incident #92): after each Claude run the worker prints an `AGENTDEV_USAGE {json}`
  line (tokens, 5-hour and weekly rate-limit windows from the CLI's `rate_limit_event`; the worker now uses
  `--output-format stream-json`). The operator sums them per Claude credential Secret and, when a window reaches its
  threshold (defaults 80 % 5-hour, 90 % weekly), pauses **new** runs of every agent on that subscription until the
  window resets (running jobs finish; automatic resume, also without the operator). A run refused by Claude's rate
  limit pauses that agent at once and, through the operator, every agent on the same subscription (reset time from
  the CLI, else 1 h) and keeps the work for a retry instead of a
  "could not produce a change" comment. Each pause is a `SecurityEvent` (source `operator`, rule
  `usage-five-hour|usage-weekly|usage-rate-limited`, severity low; one per agent, the action names the
  subscription). Agents page: banner and a `usage-paused` state while a pause lasts. Default thresholds:
  `USAGE_FIVE_HOUR`/`USAGE_WEEKLY` (`config.env`, Ansible `usage.five_hour`/`usage.weekly`). Effective limit of an
  agent: the lowest over all its repositories (API and operator). State in ConfigMap `agentdev-usage` (mounted
  read-only into agent pods), thresholds in `agentdev-usage-settings`. API: `GET /api/usage`,
  `PUT /api/usage/global` (agentdev admins), `PUT /api/usage/projects/{owner}/{repo}` (write access; only lower than
  global, clamped again by the operator); every attempt audited as `set-limits`. Web UI: **Usage** page. RBAC: the
  operator gets get/update on `agentdev-usage` and get on `agentdev-usage-settings`; the API gets configmap create
  and get/update on `agentdev-usage-settings`, get on `agentdev-usage`.
  Security review of #102: usage reports go through the forge sidecar (`POST /usage`, schema/range check, rate limit)
  and the operator reads usage only from the `forge` container (a usage line from the agent container raises
  `usage-line-forged`); a report can only raise or extend a pause, never lift or shorten it, and one report pauses
  for at most 5 h; anomalies raise `usage-anomaly`; reset times never come from model text; values from JSON and files
  are checked as integers before bash arithmetic; `run_claude` fails closed on output that is not stream-json;
  `GET /api/usage` shows non-admins only the projects and agents they can write to (risks R46, R47).
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
- **BREAKING** (#66): agent pods no longer get the Claude credential. Re-run `./run.sh 06 12 10` on `kind`
  (06: proxy, signing key and agent keys; 12: operator `CLAUDE_PROXY_URL`; 10: API `CLAUDE_PROXY_STATUS_URL`), or the
  full `infra/ansible/deploy.yml` on GKE (the proxy tasks, the operator and the API). The playbook creates the Secret
  Manager entry `claude-proxy-signing` on its first run. Agent images must be updated too (the loop removes an old
  Claude login on start, and the forge sidecar gets `USAGE_REPORTS`). To roll back, set `CLAUDE_PROXY=false` /
  `claude_proxy: false` (deprecated, removed in the next minor release).
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
